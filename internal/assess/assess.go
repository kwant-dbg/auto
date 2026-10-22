package assess

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
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
const TaskType = "assess"

// PromptVersion is the assessor prompt in use.
const PromptVersion = "v1"

// Draws is the Monte Carlo sample size.
const Draws = 10_000

// Inputs are the task inputs.
type Inputs struct {
	OpportunityID int64 `json:"opportunity_id"`
}

// Assessor runs assessments.
type Assessor struct {
	Store   *store.Store
	LLM     *llm.Client
	Model   string
	Founder *founder.Profile
}

// Handle is the worker handler.
func (a *Assessor) Handle(ctx context.Context, t *queue.Task) (any, error) {
	var in Inputs
	if err := json.Unmarshal(t.Inputs, &in); err != nil {
		return nil, queue.Permanent(fmt.Errorf("inputs: %w", err))
	}
	return a.Run(ctx, &t.ID, in.OpportunityID)
}

// ref is a citable piece of evidence shown to the model.
type ref struct {
	Key  string
	Text string
	Tier *int
}

type brief struct {
	OppID     int64
	Statement string
	Segment   string
	State     string
	Gates     []string
	Refs      []ref
}

func (a *Assessor) load(ctx context.Context, oppID int64) (*brief, error) {
	b := &brief{OppID: oppID}
	if err := a.Store.DB.QueryRow(ctx, `SELECT p.canonical_statement, o.segment, o.state FROM opportunity o JOIN problem p ON p.id = o.problem_id
		WHERE o.id = $1`, oppID).Scan(&b.Statement, &b.Segment, &b.State); err != nil {
		return nil, fmt.Errorf("opportunity %d: %w", oppID, err)
	}
	rows, err := a.Store.DB.Query(ctx, `SELECT gate_code || ' ' || status || ': ' || rationale FROM gate_status WHERE opportunity_id = $1 ORDER BY gate_code`, oppID)
	if err != nil {
		return nil, err
	}
	if b.Gates, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		return nil, err
	}
	// Only verified-supported findings are citable.
	rows, err = a.Store.DB.Query(ctx, `SELECT 'F' || f.id, '[' || f.stance || ', ' || f.bears_on || '] ' || f.claim
		FROM finding f WHERE f.opportunity_id = $1 AND f.verdict = 'supported' AND NOT f.retracted ORDER BY f.id DESC LIMIT 40`, oppID)
	if err != nil {
		return nil, err
	}
	findings, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ref, error) {
		var x ref
		err := r.Scan(&x.Key, &x.Text)
		return x, err
	})
	if err != nil {
		return nil, err
	}
	rows, err = a.Store.DB.Query(ctx, `SELECT 'O' || o.id, 'T' || o.evidence_tier || ' ' || o.category || ': ' || o.statement || ' — "' || left(o.quote, 200) || '"', o.evidence_tier
		FROM opportunity op JOIN problem_observation po ON po.problem_id = op.problem_id JOIN observation o ON o.id = po.observation_id
		WHERE op.id = $1 ORDER BY o.evidence_tier, o.specificity DESC LIMIT 25`, oppID)
	if err != nil {
		return nil, err
	}
	obs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ref, error) {
		var x ref
		var tier int
		err := r.Scan(&x.Key, &x.Text, &tier)
		x.Tier = &tier
		return x, err
	})
	b.Refs = append(findings, obs...)
	return b, err
}

// BuildPrompt renders the assessment brief.
func BuildPrompt(profile string, b *brief) string {
	var s strings.Builder
	s.WriteString("## Founder profile\n" + profile + "\n## Opportunity\n")
	fmt.Fprintf(&s, "problem: %s\nsegment: %s\nstage: %s\n\n## Gates\n", b.Statement, b.Segment, b.State)
	for _, g := range b.Gates {
		s.WriteString("- " + g + "\n")
	}
	s.WriteString("\n## Evidence (cite by reference)\n")
	for _, r := range b.Refs {
		fmt.Fprintf(&s, "- %s: %s\n", r.Key, r.Text)
	}
	return s.String()
}

type modelFactor struct {
	Code      string   `json:"code"`
	Low       float64  `json:"low"`
	Base      float64  `json:"base"`
	High      float64  `json:"high"`
	Refs      []string `json:"refs"`
	Rationale string   `json:"rationale"`
}

type modelOutput struct {
	Factors         []modelFactor   `json:"factors"`
	Paths           json.RawMessage `json:"paths"`
	First10         json.RawMessage `json:"first10"`
	NextExperiments json.RawMessage `json:"next_experiments"`
	Summary         string          `json:"summary"`
}

// Outcome summarises an assessment.
type Outcome struct {
	AssessmentID int64   `json:"assessment_id"`
	P10          float64 `json:"p10"`
	P50          float64 `json:"p50"`
	P90          float64 `json:"p90"`
	PGe500       float64 `json:"p_ge_500"`
	PGe1k        float64 `json:"p_ge_1k"`
	PGe5k        float64 `json:"p_ge_5k"`
	Weakest      string  `json:"weakest"`
	Clamped      int     `json:"clamped"`
}

var refRe = regexp.MustCompile(`^[FO]\d+$`)

// Run assesses one opportunity.
func (a *Assessor) Run(ctx context.Context, taskID *int64, oppID int64) (Outcome, error) {
	var out Outcome
	b, err := a.load(ctx, oppID)
	if err != nil {
		return out, queue.Permanent(err)
	}
	p, err := prompts.Load("assess", PromptVersion)
	if err != nil {
		return out, queue.Permanent(err)
	}
	profile := "No founder profile configured.\n"
	if a.Founder != nil {
		profile = a.Founder.Summary()
	}
	prompt := BuildPrompt(profile, b)
	res, err := a.LLM.Run(ctx, taskID, claude.Spec{
		TaskType: TaskType, PromptVersion: p.ID(), Model: a.Model, SystemPrompt: p.System, Prompt: prompt, Schema: p.Schema,
		MaxTurns: 3, Timeout: 8 * time.Minute,
	})
	if err != nil {
		return out, err
	}
	var m modelOutput
	if err := json.Unmarshal(res.Structured, &m); err != nil {
		return out, fmt.Errorf("%w: %v", claude.ErrInvalidOutput, err)
	}

	known := map[string]*int{}
	for _, r := range b.Refs {
		known[r.Key] = r.Tier
	}
	type estimate struct {
		r       Range
		clamped bool
		refs    []int64
		tier    *int
	}
	est := map[string]estimate{}
	for _, f := range m.Factors {
		if _, ok := Units[f.Code]; !ok {
			continue
		}
		var ids []int64
		var best *int
		for _, k := range f.Refs {
			k = strings.TrimSpace(k)
			tier, ok := known[k]
			if !refRe.MatchString(k) || !ok {
				continue // cited something it wasn't shown
			}
			id, _ := strconv.ParseInt(k[1:], 10, 64)
			ids = append(ids, id)
			if tier != nil && (best == nil || *tier < *best) {
				t := *tier
				best = &t
			}
		}
		r, clamped := Clamp(f.Code, Range{f.Low, f.Base, f.High}, len(ids) > 0)
		est[f.Code] = estimate{r: r, clamped: clamped, refs: ids, tier: best}
	}
	// Any factor the model left out is a prior.
	for code := range Units {
		if _, ok := est[code]; !ok {
			r, _ := Clamp(code, Range{}, false)
			est[code] = estimate{r: r, clamped: true}
		}
	}
	factors := map[string]Range{}
	for code, e := range est {
		factors[code] = e.r
		if e.clamped {
			out.Clamped++
		}
	}
	sim := Simulate(factors, Draws, uint64(oppID))
	out.P10, out.P50, out.P90, out.PGe500, out.PGe1k, out.PGe5k, out.Weakest = sim.P10, sim.P50, sim.P90, sim.PGe500, sim.PGe1k, sim.PGe5k, sim.Weakest

	inputs, _ := json.Marshal(m.Factors)
	inputHash := store.Hash([]byte(p.ID() + "\x00" + prompt))[:32]
	err = pgx.BeginFunc(ctx, a.Store.DB, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO assessment (opportunity_id, ev_p10, ev_p50, ev_p90, p_ge_1k, p_ge_5k, weakest_factor, paths, first10_plan,
				next_experiments, inputs, p_build, debate_summary, model, prompt_version, input_hash, p_ge_500)
			VALUES ($1, $2, $3, $4, $5, $6, $7, coalesce($8, '[]'::jsonb), coalesce($9, '{}'::jsonb), coalesce($10, '[]'::jsonb), $11, $12, '',
				$13, $14, $15, $16) RETURNING id`,
			oppID, sim.P10, sim.P50, sim.P90, sim.PGe1k, sim.PGe5k, sim.Weakest, nullJSON(m.Paths), nullJSON(m.First10),
			nullJSON(m.NextExperiments), inputs, factors[PBuild].Base, a.Model, p.ID(), inputHash, sim.PGe500).Scan(&out.AssessmentID); err != nil {
			return err
		}
		for code, e := range est {
			if _, err := tx.Exec(ctx, `INSERT INTO factor_estimate (assessment_id, factor_code, low, base, high, unit, evidence_tier_max,
				citation_ids, clamped_to_prior) VALUES ($1, $2, $3, $4, $5, $6, $7, coalesce($8, '{}'::bigint[]), $9)`,
				out.AssessmentID, code, e.r.Low, e.r.Base, e.r.High, Units[code], e.tier, e.refs, e.clamped); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `UPDATE opportunity SET current_assessment_id = $2 WHERE id = $1`, oppID, out.AssessmentID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO decision (subject_type, subject_id, decision_type, from_state, to_state, actor, notes, factor_snapshot)
			VALUES ('opportunity', $1, 'assessment', $2, $2, 'system', $3, $4)`, oppID, b.State,
			fmt.Sprintf("%s [P50 MRR@12m $%.0f, P(≥$1k) %.0f%%, weakest: %s]", m.Summary, sim.P50, 100*sim.PGe1k, sim.Weakest), inputs)
		return err
	})
	return out, err
}

func nullJSON(b json.RawMessage) any {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	return []byte(b)
}

// Schedule enqueues assessments for dossier opportunities that have none, or
// whose newest supported finding is newer than their assessment (at most one
// re-assessment a day each), and a first assessment for each qualified
// opportunity, so a digest card always carries ranges, paths and a next
// experiment.
func Schedule(ctx context.Context, st *store.Store, maxPerRun int) (int, error) {
	if maxPerRun <= 0 {
		maxPerRun = 5
	}
	rows, err := st.DB.Query(ctx, `
		SELECT o.id FROM opportunity o
		LEFT JOIN assessment a ON a.id = o.current_assessment_id
		WHERE ((o.state = 'dossier'
		        AND (a.id IS NULL OR (a.created_at < now() - interval '1 day'
		             AND EXISTS (SELECT 1 FROM finding f WHERE f.opportunity_id = o.id AND f.verdict = 'supported' AND f.created_at > a.created_at))))
		       OR (o.state = 'qualified' AND a.id IS NULL))
		  AND NOT EXISTS (SELECT 1 FROM task t WHERE t.type = $1 AND t.state IN ('queued', 'running') AND t.opportunity_id = o.id)
		ORDER BY o.id LIMIT $2`, TaskType, maxPerRun)
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		oid := id
		_, created, err := queue.Enqueue(ctx, st.DB, queue.NewTask{Type: TaskType, Priority: 1, Inputs: Inputs{OpportunityID: id},
			OpportunityID: &oid, DedupeKey: fmt.Sprintf("assess:%d:%s", id, time.Now().UTC().Format("2006-01-02"))})
		if err != nil {
			return n, err
		}
		if created {
			n++
		}
	}
	return n, nil
}

// BackfillPGe500 fills P(MRR ≥ $500) on assessments made before it was
// computed, by re-running each one's seeded simulation over its stored
// factor ranges: the same seed and ranges give the same draws, so the other
// figures it reports are unchanged.
func BackfillPGe500(ctx context.Context, st *store.Store) (int, error) {
	rows, err := st.DB.Query(ctx, `SELECT a.id, a.opportunity_id, f.factor_code, f.low, f.base, f.high
		FROM assessment a JOIN factor_estimate f ON f.assessment_id = a.id WHERE a.p_ge_500 IS NULL ORDER BY a.id`)
	if err != nil {
		return 0, err
	}
	type pending struct {
		opp     int64
		factors map[string]Range
	}
	todo := map[int64]*pending{}
	var order []int64
	for rows.Next() {
		var id, opp int64
		var code string
		var r Range
		if err := rows.Scan(&id, &opp, &code, &r.Low, &r.Base, &r.High); err != nil {
			rows.Close()
			return 0, err
		}
		if todo[id] == nil {
			todo[id] = &pending{opp: opp, factors: map[string]Range{}}
			order = append(order, id)
		}
		todo[id].factors[code] = r
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, id := range order {
		sim := Simulate(todo[id].factors, Draws, uint64(todo[id].opp))
		if _, err := st.DB.Exec(ctx, `UPDATE assessment SET p_ge_500 = $2 WHERE id = $1`, id, sim.PGe500); err != nil {
			return 0, err
		}
	}
	return len(order), nil
}
