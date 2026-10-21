package problems

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kwant-dbg/auto/internal/claude"
	"github.com/kwant-dbg/auto/internal/queue"
	"github.com/kwant-dbg/auto/internal/store"
	"github.com/kwant-dbg/auto/prompts"
)

// ConsolidateTaskType merges duplicate problems (PRB-5).
const ConsolidateTaskType = "consolidate"

// ConsolidatePromptVersion is the consolidation prompt in use. v2 (week 3)
// merges problems one product would serve, treating different gaps in the
// same job as features of one opportunity; v1 also required the same obstacle,
// which left fragments of one opportunity apart.
const ConsolidatePromptVersion = "v2"

// ConsolidateStats summarises a run.
type ConsolidateStats struct {
	Anchors int `json:"anchors"`
	Merged  int `json:"merged"`
}

type anchor struct {
	ID        int64
	Statement string
	Members   []string
	cands     []Candidate
}

// ConsolidateHandle is the worker handler.
func (c *Clusterer) ConsolidateHandle(ctx context.Context, t *queue.Task) (any, error) {
	return c.Consolidate(ctx, &t.ID, 40)
}

// Consolidate merges duplicates into problems that matter: those with an
// opportunity or at least two independent origins, changed since their last
// consolidation. Larger problems absorb smaller ones. It runs under the
// clustering lock so problems don't change underneath it.
func (c *Clusterer) Consolidate(ctx context.Context, taskID *int64, maxAnchors int) (ConsolidateStats, error) {
	c.Cfg.Defaults()
	var st ConsolidateStats
	if c.LLM == nil {
		return st, nil
	}
	conn, err := c.Store.DB.Acquire(ctx)
	if err != nil {
		return st, err
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, lockKey).Scan(&locked); err != nil || !locked {
		return st, err
	}
	defer conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, lockKey) //nolint:errcheck

	anchors, err := c.anchors(ctx, maxAnchors)
	if err != nil {
		return st, err
	}
	st.Anchors = len(anchors)
	p, err := prompts.Load("consolidate", ConsolidatePromptVersion)
	if err != nil {
		return st, queue.Permanent(err)
	}

	merged := map[int64]bool{}
	const batchSize = 8
	for i := 0; i < len(anchors); i += batchSize {
		var batch []anchor
		for _, a := range anchors[i:min(i+batchSize, len(anchors))] {
			if merged[a.ID] {
				continue // absorbed by an earlier anchor in this run
			}
			cands, err := c.problemCandidates(ctx, a.ID, merged)
			if err != nil {
				return st, err
			}
			a.cands = cands
			if len(cands) == 0 {
				if err := c.markConsolidated(ctx, a.ID); err != nil {
					return st, err
				}
				continue
			}
			batch = append(batch, a)
		}
		if len(batch) == 0 {
			continue
		}
		res, err := c.LLM.Run(ctx, taskID, claude.Spec{
			TaskType: ConsolidateTaskType, PromptVersion: p.ID(), Model: c.Model, SystemPrompt: p.System,
			Prompt: buildConsolidatePrompt(batch), Schema: p.Schema, MaxTurns: 3, Timeout: 6 * time.Minute,
		})
		if err != nil {
			return st, err
		}
		var out struct {
			Anchors []struct {
				AnchorID int64   `json:"anchor_id"`
				SameIDs  []int64 `json:"same_ids"`
			} `json:"anchors"`
		}
		if err := json.Unmarshal(res.Structured, &out); err != nil {
			return st, fmt.Errorf("%w: %v", claude.ErrInvalidOutput, err)
		}
		byID := map[int64]anchor{}
		for _, a := range batch {
			byID[a.ID] = a
		}
		for _, d := range out.Anchors {
			a, ok := byID[d.AnchorID]
			if !ok || merged[a.ID] {
				continue
			}
			offered := map[int64]float64{}
			for _, cd := range a.cands {
				offered[cd.ProblemID] = cd.Similarity
			}
			for _, id := range d.SameIDs {
				sim, ok := offered[id]
				if !ok || merged[id] || id == a.ID {
					continue // not offered, or already merged this run
				}
				if err := Merge(ctx, c.Store, id, a.ID, sim, c.Model, p.ID()); err != nil {
					return st, err
				}
				merged[id] = true
				st.Merged++
			}
		}
		for _, a := range batch {
			if !merged[a.ID] {
				if err := c.markConsolidated(ctx, a.ID); err != nil {
					return st, err
				}
			}
		}
	}
	return st, nil
}

// ResetConsolidation makes every active problem eligible again, e.g. after a
// new consolidation prompt.
func ResetConsolidation(ctx context.Context, st *store.Store) error {
	_, err := st.DB.Exec(ctx, `UPDATE problem SET consolidated_at = NULL WHERE status = 'active'`)
	return err
}

func (c *Clusterer) markConsolidated(ctx context.Context, id int64) error {
	_, err := c.Store.DB.Exec(ctx, `UPDATE problem SET consolidated_at = now() WHERE id = $1`, id)
	return err
}

// anchors are active problems that matter and changed since they were last
// consolidated, largest first.
func (c *Clusterer) anchors(ctx context.Context, limit int) ([]anchor, error) {
	rows, err := c.Store.DB.Query(ctx, `
		SELECT p.id, p.canonical_statement,
		       coalesce((SELECT array_agg(s) FROM (SELECT DISTINCT o.statement AS s FROM problem_observation po
		                 JOIN observation o ON o.id = po.observation_id WHERE po.problem_id = p.id LIMIT 3) x), '{}')
		FROM problem p
		WHERE p.status = 'active'
		  AND (p.independent_origin_count >= 2 OR EXISTS (SELECT 1 FROM opportunity o WHERE o.problem_id = p.id))
		  AND (p.consolidated_at IS NULL OR p.updated_at > p.consolidated_at)
		ORDER BY p.independent_origin_count DESC, p.observation_count DESC, p.id
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (anchor, error) {
		var a anchor
		err := r.Scan(&a.ID, &a.Statement, &a.Members)
		return a, err
	})
}

// problemCandidates finds problems whose members resemble the anchor's
// members, by the best member-to-member similarity.
func (c *Clusterer) problemCandidates(ctx context.Context, anchorID int64, exclude map[int64]bool) ([]Candidate, error) {
	_, low := c.Cfg.thresholds()
	var cands []Candidate
	err := pgx.BeginFunc(ctx, c.Store.DB, func(tx pgx.Tx) error {
		var rows pgx.Rows
		var err error
		if c.Cfg.UseVectors {
			rows, err = tx.Query(ctx, `
				SELECT po2.problem_id, max(1 - (m2.embedding <=> m1.embedding)) AS sim
				FROM problem_observation po1 JOIN observation m1 ON m1.id = po1.observation_id
				JOIN observation m2 ON m2.embedding IS NOT NULL
				JOIN problem_observation po2 ON po2.observation_id = m2.id
				JOIN problem p2 ON p2.id = po2.problem_id AND p2.status = 'active'
				WHERE po1.problem_id = $1 AND po2.problem_id <> $1 AND m1.embedding IS NOT NULL
				GROUP BY po2.problem_id HAVING max(1 - (m2.embedding <=> m1.embedding)) >= $3
				ORDER BY sim DESC LIMIT $2`, anchorID, c.Cfg.Candidates+3, low)
		} else {
			if _, err := tx.Exec(ctx, fmt.Sprintf(`SET LOCAL pg_trgm.similarity_threshold = %f`, max(low, 0.05))); err != nil {
				return err
			}
			rows, err = tx.Query(ctx, `
				SELECT po2.problem_id, max(similarity(m2.statement, m1.statement)) AS sim
				FROM problem_observation po1 JOIN observation m1 ON m1.id = po1.observation_id
				JOIN observation m2 ON m2.statement % m1.statement
				JOIN problem_observation po2 ON po2.observation_id = m2.id
				JOIN problem p2 ON p2.id = po2.problem_id AND p2.status = 'active'
				WHERE po1.problem_id = $1 AND po2.problem_id <> $1
				GROUP BY po2.problem_id ORDER BY sim DESC LIMIT $2`, anchorID, c.Cfg.Candidates+3)
		}
		if err != nil {
			return err
		}
		all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Candidate, error) {
			var cd Candidate
			err := r.Scan(&cd.ProblemID, &cd.Similarity)
			return cd, err
		})
		if err != nil {
			return err
		}
		for _, cd := range all {
			if exclude[cd.ProblemID] {
				continue
			}
			if err := tx.QueryRow(ctx, `SELECT canonical_statement, observation_count FROM problem WHERE id = $1`, cd.ProblemID).
				Scan(&cd.Statement, &cd.Members); err != nil {
				return err
			}
			cands = append(cands, cd)
		}
		return nil
	})
	return cands, err
}

func buildConsolidatePrompt(batch []anchor) string {
	var b strings.Builder
	fmt.Fprintf(&b, "For each of the %d anchor problems below, list the candidates that are the same underlying problem.\n\n", len(batch))
	for _, a := range batch {
		fmt.Fprintf(&b, "<anchor id=\"%d\">\nstatement: %s\n", a.ID, a.Statement)
		for _, m := range a.Members {
			fmt.Fprintf(&b, "  member: %s\n", m)
		}
		b.WriteString("candidates:\n")
		for _, c := range a.cands {
			fmt.Fprintf(&b, "  - problem_id %d (%d observations): %s\n", c.ProblemID, c.Members, c.Statement)
		}
		b.WriteString("</anchor>\n\n")
	}
	return b.String()
}

// Merge moves a problem's evidence, sub-problems and opportunity into another
// problem. If both have opportunities, the absorbed one is closed with reason
// "duplicate" and the survivor keeps its own.
func Merge(ctx context.Context, st *store.Store, from, into int64, sim float64, model, promptVersion string) error {
	return pgx.BeginFunc(ctx, st.DB, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE problem_observation SET problem_id = $2 WHERE problem_id = $1`, from, into); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE problem SET parent_id = $2 WHERE parent_id = $1 AND id <> $2`, from, into); err != nil {
			return err
		}
		var fromOpp, intoOpp *int64
		tx.QueryRow(ctx, `SELECT id FROM opportunity WHERE problem_id = $1`, from).Scan(&fromOpp) //nolint:errcheck
		tx.QueryRow(ctx, `SELECT id FROM opportunity WHERE problem_id = $1`, into).Scan(&intoOpp) //nolint:errcheck
		switch {
		case fromOpp != nil && intoOpp == nil:
			if _, err := tx.Exec(ctx, `UPDATE opportunity SET problem_id = $2 WHERE id = $1`, *fromOpp, into); err != nil {
				return err
			}
		case fromOpp != nil && intoOpp != nil:
			var state string
			if err := tx.QueryRow(ctx, `SELECT state FROM opportunity WHERE id = $1`, *fromOpp).Scan(&state); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE opportunity SET state = 'killed', stage_entered_at = now() WHERE id = $1`, *fromOpp); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO decision (subject_type, subject_id, decision_type, from_state, to_state, actor, reason_code, notes)
				VALUES ('opportunity', $1, 'merge', $2, 'killed', 'system', 'duplicate', $3)`,
				*fromOpp, state, fmt.Sprintf("problem %d merged into problem %d (opportunity %d)", from, into, *intoOpp)); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE problem SET status = 'merged', merged_into = $2, observation_count = 0,
			independent_origin_count = 0, updated_at = now() WHERE id = $1`, from, into); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO problem_merge (from_id, into_id, similarity, model, prompt_version) VALUES ($1, $2, $3, $4, $5)`,
			from, into, sim, model, promptVersion); err != nil {
			return err
		}
		return RefreshStats(ctx, tx, into)
	})
}

// ScheduleConsolidation enqueues a consolidation run when clustering has
// caught up and some problem that matters changed since it was consolidated.
func ScheduleConsolidation(ctx context.Context, st *store.Store) (bool, error) {
	var due bool
	if err := st.DB.QueryRow(ctx, `SELECT
		NOT EXISTS (SELECT 1 FROM task WHERE type IN ($1, $2) AND state IN ('queued', 'running'))
		AND NOT EXISTS (SELECT 1 FROM observation o WHERE NOT o.superseded
		                AND NOT EXISTS (SELECT 1 FROM problem_observation po WHERE po.observation_id = o.id))
		AND EXISTS (SELECT 1 FROM problem p WHERE p.status = 'active'
		            AND (p.independent_origin_count >= 2 OR EXISTS (SELECT 1 FROM opportunity o WHERE o.problem_id = p.id))
		            AND (p.consolidated_at IS NULL OR p.updated_at > p.consolidated_at))`,
		TaskType, ConsolidateTaskType).Scan(&due); err != nil {
		return false, err
	}
	if !due {
		return false, nil
	}
	_, created, err := queue.Enqueue(ctx, st.DB, queue.NewTask{Type: ConsolidateTaskType, Priority: 2})
	return created, err
}

// Settled reports whether clustering and consolidation have caught up (no run
// pending, no multi-origin problem waiting to be consolidated), so triage
// won't turn half-merged fragments into duplicate opportunities. New
// observations arriving don't block it: they're handled in the next round.
func Settled(ctx context.Context, st *store.Store) (bool, error) {
	var settled bool
	err := st.DB.QueryRow(ctx, `SELECT
		NOT EXISTS (SELECT 1 FROM task WHERE type IN ($1, $2) AND state IN ('queued', 'running'))
		AND NOT EXISTS (SELECT 1 FROM problem p WHERE p.status = 'active' AND p.independent_origin_count >= 2
		                AND (p.consolidated_at IS NULL OR p.updated_at > p.consolidated_at))`,
		TaskType, ConsolidateTaskType).Scan(&settled)
	return settled, err
}
