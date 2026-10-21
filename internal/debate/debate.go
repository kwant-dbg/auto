// Package debate runs adversarial review as a structured debate between
// independent Claude sessions (docs/04 §4.1, RES-5): a Bull and a Bear argue
// from cited evidence over two rounds, every argument is verified and
// unsupported ones are struck, and a fresh Judge rules. All sessions are
// blinded to the assessment's numbers and rankings.
package debate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
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
const TaskType = "debate"

// PromptVersion is the debate prompt version (Bull, Bear and Judge).
const PromptVersion = "v1"

// Inputs are the task inputs.
type Inputs struct {
	OpportunityID int64 `json:"opportunity_id"`
}

// Debater runs debates.
type Debater struct {
	Store    *store.Store
	LLM      *llm.Client
	Verifier *verify.Verifier
	Model    string
	MaxTurns int
}

// Handle is the worker handler.
func (d *Debater) Handle(ctx context.Context, t *queue.Task) (any, error) {
	var in Inputs
	if err := json.Unmarshal(t.Inputs, &in); err != nil {
		return nil, queue.Permanent(fmt.Errorf("inputs: %w", err))
	}
	return d.Run(ctx, &t.ID, in.OpportunityID)
}

// Argument is one argument after verification.
type Argument struct {
	Key       string            `json:"key"`
	Side      string            `json:"side"`
	Round     int               `json:"round"`
	Claim     string            `json:"claim"`
	BearsOn   string            `json:"bears_on"`
	Rebuts    string            `json:"rebuts"`
	Citations []verify.Citation `json:"citations"`
	Verdict   string            `json:"-"`
	Note      string            `json:"-"`
	Spans     []verify.Span     `json:"-"`
}

// Outcome summarises a debate.
type Outcome struct {
	DebateID   int64  `json:"debate_id"`
	Arguments  int    `json:"arguments"`
	Struck     int    `json:"struck"`
	BullHolds  int    `json:"bull_holds"`
	BearHolds  int    `json:"bear_holds"`
	Unresolved int    `json:"unresolved"`
	Objections int    `json:"objections"`
	Questions  int    `json:"questions"`
	Summary    string `json:"summary"`
}

// Brief builds the blinded brief: facts and evidence, no assessment numbers.
func (d *Debater) Brief(ctx context.Context, oppID int64) (string, *int64, error) {
	var stmt, segment, state string
	var assessmentID *int64
	if err := d.Store.DB.QueryRow(ctx, `SELECT p.canonical_statement, o.segment, o.state, o.current_assessment_id
		FROM opportunity o JOIN problem p ON p.id = o.problem_id WHERE o.id = $1`, oppID).Scan(&stmt, &segment, &state, &assessmentID); err != nil {
		return "", nil, fmt.Errorf("opportunity %d: %w", oppID, err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## Opportunity\nproblem: %s\nsegment: %s\nstage: %s\n\n## Gates\n", stmt, segment, state)
	rows, err := d.Store.DB.Query(ctx, `SELECT gate_code || ' ' || status || ': ' || rationale FROM gate_status WHERE opportunity_id = $1 ORDER BY gate_code`, oppID)
	if err != nil {
		return "", nil, err
	}
	gates, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return "", nil, err
	}
	for _, g := range gates {
		b.WriteString("- " + g + "\n")
	}
	b.WriteString("\n## Evidence (cite by document_id with a verbatim quote)\n")
	rows, err = d.Store.DB.Query(ctx, `
		SELECT 'doc ' || o.raw_document_id || ' (T' || o.evidence_tier || ' ' || o.category || '): "' || left(o.quote, 300) || '"'
		FROM opportunity op JOIN problem_observation po ON po.problem_id = op.problem_id JOIN observation o ON o.id = po.observation_id
		WHERE op.id = $1 ORDER BY o.evidence_tier, o.specificity DESC LIMIT 20`, oppID)
	if err != nil {
		return "", nil, err
	}
	ev, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return "", nil, err
	}
	for _, e := range ev {
		b.WriteString("- " + e + "\n")
	}
	rows, err = d.Store.DB.Query(ctx, `SELECT f.claim || ' [' || f.stance || ', ' || f.bears_on || '; ' ||
		coalesce((SELECT string_agg('doc ' || (s->>'raw_document_id') || ': "' || left(s->>'quote', 200) || '"', '; ')
		          FROM jsonb_array_elements(f.citation_spans) s), '') || ']'
		FROM finding f WHERE f.opportunity_id = $1 AND f.verdict = 'supported' ORDER BY f.id DESC LIMIT 20`, oppID)
	if err != nil {
		return "", nil, err
	}
	fs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return "", nil, err
	}
	if len(fs) > 0 {
		b.WriteString("\n## Verified research findings\n")
		for _, f := range fs {
			b.WriteString("- " + f + "\n")
		}
	}
	return b.String(), assessmentID, nil
}

func (d *Debater) side(ctx context.Context, taskID *int64, side string, round int, brief string, opponent []Argument) ([]Argument, error) {
	p, err := prompts.Load("debate_"+side, PromptVersion)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	b.WriteString(brief)
	if round == 2 {
		other := map[string]string{"bull": "Bear", "bear": "Bull"}[side]
		fmt.Fprintf(&b, "\n## The %s's surviving arguments\n", other)
		for _, a := range opponent {
			fmt.Fprintf(&b, "- [%s, %s] %s\n", a.Key, a.Verdict, a.Claim)
		}
	}
	fmt.Fprintf(&b, "\nThis is round %d. Give your arguments.\n", round)
	maxTurns := d.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 20
	}
	res, err := d.LLM.Run(ctx, taskID, claude.Spec{
		TaskType: TaskType + "." + side, PromptVersion: p.ID(), Model: d.Model, SystemPrompt: p.System, Prompt: b.String(), Schema: p.Schema,
		Tools: []string{"WebSearch"}, AllowedTools: append([]string{"WebSearch"}, claude.AOETools...), UseMCP: true,
		MaxTurns: maxTurns, Timeout: 15 * time.Minute,
	})
	if err != nil {
		return nil, err
	}
	var out struct {
		Arguments []Argument `json:"arguments"`
	}
	if err := json.Unmarshal(res.Structured, &out); err != nil {
		return nil, fmt.Errorf("%w: %v", claude.ErrInvalidOutput, err)
	}
	prefix := map[string]string{"bull": "U", "bear": "B"}[side]
	for i := range out.Arguments {
		// Keys are reassigned so they're unique across sides and rounds.
		out.Arguments[i].Key = fmt.Sprintf("%s%d.%d", prefix, round, i+1)
		out.Arguments[i].Side, out.Arguments[i].Round = side, round
	}
	return out.Arguments, nil
}

// verifyArgs runs the verifier and strikes unsupported arguments.
func (d *Debater) verifyArgs(ctx context.Context, taskID *int64, args []Argument) error {
	claims := make([]verify.Claim, len(args))
	for i, a := range args {
		claims[i] = verify.Claim{Key: a.Key, Text: a.Claim, Citations: a.Citations}
	}
	res, err := d.Verifier.Verify(ctx, taskID, TaskType, claims)
	if err != nil {
		return err
	}
	for i := range args {
		r := res[args[i].Key]
		args[i].Verdict, args[i].Note, args[i].Spans = r.Verdict, r.Note, r.Spans
	}
	return nil
}

func surviving(args []Argument) []Argument {
	var out []Argument
	for _, a := range args {
		if a.Verdict == verify.Supported || a.Verdict == verify.Partial {
			out = append(out, a)
		}
	}
	return out
}

// Run debates one opportunity.
func (d *Debater) Run(ctx context.Context, taskID *int64, oppID int64) (Outcome, error) {
	var out Outcome
	brief, assessmentID, err := d.Brief(ctx, oppID)
	if err != nil {
		return out, queue.Permanent(err)
	}

	// Round 1: independent sessions, in parallel.
	var bull1, bear1 []Argument
	var errBull, errBear error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); bull1, errBull = d.side(ctx, taskID, "bull", 1, brief, nil) }()
	go func() { defer wg.Done(); bear1, errBear = d.side(ctx, taskID, "bear", 1, brief, nil) }()
	wg.Wait()
	if errBull != nil {
		return out, errBull
	}
	if errBear != nil {
		return out, errBear
	}
	round1 := append(append([]Argument{}, bull1...), bear1...)
	if err := d.verifyArgs(ctx, taskID, round1); err != nil {
		return out, err
	}
	bull1, bear1 = round1[:len(bull1)], round1[len(bull1):]

	// Round 2: each side sees the other's surviving arguments.
	var bull2, bear2 []Argument
	wg.Add(2)
	go func() { defer wg.Done(); bull2, errBull = d.side(ctx, taskID, "bull", 2, brief, surviving(bear1)) }()
	go func() { defer wg.Done(); bear2, errBear = d.side(ctx, taskID, "bear", 2, brief, surviving(bull1)) }()
	wg.Wait()
	if errBull != nil {
		return out, errBull
	}
	if errBear != nil {
		return out, errBear
	}
	round2 := append(append([]Argument{}, bull2...), bear2...)
	if err := d.verifyArgs(ctx, taskID, round2); err != nil {
		return out, err
	}
	all := append(append([]Argument{}, round1...), round2...)

	// Judge: a fresh session with no tools sees only facts and survivors.
	judgeP, err := prompts.Load("debate_judge", PromptVersion)
	if err != nil {
		return out, queue.Permanent(err)
	}
	var jb strings.Builder
	jb.WriteString(brief + "\n## Arguments that survived verification\n")
	for _, a := range surviving(all) {
		reb := ""
		if a.Rebuts != "" {
			reb = " (rebuts " + a.Rebuts + ")"
		}
		fmt.Fprintf(&jb, "- [%s] %s round %d, %s%s: %s\n", a.Key, strings.ToUpper(a.Side), a.Round, a.Verdict, reb, a.Claim)
		for _, s := range a.Spans {
			q := s.Quote
			if len(q) > 300 {
				q = q[:300] + "…"
			}
			fmt.Fprintf(&jb, "    quote (%s): %q\n", s.URL, q)
		}
	}
	res, err := d.LLM.Run(ctx, taskID, claude.Spec{
		TaskType: TaskType + ".judge", PromptVersion: judgeP.ID(), Model: d.Model, SystemPrompt: judgeP.System,
		Prompt: jb.String(), Schema: judgeP.Schema, MaxTurns: 3, Timeout: 8 * time.Minute,
	})
	if err != nil {
		return out, err
	}
	var j struct {
		Rulings []struct {
			Claim        string   `json:"claim"`
			Verdict      string   `json:"verdict"`
			Rationale    string   `json:"rationale"`
			ArgumentKeys []string `json:"argument_keys"`
		} `json:"rulings"`
		Objections []struct {
			Text         string   `json:"text"`
			Severity     int      `json:"severity"`
			ArgumentKeys []string `json:"argument_keys"`
		} `json:"objections"`
		KillQuestions []struct {
			Text        string  `json:"text"`
			LinkedCode  string  `json:"linked_code"`
			PKill       float64 `json:"p_kill"`
			CostMinutes int     `json:"cost_minutes"`
		} `json:"kill_questions"`
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(res.Structured, &j); err != nil {
		return out, fmt.Errorf("%w: %v", claude.ErrInvalidOutput, err)
	}

	byKey := map[string]Argument{}
	for _, a := range all {
		byKey[a.Key] = a
	}
	err = pgx.BeginFunc(ctx, d.Store.DB, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO debate (opportunity_id, assessment_id, status, summary, judged_at)
			VALUES ($1, $2, 'judged', $3, now()) RETURNING id`, oppID, assessmentID, j.Summary).Scan(&out.DebateID); err != nil {
			return err
		}
		for _, a := range all {
			spans, _ := json.Marshal(a.Spans)
			struck := a.Verdict == verify.Unsupported || a.Verdict == ""
			var reason any
			if struck {
				reason = a.Note
				out.Struck++
			}
			if _, err := tx.Exec(ctx, `INSERT INTO debate_argument (debate_id, side, round, text, citation_spans, struck, strike_reason, verdict, model, prompt_version)
				VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''), $9, $10)`,
				out.DebateID, a.Side, a.Round, "["+a.Key+"] "+a.Claim, spans, struck, reason, a.Verdict, d.Model, "debate_"+a.Side+"/"+PromptVersion); err != nil {
				return err
			}
			out.Arguments++
		}
		for _, r := range j.Rulings {
			verdict := r.Verdict
			switch verdict {
			case "bull_holds":
				out.BullHolds++
			case "bear_holds":
				out.BearHolds++
			default:
				verdict = "unresolved"
				out.Unresolved++
			}
			if _, err := tx.Exec(ctx, `INSERT INTO debate_ruling (debate_id, claim, verdict, rationale, model, prompt_version)
				VALUES ($1, $2, $3, $4, $5, $6)`, out.DebateID, r.Claim, verdict, r.Rationale+" ["+strings.Join(r.ArgumentKeys, ", ")+"]",
				d.Model, judgeP.ID()); err != nil {
				return err
			}
		}
		for _, o := range j.Objections {
			// Objections must rest on surviving Bear arguments.
			var docs []int64
			valid := false
			for _, k := range o.ArgumentKeys {
				a, ok := byKey[k]
				if ok && a.Side == "bear" && (a.Verdict == verify.Supported || a.Verdict == verify.Partial) {
					valid = true
					for _, s := range a.Spans {
						docs = append(docs, s.DocumentID)
					}
				}
			}
			if !valid {
				continue
			}
			if _, err := tx.Exec(ctx, `INSERT INTO objection (opportunity_id, debate_id, text, evidence_ids, severity, later_outcome)
				VALUES ($1, $2, $3, coalesce($4, '{}'::bigint[]), $5, 'unknown')`, oppID, out.DebateID, o.Text, docs, min(max(o.Severity, 1), 3)); err != nil {
				return err
			}
			out.Objections++
		}
		for _, q := range j.KillQuestions {
			ok, err := lifecycle.InsertQuestion(ctx, tx, oppID, lifecycle.Question{Text: q.Text, LinkedCode: q.LinkedCode, Kind: "kill",
				PKill: q.PKill, CostMinutes: q.CostMinutes}, "debate", nil, 0)
			if err != nil {
				return err
			}
			if ok {
				out.Questions++
			}
		}
		if out.Questions > 0 {
			// New kill questions reopen desk research.
			if _, err := tx.Exec(ctx, `UPDATE opportunity SET saturated = false, no_change_streak = 0,
				budget_units_total = budget_units_total + $2 WHERE id = $1`, oppID, out.Questions); err != nil {
				return err
			}
		}
		if assessmentID != nil {
			if _, err := tx.Exec(ctx, `UPDATE assessment SET debate_summary = $2 WHERE id = $1`, *assessmentID, j.Summary); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `INSERT INTO decision (subject_type, subject_id, decision_type, actor, notes)
			VALUES ('opportunity', $1, 'debate', 'system', $2)`, oppID,
			fmt.Sprintf("%s [bull %d, bear %d, unresolved %d; %d of %d arguments struck]", j.Summary, out.BullHolds, out.BearHolds, out.Unresolved, out.Struck, out.Arguments))
		return err
	})
	out.Summary = j.Summary
	return out, err
}

// Schedule enqueues a debate for dossier opportunities whose current
// assessment hasn't been debated, at most maxPerRun at a time.
func Schedule(ctx context.Context, st *store.Store, maxPerRun int) (int, error) {
	if maxPerRun <= 0 {
		maxPerRun = 2
	}
	rows, err := st.DB.Query(ctx, `
		SELECT o.id, o.current_assessment_id FROM opportunity o
		WHERE o.state = 'dossier' AND o.current_assessment_id IS NOT NULL
		  AND NOT EXISTS (SELECT 1 FROM debate d WHERE d.opportunity_id = o.id AND d.assessment_id = o.current_assessment_id)
		  AND NOT EXISTS (SELECT 1 FROM task t WHERE t.type = $1 AND t.state IN ('queued', 'running') AND t.opportunity_id = o.id)
		ORDER BY o.id LIMIT $2`, TaskType, maxPerRun)
	if err != nil {
		return 0, err
	}
	type pick struct{ opp, assessment int64 }
	picks, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (pick, error) {
		var p pick
		err := r.Scan(&p.opp, &p.assessment)
		return p, err
	})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, p := range picks {
		oid := p.opp
		_, created, err := queue.Enqueue(ctx, st.DB, queue.NewTask{Type: TaskType, Priority: 1, Inputs: Inputs{OpportunityID: p.opp},
			OpportunityID: &oid, DedupeKey: fmt.Sprintf("debate:%d:%d", p.opp, p.assessment)})
		if err != nil {
			return n, err
		}
		if created {
			n++
		}
	}
	return n, nil
}
