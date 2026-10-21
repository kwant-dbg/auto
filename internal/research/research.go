// Package research answers an opportunity's open questions with a tool-using
// agent (RES-1, RES-2), verifies its findings (RES-3), and applies them:
// question answers, gate verdicts backed by supported findings, bounded
// follow-up questions (SCH-4), lifecycle transitions and saturation (SCH-5).
package research

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kwant-dbg/auto/internal/claude"
	"github.com/kwant-dbg/auto/internal/lifecycle"
	"github.com/kwant-dbg/auto/internal/llm"
	"github.com/kwant-dbg/auto/internal/queue"
	"github.com/kwant-dbg/auto/internal/store"
	"github.com/kwant-dbg/auto/internal/verify"
	"github.com/kwant-dbg/auto/prompts"
)

// TaskType is the queue task type.
const TaskType = "research"

// PromptVersion is the research prompt in use.
const PromptVersion = "v1"

// Limits on autonomous question creation (SCH-4).
const (
	MaxFollowUps = 3
	MaxDepth     = 2
	// SaturationStreak research rounds without a material change mark an
	// opportunity's desk research saturated (SCH-5).
	SaturationStreak = 2
)

// Inputs are the task inputs.
type Inputs struct {
	QuestionID int64 `json:"question_id"`
}

// Outcome summarises a research task.
type Outcome struct {
	QuestionID    int64  `json:"question_id"`
	Status        string `json:"status"`
	Findings      int    `json:"findings"`
	Supported     int    `json:"supported"`
	Rejected      int    `json:"rejected"`
	GateChanged   string `json:"gate_changed,omitempty"`
	State         string `json:"state"`
	FollowUps     int    `json:"follow_ups"`
	Saturated     bool   `json:"saturated"`
	SkippedReason string `json:"skipped,omitempty"`
}

// Researcher runs research tasks.
type Researcher struct {
	Store    *store.Store
	LLM      *llm.Client
	Verifier *verify.Verifier
	Model    string
	MaxTurns int
}

// Handle is the worker handler.
func (r *Researcher) Handle(ctx context.Context, t *queue.Task) (any, error) {
	var in Inputs
	if err := json.Unmarshal(t.Inputs, &in); err != nil {
		return nil, queue.Permanent(fmt.Errorf("inputs: %w", err))
	}
	return r.Run(ctx, &t.ID, in.QuestionID)
}

type qctx struct {
	QuestionID int64
	Text       string
	LinkedCode string
	Kind       string
	Depth      int
	OppID      int64
	State      string
	Segment    string
	Statement  string
	Gates      map[string]string
	Rationale  map[string]string
	Evidence   []string
	Answered   []string
}

func (r *Researcher) load(ctx context.Context, qid int64) (*qctx, error) {
	q := &qctx{QuestionID: qid, Rationale: map[string]string{}}
	var status string
	err := r.Store.DB.QueryRow(ctx, `
		SELECT q.text, q.linked_code, q.kind, q.depth, q.status, o.id, o.state, o.segment, p.canonical_statement
		FROM question q JOIN opportunity o ON o.id = q.opportunity_id JOIN problem p ON p.id = o.problem_id
		WHERE q.id = $1`, qid).Scan(&q.Text, &q.LinkedCode, &q.Kind, &q.Depth, &status, &q.OppID, &q.State, &q.Segment, &q.Statement)
	if err != nil {
		return nil, fmt.Errorf("question %d: %w", qid, err)
	}
	if status != "open" {
		return nil, fmt.Errorf("question %d is %s, not open", qid, status)
	}
	rows, err := r.Store.DB.Query(ctx, `SELECT gate_code, status, rationale FROM gate_status WHERE opportunity_id = $1 ORDER BY gate_code`, q.OppID)
	if err != nil {
		return nil, err
	}
	q.Gates = map[string]string{}
	for rows.Next() {
		var c, s, why string
		if err := rows.Scan(&c, &s, &why); err != nil {
			return nil, err
		}
		q.Gates[c], q.Rationale[c] = s, why
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = r.Store.DB.Query(ctx, `
		SELECT 'T' || o.evidence_tier || ' ' || o.category || ': ' || o.statement || ' — "' || left(o.quote, 200) || '" (doc ' || o.raw_document_id || ')'
		FROM opportunity op JOIN problem_observation po ON po.problem_id = op.problem_id
		JOIN observation o ON o.id = po.observation_id
		WHERE op.id = $1 ORDER BY o.evidence_tier, o.specificity DESC LIMIT 8`, q.OppID)
	if err != nil {
		return nil, err
	}
	if q.Evidence, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		return nil, err
	}
	rows, err = r.Store.DB.Query(ctx, `SELECT text || ' → ' || left(answer, 400) FROM question
		WHERE opportunity_id = $1 AND status = 'answered' AND answer IS NOT NULL ORDER BY answered_at DESC LIMIT 6`, q.OppID)
	if err != nil {
		return nil, err
	}
	q.Answered, err = pgx.CollectRows(rows, pgx.RowTo[string])
	return q, err
}

// BuildPrompt renders the research brief.
func BuildPrompt(q *qctx) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Question (%s, linked to %s)\n%s\n\n## Opportunity\nproblem: %s\nsegment: %s\nstage: %s\n\n## Gates so far\n",
		q.Kind, q.LinkedCode, q.Text, q.Statement, q.Segment, q.State)
	for _, code := range []string{"F2", "F3", "F4", "M1", "M2", "M3", "M4", "M5", "M6"} {
		if s, ok := q.Gates[code]; ok {
			fmt.Fprintf(&b, "- %s %s: %s\n", code, s, q.Rationale[code])
		}
	}
	b.WriteString("\n## Evidence already collected\n")
	for _, e := range q.Evidence {
		b.WriteString("- " + e + "\n")
	}
	if len(q.Answered) > 0 {
		b.WriteString("\n## Questions already answered\n")
		for _, a := range q.Answered {
			b.WriteString("- " + a + "\n")
		}
	}
	b.WriteString("\nAnswer the question. Cite only documents returned by your tools, with verbatim quotes.\n")
	return b.String()
}

type modelFinding struct {
	Key       string            `json:"key"`
	Claim     string            `json:"claim"`
	Stance    string            `json:"stance"`
	BearsOn   string            `json:"bears_on"`
	Citations []verify.Citation `json:"citations"`
}

type modelOutput struct {
	Status      string         `json:"status"`
	Answer      string         `json:"answer"`
	Findings    []modelFinding `json:"findings"`
	GateVerdict struct {
		Code        string   `json:"code"`
		Status      string   `json:"status"`
		Rationale   string   `json:"rationale"`
		FindingKeys []string `json:"finding_keys"`
	} `json:"gate_verdict"`
	FollowUps []struct {
		Text        string  `json:"text"`
		LinkedCode  string  `json:"linked_code"`
		Kind        string  `json:"kind"`
		PKill       float64 `json:"p_kill"`
		CostMinutes int     `json:"cost_minutes"`
	} `json:"follow_up_questions"`
}

// ErrNoBudget means the opportunity's earned research budget is spent.
var ErrNoBudget = errors.New("research budget exhausted")

// Run researches one question.
func (r *Researcher) Run(ctx context.Context, taskID *int64, qid int64) (Outcome, error) {
	out := Outcome{QuestionID: qid}
	q, err := r.load(ctx, qid)
	if err != nil {
		return out, queue.Permanent(err)
	}
	// Spend one unit of the opportunity's earned budget, atomically.
	tag, err := r.Store.DB.Exec(ctx, `UPDATE opportunity SET budget_units_used = budget_units_used + 1, last_research_at = now()
		WHERE id = $1 AND budget_units_used < budget_units_total AND NOT saturated`, q.OppID)
	if err != nil {
		return out, err
	}
	if tag.RowsAffected() == 0 {
		out.SkippedReason = ErrNoBudget.Error() + " or desk research saturated"
		return out, nil
	}

	p, err := prompts.Load("research", PromptVersion)
	if err != nil {
		return out, queue.Permanent(err)
	}
	maxTurns := r.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 30
	}
	prompt := BuildPrompt(q)
	res, err := r.LLM.Run(ctx, taskID, claude.Spec{
		TaskType: TaskType, PromptVersion: p.ID(), Model: r.Model, SystemPrompt: p.System, Prompt: prompt, Schema: p.Schema,
		Tools: []string{"WebSearch"}, AllowedTools: append([]string{"WebSearch"}, claude.AOETools...), UseMCP: true,
		MaxTurns: maxTurns, Timeout: 20 * time.Minute,
	})
	if err != nil {
		// Give the unit back: nothing was learned.
		r.Store.DB.Exec(context.WithoutCancel(ctx), `UPDATE opportunity SET budget_units_used = greatest(budget_units_used - 1, 0) WHERE id = $1`, q.OppID) //nolint:errcheck
		return out, err
	}
	var m modelOutput
	if err := json.Unmarshal(res.Structured, &m); err != nil {
		return out, fmt.Errorf("%w: %v", claude.ErrInvalidOutput, err)
	}

	claims := make([]verify.Claim, 0, len(m.Findings))
	for i := range m.Findings {
		if m.Findings[i].Key == "" {
			m.Findings[i].Key = fmt.Sprintf("f%d", i+1)
		}
		claims = append(claims, verify.Claim{Key: m.Findings[i].Key, Text: m.Findings[i].Claim, Citations: m.Findings[i].Citations})
	}
	verdicts, err := r.Verifier.Verify(ctx, taskID, TaskType, claims)
	if err != nil {
		return out, err
	}
	inputHash := store.Hash([]byte(p.ID() + "\x00" + prompt))[:32]
	err = pgx.BeginFunc(ctx, r.Store.DB, func(tx pgx.Tx) error {
		return r.apply(ctx, tx, taskID, q, m, verdicts, p.ID(), inputHash, &out)
	})
	return out, err
}

func (r *Researcher) apply(ctx context.Context, tx pgx.Tx, taskID *int64, q *qctx, m modelOutput, verdicts map[string]verify.Result,
	promptID, inputHash string, out *Outcome) error {
	supported := map[string]bool{}
	for _, f := range m.Findings {
		v := verdicts[f.Key]
		spans, _ := json.Marshal(v.Spans)
		stance := f.Stance
		if stance != "contradicts" && stance != "neutral" {
			stance = "supports"
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO finding (question_id, opportunity_id, task_id, claim, stance, bears_on, citation_spans, verdict, verifier_note,
				model, prompt_version, input_hash)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
			q.QuestionID, q.OppID, taskID, f.Claim, stance, f.BearsOn, spans, v.Verdict, v.Note, r.Model, promptID, inputHash); err != nil {
			return err
		}
		out.Findings++
		if v.Verdict == verify.Supported {
			supported[f.Key] = true
			out.Supported++
		} else if v.Verdict == verify.Unsupported {
			out.Rejected++
		}
	}

	// Question status and answer.
	status := map[string]string{"answered": "answered", "partial": "answered", "needs_human": "needs_human", "cannot_answer": "saturated"}[m.Status]
	if status == "" {
		status = "answered"
	}
	if status == "answered" && out.Supported == 0 {
		status = "saturated" // nothing verifiable: don't count it as answered
	}
	out.Status = status
	answer := strings.TrimSpace(m.Answer)
	if out.Findings > 0 {
		answer += fmt.Sprintf(" [%d of %d findings verified as supported]", out.Supported, out.Findings)
	}
	if _, err := tx.Exec(ctx, `UPDATE question SET status = $2, answer = $3, answered_at = now() WHERE id = $1`,
		q.QuestionID, status, answer); err != nil {
		return err
	}

	// Gate verdict: only PASS/FAIL backed by at least one supported finding,
	// never F1 (the founder's call), and only for the question's own gate.
	gv := m.GateVerdict
	if (gv.Status == "PASS" || gv.Status == "FAIL") && gv.Code != "F1" && gv.Code == q.LinkedCode {
		backed := false
		var evidenceDocs []int64
		for _, k := range gv.FindingKeys {
			if supported[k] {
				backed = true
				for _, s := range verdicts[k].Spans {
					evidenceDocs = append(evidenceDocs, s.DocumentID)
				}
			}
		}
		if backed && q.Gates[gv.Code] != gv.Status {
			if _, err := tx.Exec(ctx, `
				INSERT INTO gate_status (opportunity_id, gate_code, status, evidence_ids, rationale) VALUES ($1, $2, $3, '{}', $4)
				ON CONFLICT (opportunity_id, gate_code) DO UPDATE SET status = EXCLUDED.status, rationale = EXCLUDED.rationale, updated_at = now()`,
				q.OppID, gv.Code, gv.Status, fmt.Sprintf("%s [research on question %d; documents %v]", gv.Rationale, q.QuestionID, evidenceDocs)); err != nil {
				return err
			}
			out.GateChanged = gv.Code + ":" + gv.Status
		}
	}

	// Bounded follow-ups (SCH-4).
	if q.Depth < MaxDepth {
		for i, f := range m.FollowUps {
			if i >= MaxFollowUps || strings.TrimSpace(f.Text) == "" || f.LinkedCode == "" {
				continue
			}
			parent := q.QuestionID
			ok, err := lifecycle.InsertQuestion(ctx, tx, q.OppID, lifecycle.Question{
				Text: strings.TrimSpace(f.Text), LinkedCode: f.LinkedCode, Kind: f.Kind, PKill: f.PKill, CostMinutes: f.CostMinutes,
			}, "agent", &parent, q.Depth+1)
			if err != nil {
				return err
			}
			if ok {
				out.FollowUps++
			}
		}
	}

	// Lifecycle transition.
	state, err := lifecycle.Advance(ctx, tx, q.OppID, "system", "research",
		fmt.Sprintf("after research on question %d (%s): %s", q.QuestionID, q.LinkedCode, out.GateChanged))
	if err != nil {
		return err
	}
	out.State = state

	// Saturation: a round with no gate change, no state change and nothing
	// verified is "no material change"; two in a row stop desk research.
	material := out.GateChanged != "" || state != q.State || out.Supported > 0
	if material {
		_, err = tx.Exec(ctx, `UPDATE opportunity SET no_change_streak = 0 WHERE id = $1`, q.OppID)
		return err
	}
	var streak int
	if err := tx.QueryRow(ctx, `UPDATE opportunity SET no_change_streak = no_change_streak + 1 WHERE id = $1 RETURNING no_change_streak`,
		q.OppID).Scan(&streak); err != nil {
		return err
	}
	if streak >= SaturationStreak {
		out.Saturated = true
		if _, err := tx.Exec(ctx, `UPDATE opportunity SET saturated = true WHERE id = $1`, q.OppID); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO decision (subject_type, subject_id, decision_type, from_state, to_state, actor, notes)
			VALUES ('opportunity', $1, 'saturated', $2, $2, 'system', 'desk research saturated: two rounds without a material change; escalated to the founder')`,
			q.OppID, state)
	}
	return err
}

// Options bound research scheduling.
type Options struct {
	MaxPerDay     int `yaml:"max_per_day"`
	MaxConcurrent int `yaml:"max_concurrent"`
	MaxTurns      int `yaml:"max_turns"`
}

// Defaults fills unset values.
func (o *Options) Defaults() {
	if o.MaxPerDay <= 0 {
		o.MaxPerDay = 30
	}
	if o.MaxConcurrent <= 0 {
		o.MaxConcurrent = 3
	}
	if o.MaxTurns <= 0 {
		o.MaxTurns = 30
	}
}

// Schedule is the admission controller (docs/04 §5): it picks, per
// opportunity with budget left and no research in flight, the open question
// with the best value (kill questions first, then P(kill) ÷ cost), within the
// daily and concurrency caps. Later stages go first.
func Schedule(ctx context.Context, st *store.Store, opt Options) (int, error) {
	opt.Defaults()
	var today, inflight int
	if err := st.DB.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE created_at >= date_trunc('day', now())),
		count(*) FILTER (WHERE state IN ('queued', 'running'))
		FROM task WHERE type = $1`, TaskType).Scan(&today, &inflight); err != nil {
		return 0, err
	}
	room := min(opt.MaxPerDay-today, opt.MaxConcurrent-inflight)
	if room <= 0 {
		return 0, nil
	}
	rows, err := st.DB.Query(ctx, `
		SELECT DISTINCT ON (o.id) q.id, o.id
		FROM opportunity o JOIN question q ON q.opportunity_id = o.id
		JOIN problem p ON p.id = o.problem_id
		WHERE o.state IN ('dossier', 'qualified', 'candidate') AND NOT o.saturated
		  AND o.budget_units_used < o.budget_units_total
		  AND q.status = 'open'
		  AND NOT EXISTS (SELECT 1 FROM task t WHERE t.type = $1 AND t.state IN ('queued', 'running') AND t.opportunity_id = o.id)
		ORDER BY o.id, (q.kind = 'kill') DESC, coalesce(q.voe_estimate, 0) DESC, q.id`, TaskType)
	if err != nil {
		return 0, err
	}
	type pick struct{ qid, oid int64 }
	picks, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (pick, error) {
		var p pick
		err := r.Scan(&p.qid, &p.oid)
		return p, err
	})
	if err != nil {
		return 0, err
	}
	// Stage priority, then evidence strength.
	order := map[int64]int{}
	srows, err := st.DB.Query(ctx, `SELECT o.id, array_position(ARRAY['dossier','qualified','candidate'], o.state) * 1000000
		- p.t1_origin_count * 1000 - p.independent_origin_count FROM opportunity o JOIN problem p ON p.id = o.problem_id`)
	if err != nil {
		return 0, err
	}
	for srows.Next() {
		var id int64
		var rank int
		if err := srows.Scan(&id, &rank); err != nil {
			return 0, err
		}
		order[id] = rank
	}
	if err := srows.Err(); err != nil {
		return 0, err
	}
	sort.SliceStable(picks, func(i, j int) bool { return order[picks[i].oid] < order[picks[j].oid] })
	n := 0
	for _, p := range picks {
		if n >= room {
			break
		}
		_, created, err := queue.Enqueue(ctx, st.DB, queue.NewTask{
			Type: TaskType, DedupeKey: fmt.Sprintf("research:%d", p.qid), Priority: 1,
			Inputs: Inputs{QuestionID: p.qid}, OpportunityID: &p.oid, QuestionID: &p.qid,
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
