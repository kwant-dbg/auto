// Package triage turns a well-evidenced problem into an opportunity
// (docs/05 §2): it judges the gates that existing evidence can settle,
// enforces evidence rules in code, applies the lifecycle transition and
// creates the critical questions that drive research.
package triage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kwant-dbg/auto/internal/claude"
	"github.com/kwant-dbg/auto/internal/founder"
	"github.com/kwant-dbg/auto/internal/llm"
	"github.com/kwant-dbg/auto/internal/queue"
	"github.com/kwant-dbg/auto/internal/store"
	"github.com/kwant-dbg/auto/prompts"
)

// TaskType is the queue task type.
const TaskType = "triage"

// PromptVersion is the triage prompt in use. v2 (week 3) restates the problem
// as what its observations share, not one member's specific gap.
const PromptVersion = "v2"

// Gate codes (docs/03 §2.1).
var (
	FounderGates = []string{"F1", "F2", "F3", "F4"}
	MarketGates  = []string{"M1", "M2", "M3", "M4", "M5", "M6"}
)

// Kill reason codes (docs/05 §2). out_of_scope was added in week 3: problems
// software or an AI-enabled service can't address (e.g. physical work).
var KillReasons = map[string]bool{
	"no_buyer": true, "no_spend": true, "unreachable": true, "incumbent_strong": true, "negative_margin": true,
	"infeasible": true, "founder_gate": true, "invalidated_by_experiment": true, "founder_veto": true, "out_of_scope": true,
	"duplicate": true, // merged into another opportunity's problem (week 3)
}

// Earned research budgets by stage (SCH-3).
const (
	BudgetCandidate = 1
	BudgetQualified = 5
)

// Inputs are the task inputs.
type Inputs struct {
	ProblemID int64 `json:"problem_id"`
}

// Outcome is what triage decided.
type Outcome struct {
	OpportunityID int64  `json:"opportunity_id"`
	State         string `json:"state"`
	Reason        string `json:"reason,omitempty"`
	Questions     int    `json:"questions"`
}

// Triager runs triage.
type Triager struct {
	Store   *store.Store
	LLM     *llm.Client
	Model   string
	Founder *founder.Profile
	// MaxObservations shown to the model, strongest first.
	MaxObservations int
}

// Handle is the worker handler.
func (t *Triager) Handle(ctx context.Context, task *queue.Task) (any, error) {
	var in Inputs
	if err := json.Unmarshal(task.Inputs, &in); err != nil {
		return nil, queue.Permanent(fmt.Errorf("inputs: %w", err))
	}
	return t.Run(ctx, &task.ID, in.ProblemID)
}

// Evidence is one observation shown to the model.
type Evidence struct {
	ID        int64
	Tier      int
	Category  string
	Source    string
	Statement string
	Quote     string
	URL       string
	Origin    string
}

type problemRow struct {
	ID        int64
	Statement string
	Segment   string
	Origins   int
	Count     int
}

func (t *Triager) load(ctx context.Context, problemID int64) (problemRow, []Evidence, error) {
	var p problemRow
	if err := t.Store.DB.QueryRow(ctx, `SELECT id, canonical_statement, segment, independent_origin_count, observation_count
		FROM problem WHERE id = $1`, problemID).Scan(&p.ID, &p.Statement, &p.Segment, &p.Origins, &p.Count); err != nil {
		return p, nil, fmt.Errorf("problem %d: %w", problemID, err)
	}
	limit := t.MaxObservations
	if limit <= 0 {
		limit = 30
	}
	// Strongest evidence first, one per origin before repeats, so a single
	// loud origin can't crowd out independent ones.
	rows, err := t.Store.DB.Query(ctx, `
		SELECT id, evidence_tier, category, source, statement, quote, url, origin_key FROM (
			SELECT o.id, o.evidence_tier, o.category, s.name AS source, o.statement, o.quote, r.canonical_url AS url, o.origin_key,
			       o.specificity, row_number() OVER (PARTITION BY o.origin_key ORDER BY o.evidence_tier, o.specificity DESC, o.id) AS rn
			FROM problem_observation po
			JOIN observation o ON o.id = po.observation_id
			JOIN raw_document r ON r.id = o.raw_document_id
			JOIN source s ON s.id = r.source_id
			WHERE po.problem_id = $1) x
		ORDER BY rn, evidence_tier, specificity DESC, id
		LIMIT $2`, problemID, limit)
	if err != nil {
		return p, nil, err
	}
	ev, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Evidence, error) {
		var e Evidence
		err := r.Scan(&e.ID, &e.Tier, &e.Category, &e.Source, &e.Statement, &e.Quote, &e.URL, &e.Origin)
		return e, err
	})
	return p, ev, err
}

// BuildPrompt renders the triage input.
func BuildPrompt(profile string, p problemRow, ev []Evidence) string {
	var b strings.Builder
	b.WriteString("## Founder profile\n")
	b.WriteString(profile)
	fmt.Fprintf(&b, "\n## Problem\nstatement: %s\nsegment: %s\nobservations: %d from %d independent origins (showing %d)\n\n## Observations\n",
		p.Statement, p.Segment, p.Count, p.Origins, len(ev))
	for _, e := range ev {
		q := e.Quote
		if len(q) > 400 {
			q = q[:400] + "…"
		}
		fmt.Fprintf(&b, "<observation id=\"%d\" tier=\"T%d\" category=\"%s\" source=\"%s\">\nstatement: %s\nquote: %s\nurl: %s\n</observation>\n",
			e.ID, e.Tier, e.Category, e.Source, e.Statement, q, e.URL)
	}
	return b.String()
}

type modelGate struct {
	Code        string  `json:"code"`
	Status      string  `json:"status"`
	Rationale   string  `json:"rationale"`
	EvidenceIDs []int64 `json:"evidence_ids"`
}

type modelQuestion struct {
	Text        string  `json:"text"`
	LinkedCode  string  `json:"linked_code"`
	Kind        string  `json:"kind"`
	PKill       float64 `json:"p_kill"`
	CostMinutes int     `json:"cost_minutes"`
}

// Result is the model's parsed output.
type Result struct {
	CanonicalStatement string `json:"canonical_statement"`
	Segment            string `json:"segment"`
	Addressable        struct {
		Status    string `json:"status"`
		Rationale string `json:"rationale"`
	} `json:"addressable"`
	Gates     []modelGate     `json:"gates"`
	Questions []modelQuestion `json:"questions"`
	Summary   string          `json:"summary"`
}

// Gate is a gate verdict after code-enforced rules.
type Gate struct {
	Code        string
	Status      string
	Rationale   string
	EvidenceIDs []int64
}

// EnforceGates applies the evidence rules the model can't be trusted with:
// cited ids must be the problem's own observations; M1 PASS needs a citation;
// M2 PASS needs a cited T1–T3 observation; F1 is skipped or left to the
// founder per the profile; M3–M6 start UNKNOWN (they need research).
func EnforceGates(model []modelGate, ev []Evidence, f1Required bool) map[string]Gate {
	tier := map[int64]int{}
	for _, e := range ev {
		tier[e.ID] = e.Tier
	}
	out := map[string]Gate{}
	for _, code := range append(append([]string{}, FounderGates...), MarketGates...) {
		out[code] = Gate{Code: code, Status: "UNKNOWN", Rationale: "needs research"}
	}
	if f1Required {
		out["F1"] = Gate{Code: "F1", Status: "UNKNOWN", Rationale: "the founder must confirm there is no employment conflict"}
	} else {
		out["F1"] = Gate{Code: "F1", Status: "SKIPPED", Rationale: "employment check disabled in the founder profile"}
	}
	for _, g := range model {
		if g.Code == "F1" || !strings.HasPrefix(g.Code, "F") && !strings.HasPrefix(g.Code, "M") {
			continue
		}
		if _, known := out[g.Code]; !known {
			continue
		}
		var ids []int64
		for _, id := range g.EvidenceIDs {
			if _, ok := tier[id]; ok {
				ids = append(ids, id)
			}
		}
		status, why := g.Status, g.Rationale
		switch {
		case status == "PASS" && g.Code == "M1" && len(ids) == 0:
			status, why = "UNKNOWN", why+" (downgraded: PASS cited no observation)"
		case status == "PASS" && g.Code == "M2":
			strong := false
			for _, id := range ids {
				if tier[id] <= 3 {
					strong = true
				}
			}
			if !strong {
				status, why = "UNKNOWN", why+" (downgraded: no T1–T3 observation cited)"
			}
		}
		if status != "PASS" && status != "FAIL" && status != "UNKNOWN" {
			status = "UNKNOWN"
		}
		out[g.Code] = Gate{Code: g.Code, Status: status, Rationale: why, EvidenceIDs: ids}
	}
	return out
}

// Decide maps the verdicts to a lifecycle state and reason.
func Decide(addressable string, gates map[string]Gate) (state, reason string) {
	if addressable == "no" {
		return "killed", "out_of_scope"
	}
	for _, code := range FounderGates {
		if gates[code].Status == "FAIL" {
			return "killed", "founder_gate"
		}
	}
	if gates["M1"].Status == "FAIL" {
		return "parked", "no_buyer"
	}
	if gates["M2"].Status == "FAIL" {
		return "parked", "no_spend"
	}
	if gates["M1"].Status == "PASS" && gates["M2"].Status == "PASS" {
		return "qualified", ""
	}
	return "candidate", ""
}

// killQuestions are the cheap disconfirming checks every qualified
// opportunity gets first, ordered by P(kill) ÷ cost (docs/05 §3).
func killQuestions(segment string) []modelQuestion {
	return []modelQuestion{
		{Text: fmt.Sprintf("Is there an incumbent with a strong rating that already serves %s for this job at a comparable price?", segment), LinkedCode: "M4", Kind: "kill", PKill: 0.4, CostMinutes: 10},
		{Text: fmt.Sprintf("Can we find 50 named organisations in %s with public contact paths?", segment), LinkedCode: "M3", Kind: "kill", PKill: 0.3, CostMinutes: 15},
		{Text: "Does running the core workflow on a sample input with current models cost less than 40% of a plausible price?", LinkedCode: "M5", Kind: "kill", PKill: 0.15, CostMinutes: 10},
		{Text: "Is there a critical dependency (a capability, an integration, API access) that doesn't exist or is prohibited by terms?", LinkedCode: "M6", Kind: "kill", PKill: 0.15, CostMinutes: 10},
	}
}

// Run triages one problem.
func (t *Triager) Run(ctx context.Context, taskID *int64, problemID int64) (Outcome, error) {
	p, ev, err := t.load(ctx, problemID)
	if err != nil {
		return Outcome{}, queue.Permanent(err)
	}
	if len(ev) == 0 {
		return Outcome{}, queue.Permanent(fmt.Errorf("problem %d has no observations", problemID))
	}
	pr, err := prompts.Load("triage", PromptVersion)
	if err != nil {
		return Outcome{}, queue.Permanent(err)
	}
	profile, f1 := "No founder profile configured.\n", false
	if t.Founder != nil {
		profile, f1 = t.Founder.Summary(), t.Founder.Founder.EmploymentConstraints.ConflictCheckRequired
	}
	res, err := t.LLM.Run(ctx, taskID, claude.Spec{
		TaskType: TaskType, PromptVersion: pr.ID(), Model: t.Model, SystemPrompt: pr.System,
		Prompt: BuildPrompt(profile, p, ev), Schema: pr.Schema, MaxTurns: 3, Timeout: 6 * time.Minute,
	})
	if err != nil {
		return Outcome{}, err
	}
	var r Result
	if err := json.Unmarshal(res.Structured, &r); err != nil {
		return Outcome{}, fmt.Errorf("%w: %v", claude.ErrInvalidOutput, err)
	}
	gates := EnforceGates(r.Gates, ev, f1)
	state, reason := Decide(r.Addressable.Status, gates)
	out, err := t.apply(ctx, p, r, gates, state, reason, pr.ID())
	return out, err
}

// apply writes the opportunity, gates, questions and decision in one
// transaction. Re-triage replaces open triage questions rather than adding.
func (t *Triager) apply(ctx context.Context, p problemRow, r Result, gates map[string]Gate, state, reason, promptID string) (Outcome, error) {
	out := Outcome{State: state, Reason: reason}
	segment := strings.TrimSpace(r.Segment)
	if segment == "" {
		segment = p.Segment
	}
	statement := strings.TrimSpace(r.CanonicalStatement)
	if statement == "" {
		statement = p.Statement
	}
	err := pgx.BeginFunc(ctx, t.Store.DB, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE problem SET canonical_statement = $2, segment = $3, last_triaged_at = now(),
			triaged_origin_count = independent_origin_count, updated_at = now() WHERE id = $1`, p.ID, statement, segment); err != nil {
			return err
		}
		var prevState *string
		budget := BudgetCandidate
		if state == "qualified" {
			budget = BudgetQualified
		}
		err := tx.QueryRow(ctx, `SELECT state FROM opportunity WHERE problem_id = $1`, p.ID).Scan(&prevState)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO opportunity (problem_id, segment, state, budget_units_total) VALUES ($1, $2, $3, $4)
			ON CONFLICT (problem_id) DO UPDATE SET segment = EXCLUDED.segment, state = EXCLUDED.state,
				stage_entered_at = CASE WHEN opportunity.state <> EXCLUDED.state THEN now() ELSE opportunity.stage_entered_at END,
				budget_units_total = greatest(opportunity.budget_units_total, EXCLUDED.budget_units_total)
			RETURNING id`, p.ID, segment, state, budget).Scan(&out.OpportunityID); err != nil {
			return err
		}
		for _, g := range gates {
			if _, err := tx.Exec(ctx, `
				INSERT INTO gate_status (opportunity_id, gate_code, status, evidence_ids, rationale) VALUES ($1, $2, $3, coalesce($4, '{}'::bigint[]), $5)
				ON CONFLICT (opportunity_id, gate_code) DO UPDATE SET status = EXCLUDED.status, evidence_ids = EXCLUDED.evidence_ids,
					rationale = EXCLUDED.rationale, updated_at = now()`,
				out.OpportunityID, g.Code, g.Status, g.EvidenceIDs, g.Rationale); err != nil {
				return err
			}
		}

		if _, err := tx.Exec(ctx, `DELETE FROM question WHERE opportunity_id = $1 AND origin = 'triage' AND status = 'open'`, out.OpportunityID); err != nil {
			return err
		}
		questions := r.Questions
		if state == "qualified" {
			questions = append(killQuestions(segment), questions...)
		}
		seen := map[string]bool{}
		if state != "killed" {
			for _, q := range questions {
				key := strings.ToLower(strings.TrimSpace(q.Text))
				if key == "" || seen[key] {
					continue
				}
				seen[key] = true
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
				if _, err := tx.Exec(ctx, `
					INSERT INTO question (opportunity_id, text, linked_code, kind, status, p_kill, voe_estimate, cost_estimate, origin)
					VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'triage')`,
					out.OpportunityID, strings.TrimSpace(q.Text), q.LinkedCode, kind, status, pk, pk/cost, cost); err != nil {
					return err
				}
				out.Questions++
			}
		}
		if gates["F1"].Status == "UNKNOWN" {
			if _, err := tx.Exec(ctx, `INSERT INTO question (opportunity_id, text, linked_code, kind, status, origin)
				VALUES ($1, 'Does this overlap your employer''s business or use knowledge gained there?', 'F1', 'kill', 'needs_human', 'triage')`,
				out.OpportunityID); err != nil {
				return err
			}
		}

		snapshot, _ := json.Marshal(map[string]any{"gates": gates, "addressable": r.Addressable, "summary": r.Summary})
		var wake any
		if state == "parked" {
			w, _ := json.Marshal([]map[string]any{{"type": "evidence_count", "problem_id": p.ID, "min_independent_origins": p.Origins + 3}})
			wake = w
		}
		var from any
		if prevState != nil {
			from = *prevState
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO decision (subject_type, subject_id, decision_type, from_state, to_state, actor, reason_code, notes,
				evidence_ids, factor_snapshot, wake_conditions)
			VALUES ('opportunity', $1, 'triage', $2, $3, 'system', NULLIF($4, ''), $5, coalesce($6, '{}'::bigint[]), $7, $8)`,
			out.OpportunityID, from, state, reason, r.Summary+" ["+promptID+"]", gateEvidence(gates), snapshot, wake)
		return err
	})
	return out, err
}

func gateEvidence(g map[string]Gate) []int64 {
	seen := map[int64]bool{}
	var ids []int64
	for _, gate := range g {
		for _, id := range gate.EvidenceIDs {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// Options bound triage scheduling.
type Options struct {
	MinIndependentOrigins int `yaml:"min_independent_origins"`
	MaxPerDay             int `yaml:"max_per_day"`
}

// Defaults fills unset values.
func (o *Options) Defaults() {
	if o.MinIndependentOrigins <= 0 {
		o.MinIndependentOrigins = 2
	}
	if o.MaxPerDay <= 0 {
		o.MaxPerDay = 40
	}
}

// Schedule enqueues triage for eligible problems, strongest evidence first,
// within the daily cap:
//   - never triaged, with at least MinIndependentOrigins independent origins;
//   - or re-triage: a candidate or parked opportunity whose origins at least
//     doubled (and grew by 2+) since the last triage. That is how parked
//     opportunities' evidence wake conditions fire.
func Schedule(ctx context.Context, st *store.Store, opt Options) (int, error) {
	opt.Defaults()
	var today int
	if err := st.DB.QueryRow(ctx, `SELECT count(*) FROM task WHERE type = $1 AND created_at >= date_trunc('day', now())`, TaskType).Scan(&today); err != nil {
		return 0, err
	}
	room := opt.MaxPerDay - today
	if room <= 0 {
		return 0, nil
	}
	rows, err := st.DB.Query(ctx, `
		SELECT p.id, p.independent_origin_count FROM problem p
		LEFT JOIN opportunity o ON o.problem_id = p.id
		WHERE p.status = 'active' AND p.independent_origin_count >= $1
		  AND (p.last_triaged_at IS NULL
		       OR (o.state IN ('candidate', 'parked')
		           AND p.independent_origin_count >= greatest(2 * p.triaged_origin_count, p.triaged_origin_count + 2)))
		ORDER BY p.t1_origin_count DESC, p.independent_origin_count DESC, p.best_tier, p.id
		LIMIT $2`, opt.MinIndependentOrigins, room)
	if err != nil {
		return 0, err
	}
	type cand struct{ id, origins int64 }
	cands, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (cand, error) {
		var c cand
		err := r.Scan(&c.id, &c.origins)
		return c, err
	})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, c := range cands {
		_, created, err := queue.Enqueue(ctx, st.DB, queue.NewTask{
			Type:      TaskType,
			DedupeKey: fmt.Sprintf("triage:%s:%d:%d", PromptVersion, c.id, c.origins),
			Priority:  1,
			Inputs:    Inputs{ProblemID: c.id},
		})
		if err != nil {
			return n, err
		}
		if created {
			n++
		}
	}
	return n, nil
}
