// Package baseline runs B0, the one-shot deep-research baseline AOE must
// beat in blind comparison (docs/07 §4, G2): a single web-search session with
// the same founder profile, asked for cards in AOE's template. Its quotes
// are not snapshotted or span-checked; that difference is part of what the
// comparison measures.
package baseline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kwant-dbg/auto/internal/claude"
	"github.com/kwant-dbg/auto/internal/llm"
	"github.com/kwant-dbg/auto/internal/queue"
	"github.com/kwant-dbg/auto/internal/report"
	"github.com/kwant-dbg/auto/internal/store"
	"github.com/kwant-dbg/auto/prompts"
)

// TaskType is the queue task type.
const TaskType = "baseline_b0"

// PromptVersion is the B0 prompt version.
const PromptVersion = "v1"

// Inputs are the task inputs.
type Inputs struct {
	Week string `json:"week"`
}

// Runner runs B0.
type Runner struct {
	Store    *store.Store
	LLM      *llm.Client
	Model    string
	Profile  string // founder profile summary
	Cards    int    // cards per run
	MaxTurns int
	// SkipF1 marks F1 SKIPPED on B0's cards when the founder disabled the
	// employment check, as triage does on AOE's, so the gate line can't
	// tell the two apart in a blind pair.
	SkipF1 bool
}

// Handle is the worker handler.
func (r *Runner) Handle(ctx context.Context, t *queue.Task) (any, error) {
	var in Inputs
	if err := json.Unmarshal(t.Inputs, &in); err != nil || in.Week == "" {
		return nil, queue.Permanent(fmt.Errorf("inputs: %v", err))
	}
	return r.Run(ctx, &t.ID, in.Week)
}

// Outcome summarises a run.
type Outcome struct {
	RunID int64 `json:"run_id"`
	Cards int   `json:"cards"`
}

type modelCard struct {
	Segment  string            `json:"segment"`
	Job      string            `json:"job"`
	Problem  string            `json:"problem"`
	Evidence []report.Evidence `json:"evidence"`
	Counter  []report.Evidence `json:"counter_evidence"`
	Gates    map[string]string `json:"gates"`
	Prospect int               `json:"prospects"`
	Paths    []report.Path     `json:"paths"`
	MRR      report.MRR        `json:"mrr"`
	Weakest  string            `json:"weakest_link"`
	Next     string            `json:"next_experiment"`
	Hours    float64           `json:"founder_hours"`
}

// ToCard maps B0's output onto the shared card. Evidence items with a claim
// are shown like AOE's research findings, so the template reads the same.
func (m modelCard) ToCard() report.Card {
	c := report.Card{
		Segment: m.Segment, Job: m.Job, Problem: m.Problem, Gates: m.Gates, Prospects: m.Prospect,
		Paths: m.Paths, Weakest: m.Weakest, NextExperiment: m.Next, FounderHours: m.Hours,
	}
	mrr := m.MRR
	c.MRR = &mrr
	hosts := map[string]bool{}
	for _, e := range m.Evidence {
		e.Snapshot = nil
		if e.Tier >= 7 {
			continue // inference is not evidence
		}
		hosts[hostOf(e.URL)] = true
		claim := e.Claim
		e.Claim = ""
		if len(c.Evidence) < 3 {
			c.Evidence = append(c.Evidence, e)
			continue
		}
		e.Claim = claim
		c.Findings = append(c.Findings, e)
	}
	c.Origins = len(hosts)
	for _, e := range m.Counter {
		e.Snapshot = nil
		c.Counter = append(c.Counter, e)
	}
	return c
}

// Run asks for this week's cards and stores them. A week is run once.
func (r *Runner) Run(ctx context.Context, taskID *int64, week string) (Outcome, error) {
	var out Outcome
	var existing int64
	err := r.Store.DB.QueryRow(ctx, `SELECT id FROM baseline_run WHERE week = $1 AND prompt_version = $2`, week, PromptVersion).Scan(&existing)
	if err == nil {
		out.RunID = existing
		return out, r.Store.DB.QueryRow(ctx, `SELECT count(*) FROM baseline_card WHERE run_id = $1`, existing).Scan(&out.Cards)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	p, err := prompts.Load("baseline_b0", PromptVersion)
	if err != nil {
		return out, queue.Permanent(err)
	}
	prev, err := r.previous(ctx)
	if err != nil {
		return out, err
	}
	res, err := r.LLM.Run(ctx, taskID, claude.Spec{
		TaskType: TaskType, PromptVersion: p.ID(), Model: r.Model, SystemPrompt: p.System,
		Prompt: BuildPrompt(r.Profile, max(r.Cards, 1), prev), Schema: p.Schema,
		Tools: []string{"WebSearch"}, AllowedTools: []string{"WebSearch"},
		MaxTurns: max(r.MaxTurns, 10), Timeout: 30 * time.Minute,
	})
	if err != nil {
		return out, err
	}
	var m struct {
		Cards []modelCard `json:"cards"`
	}
	if err := json.Unmarshal(res.Structured, &m); err != nil || len(m.Cards) == 0 {
		return out, fmt.Errorf("%w: %v", claude.ErrInvalidOutput, err)
	}
	err = pgx.BeginFunc(ctx, r.Store.DB, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO baseline_run (week, task_id, model, prompt_version, num_turns) VALUES ($1, $2, $3, $4, $5) RETURNING id`,
			week, taskID, r.Model, PromptVersion, res.NumTurns).Scan(&out.RunID); err != nil {
			return err
		}
		for _, mc := range m.Cards {
			card := mc.ToCard()
			if r.SkipF1 && card.Gates != nil {
				card.Gates["F1"] = "SKIPPED"
			}
			b, err := json.Marshal(card)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO baseline_card (run_id, card) VALUES ($1, $2)`, out.RunID, b); err != nil {
				return err
			}
			out.Cards++
		}
		return nil
	})
	return out, err
}

// previous lists earlier B0 ideas so runs don't repeat themselves (AOE
// doesn't resurface its own cards either).
func (r *Runner) previous(ctx context.Context) ([]string, error) {
	rows, err := r.Store.DB.Query(ctx, `SELECT card->>'segment' || ' — ' || (card->>'job') FROM baseline_card ORDER BY id DESC LIMIT 30`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// BuildPrompt renders the request.
func BuildPrompt(profile string, cards int, previous []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Find the %d best software opportunities for this founder and report them as cards.\n\n## Founder profile\n\n%s\n", cards, strings.TrimSpace(profile))
	if len(previous) > 0 {
		b.WriteString("\n## Already suggested in earlier runs (choose different ones)\n\n")
		for _, p := range previous {
			fmt.Fprintf(&b, "- %s\n", p)
		}
	}
	return b.String()
}

// Schedule enqueues this week's run once.
func Schedule(ctx context.Context, st *store.Store, now time.Time) (bool, error) {
	week := report.ISOWeek(now)
	_, created, err := queue.Enqueue(ctx, st.DB, queue.NewTask{
		Type: TaskType, DedupeKey: fmt.Sprintf("baseline:%s:%s", PromptVersion, week), Priority: 1, Inputs: Inputs{Week: week},
	})
	return created, err
}

func hostOf(raw string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(raw, "https://"), "http://")
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimPrefix(strings.ToLower(s), "www.")
}

// Pending reports whether the week's run is still on its way: not stored
// yet, and its task is queued or running.
func Pending(ctx context.Context, st *store.Store, week string) (bool, error) {
	var pending bool
	err := st.DB.QueryRow(ctx, `SELECT NOT EXISTS (SELECT 1 FROM baseline_run WHERE week = $1)
		AND EXISTS (SELECT 1 FROM task WHERE type = $2 AND dedupe_key = $3 AND state IN ('queued', 'running'))`,
		week, TaskType, fmt.Sprintf("baseline:%s:%s", PromptVersion, week)).Scan(&pending)
	return pending, err
}
