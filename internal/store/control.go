package store

import (
	"context"
	"encoding/json"
	"time"
)

// Paused reports whether the kill switch is on (SCH-8).
func (s *Store) Paused(ctx context.Context) (bool, error) {
	var raw []byte
	if err := s.DB.QueryRow(ctx, `SELECT value FROM control WHERE key = 'paused'`).Scan(&raw); err != nil {
		return false, err
	}
	var paused bool
	err := json.Unmarshal(raw, &paused)
	return paused, err
}

// SetPaused turns the kill switch on or off.
func (s *Store) SetPaused(ctx context.Context, paused bool) error {
	_, err := s.DB.Exec(ctx, `
		INSERT INTO control (key, value, updated_at) VALUES ('paused', to_jsonb($1::boolean), now())
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, paused)
	return err
}

// LLMCall is one model invocation in the ledger.
type LLMCall struct {
	TaskID            *int64
	TaskType          string
	Model             string
	PromptVersion     string
	ClaudeCodeVersion string
	SessionID         string
	NumTurns          int
	EstCostUSD        float64
	Usage             json.RawMessage
	Latency           time.Duration
	ExitCode          int
	Status            string // ok | error | timeout | invalid_output
	Error             string
}

// RecordLLMCall appends to the llm_call ledger.
func (s *Store) RecordLLMCall(ctx context.Context, c LLMCall) error {
	var usage any
	if len(c.Usage) > 0 {
		usage = c.Usage
	}
	_, err := s.DB.Exec(ctx, `
		INSERT INTO llm_call (task_id, task_type, model, prompt_version, claude_code_version, session_id,
			num_turns, est_cost_usd, usage, latency_ms, exit_code, status, error)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), $7, $8, $9, $10, $11, $12, NULLIF($13, ''))`,
		c.TaskID, c.TaskType, c.Model, c.PromptVersion, c.ClaudeCodeVersion, c.SessionID,
		c.NumTurns, c.EstCostUSD, usage, c.Latency.Milliseconds(), c.ExitCode, c.Status, c.Error)
	return err
}

// TurnsSince sums agent turns recorded since t (capacity caps, SCH-7).
func (s *Store) TurnsSince(ctx context.Context, t time.Time) (int, error) {
	var n int
	err := s.DB.QueryRow(ctx, `SELECT coalesce(sum(num_turns), 0) FROM llm_call WHERE created_at >= $1`, t).Scan(&n)
	return n, err
}

// RecordMetric upserts today's value of a metric for a subject.
func (s *Store) RecordMetric(ctx context.Context, sourceID int64, subject, name string, value float64) error {
	_, err := s.DB.Exec(ctx, `
		INSERT INTO metric_observation (source_id, subject, name, value) VALUES ($1, $2, $3, $4)
		ON CONFLICT (source_id, subject, name, observed_on) DO UPDATE SET value = EXCLUDED.value, observed_at = now()`,
		sourceID, subject, name, value)
	return err
}
