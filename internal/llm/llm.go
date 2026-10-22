// Package llm is the single gateway from task handlers to Claude. It enforces
// the daily turn cap (SCH-7), records every call in the llm_call ledger, and
// turns rate limits and exhausted caps into deferrals rather than failures.
package llm

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kwant-dbg/auto/internal/claude"
	"github.com/kwant-dbg/auto/internal/store"
	"github.com/kwant-dbg/auto/internal/worker"
)

// Runner is the subset of *claude.Runner used here (fakeable in tests).
type Runner interface {
	Run(ctx context.Context, s claude.Spec) (claude.Result, error)
}

// Client runs specs under the capacity caps.
type Client struct {
	Runner            Runner
	Store             *store.Store
	MaxTurnsPerDay    int
	ClaudeCodeVersion string
	// RateLimitBackoff is how long to defer after a rate limit (default 15m).
	RateLimitBackoff time.Duration
	Now              func() time.Time
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Run executes a spec for a task. It returns a *worker.DeferError when the
// daily cap is reached or the plan is rate limited.
func (c *Client) Run(ctx context.Context, taskID *int64, s claude.Spec) (claude.Result, error) {
	now := c.now().UTC()
	dayStart := now.Truncate(24 * time.Hour)
	if c.MaxTurnsPerDay > 0 {
		used, err := c.Store.TurnsSince(ctx, dayStart)
		if err != nil {
			return claude.Result{}, err
		}
		if used >= c.MaxTurnsPerDay {
			return claude.Result{}, &worker.DeferError{
				Until:  dayStart.Add(24 * time.Hour),
				Reason: fmt.Sprintf("daily turn cap reached (%d/%d)", used, c.MaxTurnsPerDay),
			}
		}
	}

	res, err := c.Runner.Run(ctx, s)
	rec := store.LLMCall{
		TaskID: taskID, TaskType: s.TaskType, Model: s.Model, PromptVersion: s.PromptVersion,
		ClaudeCodeVersion: c.ClaudeCodeVersion, SessionID: res.SessionID, NumTurns: res.NumTurns,
		EstCostUSD: res.CostUSD, Usage: res.Usage, Latency: res.Duration, ExitCode: res.ExitCode,
		Status: res.Status(err),
	}
	if err != nil {
		rec.Error = err.Error()
	}
	if recErr := c.Store.RecordLLMCall(context.WithoutCancel(ctx), rec); recErr != nil {
		return res, errors.Join(err, fmt.Errorf("record llm call: %w", recErr))
	}

	if errors.Is(err, claude.ErrRateLimited) {
		backoff := c.RateLimitBackoff
		if backoff <= 0 {
			backoff = 15 * time.Minute
		}
		until := c.now().Add(backoff)
		if reset, ok := ResetTime(err.Error(), c.now()); ok && reset.After(until) {
			until = reset
		}
		return res, &worker.DeferError{Until: until, Reason: "rate limited: " + err.Error()}
	}
	return res, err
}

var resetRe = regexp.MustCompile(`(?i)resets\s+(\d{1,2})(?::(\d{2}))?\s*(am|pm)\s*\(utc\)`)

// ResetTime reads the reset time from a limit message ("resets 10:30pm
// (UTC)") as its next occurrence after now, plus a minute's margin.
func ResetTime(msg string, now time.Time) (time.Time, bool) {
	m := resetRe.FindStringSubmatch(msg)
	if m == nil {
		return time.Time{}, false
	}
	h, _ := strconv.Atoi(m[1])
	mins := 0
	if m[2] != "" {
		mins, _ = strconv.Atoi(m[2])
	}
	if h < 1 || h > 12 || mins > 59 {
		return time.Time{}, false
	}
	h %= 12
	if strings.EqualFold(m[3], "pm") {
		h += 12
	}
	now = now.UTC()
	t := time.Date(now.Year(), now.Month(), now.Day(), h, mins, 0, 0, time.UTC)
	if !t.After(now) {
		t = t.Add(24 * time.Hour)
	}
	return t.Add(time.Minute), true
}
