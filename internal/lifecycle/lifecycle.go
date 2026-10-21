// Package lifecycle holds the opportunity state machine (docs/05 §2) shared
// by triage, research and assessment: which failed gate kills or parks an
// opportunity, when it advances, and the bookkeeping every transition does.
package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Earned research budgets by stage (SCH-3).
var Budget = map[string]int{"candidate": 1, "qualified": 5, "dossier": 30}

// failOutcome maps a FAILed gate to the state and reason code it causes.
// Founder gates kill; a missing buyer or spend parks (new evidence can revive
// it); the research-settled market gates kill.
var failOutcome = map[string][2]string{
	"F1": {"killed", "founder_gate"}, "F2": {"killed", "founder_gate"}, "F3": {"killed", "founder_gate"}, "F4": {"killed", "founder_gate"},
	"M1": {"parked", "no_buyer"}, "M2": {"parked", "no_spend"},
	"M3": {"killed", "unreachable"}, "M4": {"killed", "incumbent_strong"},
	"M5": {"killed", "negative_margin"}, "M6": {"killed", "infeasible"},
}

// gateOrder decides which failure is reported when several gates fail.
var gateOrder = []string{"F1", "F2", "F3", "F4", "M4", "M3", "M5", "M6", "M1", "M2"}

// Next returns the state an opportunity should be in given its gates and how
// many kill questions are unresolved (open, unanswerable by desk research, or
// waiting on the founder), and the reason code if it leaves the pipeline. An
// unresolved kill question holds the opportunity where it is: an unanswered
// kill check is not a passed one. Parked and killed opportunities only move
// by human decision or a wake condition.
func Next(state string, gates map[string]string, unresolvedKillQuestions int) (next, reason string) {
	switch state {
	case "killed", "parked", "validating", "building", "live":
		return state, ""
	}
	for _, code := range gateOrder {
		if gates[code] == "FAIL" {
			o := failOutcome[code]
			return o[0], o[1]
		}
	}
	switch state {
	case "candidate":
		if gates["M1"] == "PASS" && gates["M2"] == "PASS" {
			return "qualified", ""
		}
	case "qualified":
		if unresolvedKillQuestions == 0 {
			return "dossier", ""
		}
	}
	return state, ""
}

// Question is a question to create.
type Question struct {
	Text        string
	LinkedCode  string
	Kind        string
	PKill       float64
	CostMinutes int
}

// KillQuestions are the cheap disconfirming checks every qualified
// opportunity gets first, ordered by P(kill) ÷ cost (docs/05 §3).
func KillQuestions(segment string) []Question {
	return []Question{
		{Text: fmt.Sprintf("Is there an incumbent with a strong rating that already serves %s for this job at a comparable price?", segment), LinkedCode: "M4", Kind: "kill", PKill: 0.4, CostMinutes: 10},
		{Text: fmt.Sprintf("Can we find 50 named organisations in %s with public contact paths?", segment), LinkedCode: "M3", Kind: "kill", PKill: 0.3, CostMinutes: 15},
		{Text: "Does running the core workflow on a sample input with current models cost less than 40% of a plausible price?", LinkedCode: "M5", Kind: "kill", PKill: 0.15, CostMinutes: 10},
		{Text: "Is there a critical dependency (a capability, an integration, API access) that doesn't exist or is prohibited by terms?", LinkedCode: "M6", Kind: "kill", PKill: 0.15, CostMinutes: 10},
	}
}

// InsertQuestion adds a question unless an open one with the same text exists.
func InsertQuestion(ctx context.Context, tx pgx.Tx, oppID int64, q Question, origin string, parent *int64, depth int) (bool, error) {
	kind := q.Kind
	if kind != "kill" {
		kind = "standard"
	}
	cost := float64(max(q.CostMinutes, 1))
	pk := min(max(q.PKill, 0), 1)
	status := "open"
	if q.LinkedCode == "F1" {
		status = "needs_human"
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO question (opportunity_id, text, linked_code, kind, status, p_kill, voe_estimate, cost_estimate, origin, parent_question_id, depth)
		SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11
		WHERE NOT EXISTS (SELECT 1 FROM question WHERE opportunity_id = $1 AND lower(text) = lower($2) AND status = 'open')`,
		oppID, q.Text, q.LinkedCode, kind, status, pk, pk/cost, cost, origin, parent, depth)
	return tag.RowsAffected() > 0, err
}

// Gates reads an opportunity's gate verdicts.
func Gates(ctx context.Context, tx pgx.Tx, oppID int64) (map[string]string, error) {
	rows, err := tx.Query(ctx, `SELECT gate_code, status FROM gate_status WHERE opportunity_id = $1`, oppID)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for rows.Next() {
		var c, s string
		if err := rows.Scan(&c, &s); err != nil {
			return nil, err
		}
		out[c] = s
	}
	return out, rows.Err()
}

// Advance applies Next and records the transition. It returns the new state.
func Advance(ctx context.Context, tx pgx.Tx, oppID int64, actor, decisionType, notes string) (string, error) {
	var state, segment string
	if err := tx.QueryRow(ctx, `SELECT state, segment FROM opportunity WHERE id = $1 FOR UPDATE`, oppID).Scan(&state, &segment); err != nil {
		return "", err
	}
	gates, err := Gates(ctx, tx, oppID)
	if err != nil {
		return "", err
	}
	var unresolved int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM question WHERE opportunity_id = $1 AND kind = 'kill' AND status <> 'answered'`, oppID).Scan(&unresolved); err != nil {
		return "", err
	}
	next, reason := Next(state, gates, unresolved)
	if next == state {
		return state, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE opportunity SET state = $2, stage_entered_at = now(),
		budget_units_total = greatest(budget_units_total, $3), no_change_streak = 0 WHERE id = $1`, oppID, next, Budget[next]); err != nil {
		return "", err
	}
	if next == "qualified" {
		for _, q := range KillQuestions(segment) {
			if _, err := InsertQuestion(ctx, tx, oppID, q, "triage", nil, 0); err != nil {
				return "", err
			}
		}
	}
	snapshot, _ := json.Marshal(map[string]any{"gates": gates})
	if _, err := tx.Exec(ctx, `INSERT INTO decision (subject_type, subject_id, decision_type, from_state, to_state, actor, reason_code, notes, factor_snapshot)
		VALUES ('opportunity', $1, $2, $3, $4, $5, NULLIF($6, ''), $7, $8)`,
		oppID, decisionType, state, next, actor, reason, notes, snapshot); err != nil {
		return "", err
	}
	return next, nil
}
