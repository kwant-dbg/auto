// Package lifecycle holds the opportunity state machine (docs/05 §2) shared
// by triage, research and assessment: which failed gate kills or parks an
// opportunity, when it advances, and the bookkeeping every transition does.
package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Earned research budgets by stage (SCH-3).
var Budget = map[string]int{"candidate": 1, "qualified": 8, "dossier": 30}

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
	t := Transition{From: state, To: next, Actor: actor, Type: decisionType, Reason: reason, Notes: notes}
	if next == "parked" {
		// A missing buyer or spend may show up later: wake on fresh evidence,
		// as triage does.
		var problemID int64
		var origins int
		if err := tx.QueryRow(ctx, `SELECT p.id, p.independent_origin_count FROM opportunity o JOIN problem p ON p.id = o.problem_id
			WHERE o.id = $1`, oppID).Scan(&problemID, &origins); err != nil {
			return "", err
		}
		t.Wake = []Wake{{Type: "evidence_count", ProblemID: problemID, MinIndependentOrigins: origins + 3}}
	}
	return next, Move(ctx, tx, oppID, t)
}

// Transition is one state change to record.
type Transition struct {
	From, To string
	Actor    string // system | human
	Type     string // decision type, e.g. research, human, wake
	Reason   string // reason code, if any
	Notes    string
	Wake     []Wake // required when parking
}

// Move applies a transition and its bookkeeping: the stage budget, kill
// questions on qualifying, a fresh research unit when an opportunity comes
// back as a candidate, and the decision row with a gate snapshot.
func Move(ctx context.Context, tx pgx.Tx, oppID int64, t Transition) error {
	if t.To == "parked" && len(t.Wake) == 0 {
		return fmt.Errorf("opportunity %d: parking needs at least one wake condition", oppID)
	}
	var segment string
	if err := tx.QueryRow(ctx, `SELECT segment FROM opportunity WHERE id = $1`, oppID).Scan(&segment); err != nil {
		return err
	}
	revived := t.To == "candidate" && (t.From == "parked" || t.From == "killed")
	if _, err := tx.Exec(ctx, `UPDATE opportunity SET state = $2, stage_entered_at = now(), no_change_streak = 0,
		saturated = CASE WHEN $4 THEN false ELSE saturated END,
		budget_units_total = CASE WHEN $4 THEN budget_units_used + $3 ELSE greatest(budget_units_total, $3) END
		WHERE id = $1`, oppID, t.To, Budget[t.To], revived); err != nil {
		return err
	}
	if t.To == "qualified" {
		for _, q := range KillQuestions(segment) {
			if _, err := InsertQuestion(ctx, tx, oppID, q, "triage", nil, 0); err != nil {
				return err
			}
		}
	}
	gates, err := Gates(ctx, tx, oppID)
	if err != nil {
		return err
	}
	snapshot, _ := json.Marshal(map[string]any{"gates": gates})
	var wake any
	if len(t.Wake) > 0 {
		w, _ := json.Marshal(t.Wake)
		wake = w
	}
	_, err = tx.Exec(ctx, `INSERT INTO decision (subject_type, subject_id, decision_type, from_state, to_state, actor, reason_code, notes, factor_snapshot, wake_conditions)
		VALUES ('opportunity', $1, $2, $3, $4, $5, NULLIF($6, ''), $7, $8, $9)`,
		oppID, t.Type, t.From, t.To, t.Actor, t.Reason, t.Notes, snapshot, wake)
	return err
}

// KillReasons is the fixed kill taxonomy (docs/05 §2). "duplicate" is
// reserved for consolidation.
var KillReasons = []string{"no_buyer", "no_spend", "unreachable", "incumbent_strong", "negative_margin", "infeasible",
	"founder_gate", "invalidated_by_experiment", "founder_veto", "out_of_scope"}

// Wake is a machine-checkable wake condition on a parked opportunity
// (docs/05 §2). date and evidence_count are checked by WakeDue; the other
// types are recorded for the phases that will watch them.
type Wake struct {
	Type                  string `json:"type"` // date | evidence_count | competitor_event | capability | regulation
	Date                  string `json:"date,omitempty"`
	ProblemID             int64  `json:"problem_id,omitempty"`
	MinIndependentOrigins int    `json:"min_independent_origins,omitempty"`
	Text                  string `json:"text,omitempty"`
}

// Checkable reports whether WakeDue can fire the condition.
func (w Wake) Checkable() bool {
	return (w.Type == "date" && w.Date != "") || (w.Type == "evidence_count" && w.ProblemID > 0 && w.MinIndependentOrigins > 0)
}

// WakeDue returns parked opportunities whose latest parking decision has a
// condition that fired, to the candidate stage with a fresh research unit.
func WakeDue(ctx context.Context, db interface {
	Begin(context.Context) (pgx.Tx, error)
}, now time.Time) (int, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	rows, err := tx.Query(ctx, `
		SELECT o.id, d.wake_conditions FROM opportunity o
		JOIN LATERAL (SELECT wake_conditions FROM decision WHERE subject_type = 'opportunity' AND subject_id = o.id
		              AND to_state = 'parked' ORDER BY id DESC LIMIT 1) d ON true
		WHERE o.state = 'parked' AND d.wake_conditions IS NOT NULL
		FOR UPDATE OF o SKIP LOCKED`)
	if err != nil {
		return 0, err
	}
	type parked struct {
		id    int64
		conds []Wake
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (parked, error) {
		var p parked
		var raw []byte
		if err := r.Scan(&p.id, &raw); err != nil {
			return p, err
		}
		_ = json.Unmarshal(raw, &p.conds)
		return p, nil
	})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, p := range list {
		fired, err := firedCondition(ctx, tx, p.conds, now)
		if err != nil {
			return n, err
		}
		if fired == "" {
			continue
		}
		if err := Move(ctx, tx, p.id, Transition{From: "parked", To: "candidate", Actor: "system", Type: "wake", Notes: "wake condition fired: " + fired}); err != nil {
			return n, err
		}
		n++
	}
	return n, tx.Commit(ctx)
}

func firedCondition(ctx context.Context, tx pgx.Tx, conds []Wake, now time.Time) (string, error) {
	for _, w := range conds {
		switch w.Type {
		case "date":
			d, err := time.Parse("2006-01-02", w.Date)
			if err == nil && !now.Before(d) {
				return "date " + w.Date, nil
			}
		case "evidence_count":
			var origins int
			err := tx.QueryRow(ctx, `SELECT independent_origin_count FROM problem WHERE id = $1`, w.ProblemID).Scan(&origins)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return "", err
			}
			if err == nil && w.MinIndependentOrigins > 0 && origins >= w.MinIndependentOrigins {
				return fmt.Sprintf("problem %d reached %d independent origins (needed %d)", w.ProblemID, origins, w.MinIndependentOrigins), nil
			}
		}
	}
	return "", nil
}
