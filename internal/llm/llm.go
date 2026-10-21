// Package llm is the single gateway from task handlers to Claude. It enforces
// the daily turn cap (SCH-7), records every call in the llm_call ledger, and
// turns rate limits and exhausted caps into deferrals rather than failures.
package llm

import (
	"context"
	"errors"
	"fmt"
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
		return res, &worker.DeferError{Until: c.now().Add(backoff), Reason: "rate limited: " + err.Error()}
	}
	return res, err
}
