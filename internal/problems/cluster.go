// Package problems groups verified observations into problems (docs/02 §6):
// candidate search by vector or trigram similarity against every member of
// each problem, automatic attach or creation outside an ambiguous band, and
// model adjudication inside it. Problems form a hierarchy (narrower/broader).
package problems

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kwant-dbg/auto/internal/claude"
	"github.com/kwant-dbg/auto/internal/llm"
	"github.com/kwant-dbg/auto/internal/queue"
	"github.com/kwant-dbg/auto/internal/store"
	"github.com/kwant-dbg/auto/prompts"
)

// TaskType is the clustering sweeper's task type.
const TaskType = "cluster"

// AdjudicatePromptVersion is the adjudicator prompt in use.
const AdjudicatePromptVersion = "v1"

// Config tunes clustering. Similarities are in [0, 1]: cosine similarity for
// vectors, pg_trgm similarity for text. Calibrate them on labelled pairs (PRB-6).
type Config struct {
	UseVectors      bool    `yaml:"-"`
	VectorHigh      float64 `yaml:"vector_high"`
	VectorLow       float64 `yaml:"vector_low"`
	TrigramHigh     float64 `yaml:"trigram_high"`
	TrigramLow      float64 `yaml:"trigram_low"`
	Candidates      int     `yaml:"candidates"`
	MaxPerRun       int     `yaml:"max_per_run"`
	AdjudicateBatch int     `yaml:"adjudicate_batch"`
}

// Defaults fills unset values.
func (c *Config) Defaults() {
	if c.VectorHigh == 0 {
		c.VectorHigh = 0.92
	}
	if c.VectorLow == 0 {
		c.VectorLow = 0.75
	}
	if c.TrigramHigh == 0 {
		c.TrigramHigh = 0.85
	}
	if c.TrigramLow == 0 {
		c.TrigramLow = 0.25
	}
	if c.Candidates <= 0 {
		c.Candidates = 5
	}
	if c.MaxPerRun <= 0 {
		c.MaxPerRun = 200
	}
	if c.AdjudicateBatch <= 0 {
		c.AdjudicateBatch = 15
	}
}

func (c Config) thresholds() (high, low float64) {
	if c.UseVectors {
		return c.VectorHigh, c.VectorLow
	}
	return c.TrigramHigh, c.TrigramLow
}

// Clusterer assigns observations to problems.
type Clusterer struct {
	Store *store.Store
	LLM   *llm.Client // nil disables adjudication: ambiguous observations wait
	Model string
	Cfg   Config
}

// Stats summarises a run.
type Stats struct {
	Considered  int `json:"considered"`
	AutoAttach  int `json:"auto_attach"`
	AutoNew     int `json:"auto_new"`
	Adjudicated int `json:"adjudicated"`
	Waiting     int `json:"waiting"`
	NewProblems int `json:"new_problems"`
}

type obs struct {
	ID        int64
	Statement string
	Quote     string
	Segment   string
}

// Candidate is a problem similar to an observation.
type Candidate struct {
	ProblemID  int64    `json:"problem_id"`
	Similarity float64  `json:"-"`
	Statement  string   `json:"statement"`
	Members    int      `json:"members"`
	Examples   []string `json:"examples,omitempty"`
}

type pending struct {
	obs
	cands []Candidate
	best  float64
}

// lockKey serialises clustering runs: two concurrent runs could each create
// the same new problem.
const lockKey = 7_004_136

// Handle is the worker handler.
func (c *Clusterer) Handle(ctx context.Context, t *queue.Task) (any, error) {
	return c.Run(ctx, &t.ID)
}

// Run clusters up to MaxPerRun unassigned observations.
func (c *Clusterer) Run(ctx context.Context, taskID *int64) (Stats, error) {
	c.Cfg.Defaults()
	var st Stats
	conn, err := c.Store.DB.Acquire(ctx)
	if err != nil {
		return st, err
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, lockKey).Scan(&locked); err != nil {
		return st, err
	}
	if !locked {
		return st, nil // another run is in progress; this one has nothing to do
	}
	defer conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, lockKey) //nolint:errcheck

	todo, err := c.unassigned(ctx)
	if err != nil {
		return st, err
	}
	st.Considered = len(todo)
	high, low := c.Cfg.thresholds()

	var waiting []pending
	for _, o := range todo {
		cands, err := c.candidates(ctx, o)
		if err != nil {
			return st, err
		}
		best := 0.0
		if len(cands) > 0 {
			best = cands[0].Similarity
		}
		ids := candidateIDs(cands)
		switch {
		case len(cands) > 0 && best >= high:
			if err := c.attach(ctx, o.ID, cands[0].ProblemID, "auto_attach", best, ids, "same", "", ""); err != nil {
				return st, err
			}
			st.AutoAttach++
		case len(cands) == 0 || best < low:
			pid, err := c.newProblem(ctx, o, 0)
			if err != nil {
				return st, err
			}
			if err := c.attach(ctx, o.ID, pid, "auto_new", best, ids, "different", "", ""); err != nil {
				return st, err
			}
			st.AutoNew++
			st.NewProblems++
		default:
			waiting = append(waiting, pending{obs: o, cands: cands, best: best})
		}
	}

	if c.LLM == nil {
		st.Waiting = len(waiting)
		return st, nil
	}
	for i := 0; i < len(waiting); i += c.Cfg.AdjudicateBatch {
		batch := waiting[i:min(i+c.Cfg.AdjudicateBatch, len(waiting))]
		done, created, err := c.adjudicate(ctx, taskID, batch)
		st.Adjudicated += done
		st.NewProblems += created
		if err != nil {
			st.Waiting = len(waiting) - st.Adjudicated
			return st, err
		}
	}
	st.Waiting = len(waiting) - st.Adjudicated
	return st, nil
}

func (c *Clusterer) unassigned(ctx context.Context) ([]obs, error) {
	rows, err := c.Store.DB.Query(ctx, `
		SELECT o.id, o.statement, o.quote, o.actor_segment FROM observation o
		WHERE NOT EXISTS (SELECT 1 FROM problem_observation po WHERE po.observation_id = o.id)
		  AND (NOT $1 OR o.embedding IS NOT NULL)
		ORDER BY o.id LIMIT $2`, c.Cfg.UseVectors, c.Cfg.MaxPerRun)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (obs, error) {
		var o obs
		err := r.Scan(&o.ID, &o.Statement, &o.Quote, &o.Segment)
		return o, err
	})
}

// candidates returns the most similar active problems, by the best match
// among each problem's members.
func (c *Clusterer) candidates(ctx context.Context, o obs) ([]Candidate, error) {
	_, low := c.Cfg.thresholds()
	var cands []Candidate
	err := pgx.BeginFunc(ctx, c.Store.DB, func(tx pgx.Tx) error {
		var rows pgx.Rows
		var err error
		if c.Cfg.UseVectors {
			rows, err = tx.Query(ctx, `
				WITH q AS (SELECT embedding FROM observation WHERE id = $1)
				SELECT po.problem_id, max(1 - (m.embedding <=> q.embedding)) AS sim
				FROM observation m JOIN problem_observation po ON po.observation_id = m.id
				JOIN problem p ON p.id = po.problem_id AND p.status = 'active', q
				WHERE m.embedding IS NOT NULL
				GROUP BY po.problem_id ORDER BY sim DESC LIMIT $2`, o.ID, c.Cfg.Candidates)
		} else {
			// Below the low threshold nothing is a candidate, so it doubles as
			// the trigram index's similarity cut-off.
			if _, err := tx.Exec(ctx, fmt.Sprintf(`SET LOCAL pg_trgm.similarity_threshold = %f`, max(low*0.8, 0.05))); err != nil {
				return err
			}
			rows, err = tx.Query(ctx, `
				SELECT po.problem_id, max(similarity(m.statement, $1)) AS sim
				FROM observation m JOIN problem_observation po ON po.observation_id = m.id
				JOIN problem p ON p.id = po.problem_id AND p.status = 'active'
				WHERE m.statement % $1
				GROUP BY po.problem_id ORDER BY sim DESC LIMIT $2`, o.Statement, c.Cfg.Candidates)
		}
		if err != nil {
			return err
		}
		cands, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Candidate, error) {
			var cd Candidate
			err := r.Scan(&cd.ProblemID, &cd.Similarity)
			return cd, err
		})
		if err != nil {
			return err
		}
		for i := range cands {
			if err := tx.QueryRow(ctx, `
				SELECT p.canonical_statement, p.observation_count,
				       coalesce((SELECT array_agg(s ORDER BY s) FROM (
				           SELECT DISTINCT m.statement AS s FROM observation m
				           JOIN problem_observation po ON po.observation_id = m.id
				           WHERE po.problem_id = p.id AND m.statement <> p.canonical_statement LIMIT 2) x), '{}')
				FROM problem p WHERE p.id = $1`, cands[i].ProblemID).Scan(&cands[i].Statement, &cands[i].Members, &cands[i].Examples); err != nil {
				return err
			}
		}
		return nil
	})
	return cands, err
}

func candidateIDs(cs []Candidate) []int64 {
	ids := make([]int64, len(cs))
	for i, c := range cs {
		ids[i] = c.ProblemID
	}
	return ids
}

// newProblem creates a problem seeded by one observation.
func (c *Clusterer) newProblem(ctx context.Context, o obs, parent int64) (int64, error) {
	var p *int64
	if parent != 0 {
		p = &parent
	}
	var id int64
	err := c.Store.DB.QueryRow(ctx, `INSERT INTO problem (parent_id, canonical_statement, segment) VALUES ($1, $2, $3) RETURNING id`,
		p, o.Statement, o.Segment).Scan(&id)
	return id, err
}

// attach links an observation to a problem, logs the decision and refreshes
// the problem's evidence counts, in one transaction.
func (c *Clusterer) attach(ctx context.Context, obsID, problemID int64, method string, sim float64, cands []int64, relation, model, promptVersion string) error {
	return pgx.BeginFunc(ctx, c.Store.DB, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO problem_observation (problem_id, observation_id, attach_method, score)
			VALUES ($1, $2, $3, $4) ON CONFLICT (observation_id) DO NOTHING`,
			problemID, obsID, map[string]string{"auto_attach": "auto", "auto_new": "auto", "adjudicated": "adjudicated"}[method], sim)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil // already assigned
		}
		if _, err := tx.Exec(ctx, `INSERT INTO cluster_decision (observation_id, method, similarity, candidate_ids, relation, problem_id, model, prompt_version)
			VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), NULLIF($8, ''))`, obsID, method, sim, cands, relation, problemID, model, promptVersion); err != nil {
			return err
		}
		return RefreshStats(ctx, tx, problemID)
	})
}

// RefreshStats recomputes a problem's evidence counts from its members:
// observations, independent origins, best tier, and origins with tier ≤ 1.
func RefreshStats(ctx context.Context, tx pgx.Tx, problemID int64) error {
	_, err := tx.Exec(ctx, `
		UPDATE problem p SET
			observation_count = s.n,
			independent_origin_count = s.origins,
			best_tier = s.best,
			t1_origin_count = s.t1,
			updated_at = now()
		FROM (
			SELECT count(*) AS n, count(DISTINCT o.origin_key) AS origins, min(o.evidence_tier) AS best,
			       count(DISTINCT o.origin_key) FILTER (WHERE o.evidence_tier <= 1) AS t1
			FROM problem_observation po JOIN observation o ON o.id = po.observation_id
			WHERE po.problem_id = $1
		) s
		WHERE p.id = $1`, problemID)
	return err
}

type adjudication struct {
	ObservationID int64  `json:"observation_id"`
	Relation      string `json:"relation"`
	ProblemID     int64  `json:"problem_id"`
	NewGroup      string `json:"new_group"`
}

// BuildAdjudicationPrompt renders one batch.
func BuildAdjudicationPrompt(batch []pending) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Classify each of the %d observations below against its candidate problems.\n\n", len(batch))
	for _, p := range batch {
		quote := p.Quote
		if len(quote) > 400 {
			quote = quote[:400] + "…"
		}
		fmt.Fprintf(&b, "<observation id=\"%d\">\nstatement: %s\nquote: %s\ncandidates:\n", p.ID, p.Statement, quote)
		for _, c := range p.cands {
			fmt.Fprintf(&b, "  - problem_id %d (%d observations): %s\n", c.ProblemID, c.Members, c.Statement)
			for _, ex := range c.Examples {
				fmt.Fprintf(&b, "      also: %s\n", ex)
			}
		}
		b.WriteString("</observation>\n\n")
	}
	return b.String()
}

// adjudicate asks the model about one batch and applies its decisions. It
// returns how many observations were assigned and how many problems created.
func (c *Clusterer) adjudicate(ctx context.Context, taskID *int64, batch []pending) (done, created int, err error) {
	p, err := prompts.Load("adjudicate", AdjudicatePromptVersion)
	if err != nil {
		return 0, 0, queue.Permanent(err)
	}
	res, err := c.LLM.Run(ctx, taskID, claude.Spec{
		TaskType: TaskType, PromptVersion: p.ID(), Model: c.Model,
		SystemPrompt: p.System, Prompt: BuildAdjudicationPrompt(batch), Schema: p.Schema,
		MaxTurns: 3, Timeout: 6 * time.Minute,
	})
	if err != nil {
		return 0, 0, err
	}
	var out struct {
		Decisions []adjudication `json:"decisions"`
	}
	if err := json.Unmarshal(res.Structured, &out); err != nil {
		return 0, 0, fmt.Errorf("%w: %v", claude.ErrInvalidOutput, err)
	}

	byID := map[int64]pending{}
	for _, b := range batch {
		byID[b.ID] = b
	}
	groups := map[string]int64{} // new_group label → problem id
	seen := map[int64]bool{}
	for _, d := range out.Decisions {
		pd, ok := byID[d.ObservationID]
		if !ok || seen[d.ObservationID] {
			continue // unknown or duplicate decision
		}
		seen[d.ObservationID] = true
		ids := candidateIDs(pd.cands)
		valid := false
		for _, id := range ids {
			if id == d.ProblemID {
				valid = true
			}
		}
		rel := d.Relation
		if rel != "different" && !valid {
			rel = "different" // the model named a problem that wasn't offered
		}

		target := d.ProblemID
		if rel != "same" {
			label := d.NewGroup
			if label == "" {
				label = fmt.Sprintf("solo-%d", d.ObservationID)
			}
			gid, exists := groups[label]
			if !exists {
				parent := int64(0)
				if rel == "narrower" {
					parent = d.ProblemID
				}
				gid, err = c.newProblem(ctx, pd.obs, parent)
				if err != nil {
					return done, created, err
				}
				if rel == "broader" {
					if _, err := c.Store.DB.Exec(ctx, `UPDATE problem SET parent_id = $1 WHERE id = $2 AND parent_id IS NULL`, gid, d.ProblemID); err != nil {
						return done, created, err
					}
				}
				groups[label] = gid
				created++
			}
			target = gid
		}
		if err := c.attach(ctx, d.ObservationID, target, "adjudicated", pd.best, ids, rel, c.Model, p.ID()); err != nil {
			return done, created, err
		}
		done++
	}
	// Observations the model skipped stay unassigned and are retried next run.
	return done, created, nil
}

// Schedule enqueues one clustering sweeper when observations are waiting and
// none is pending.
func Schedule(ctx context.Context, st *store.Store, useVectors bool) (bool, error) {
	var due bool
	if err := st.DB.QueryRow(ctx, `SELECT
		EXISTS (SELECT 1 FROM observation o WHERE NOT EXISTS (SELECT 1 FROM problem_observation po WHERE po.observation_id = o.id)
		        AND (NOT $2 OR o.embedding IS NOT NULL))
		AND NOT EXISTS (SELECT 1 FROM task WHERE type = $1 AND state IN ('queued', 'running'))`, TaskType, useVectors).Scan(&due); err != nil {
		return false, err
	}
	if !due {
		return false, nil
	}
	_, created, err := queue.Enqueue(ctx, st.DB, queue.NewTask{Type: TaskType, Priority: 2})
	return created, err
}
