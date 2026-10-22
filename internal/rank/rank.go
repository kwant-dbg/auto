// Package rank orders the opportunities eligible for digest cards by
// pairwise comparison (RPT-2): a model judges every pair of blinded cards in
// both presentation orders, and Bradley-Terry turns the verdicts into one
// strength per opportunity. Judging both orders cancels position bias; how
// often the two orders agree is recorded as a consistency signal.
package rank

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
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
const TaskType = "rank"

// PromptVersion is the ranking prompt version.
const PromptVersion = "v1"

// MaxCandidates bounds a ranking run: n cards make n(n-1)/2 pairs.
const MaxCandidates = 10

// Inputs are the task inputs.
type Inputs struct {
	OpportunityIDs []int64 `json:"opportunity_ids"`
}

// Ranker runs pairwise ranking.
type Ranker struct {
	Store   *store.Store
	LLM     *llm.Client
	Model   string
	Profile string
}

// Handle is the worker handler.
func (r *Ranker) Handle(ctx context.Context, t *queue.Task) (any, error) {
	var in Inputs
	if err := json.Unmarshal(t.Inputs, &in); err != nil {
		return nil, queue.Permanent(fmt.Errorf("inputs: %w", err))
	}
	return r.Run(ctx, &t.ID, in.OpportunityIDs)
}

// Outcome summarises a run.
type Outcome struct {
	RunID          int64             `json:"run_id"`
	Scores         map[int64]float64 `json:"scores"`
	OrderAgreement float64           `json:"order_agreement"`
	Judgments      int               `json:"judgments"`
}

type pair struct{ A, B int } // indexes into the card list

type judgment struct {
	Pair   int    `json:"pair"`
	Winner string `json:"winner"`
	Reason string `json:"reason"`
}

// Run ranks the given opportunities and stores their strengths.
func (r *Ranker) Run(ctx context.Context, taskID *int64, ids []int64) (Outcome, error) {
	var out Outcome
	if len(ids) < 2 {
		return out, queue.Permanent(fmt.Errorf("ranking needs at least 2 opportunities, got %d", len(ids)))
	}
	if len(ids) > MaxCandidates {
		ids = ids[:MaxCandidates]
	}
	p, err := prompts.Load("rank", PromptVersion)
	if err != nil {
		return out, queue.Permanent(err)
	}
	chk := report.NewChecker(r.Store)
	cards := make([]string, len(ids))
	for i, id := range ids {
		c, err := report.LoadCard(ctx, chk, id)
		if err != nil {
			return out, queue.Permanent(err)
		}
		cards[i] = c.Markdown(report.RenderOptions{Blind: true, Label: "Card " + letter(i)})
	}
	var pairs []pair
	for i := range ids {
		for j := i + 1; j < len(ids); j++ {
			pairs = append(pairs, pair{i, j})
		}
	}
	swapped := make([]pair, len(pairs))
	for k, pr := range pairs {
		swapped[k] = pair{pr.B, pr.A}
	}

	var wins [][2]int // [winner, loser] indexes
	verdicts := map[[2]int][]int{}
	var raw []judgment
	for _, order := range [][]pair{pairs, swapped} {
		res, err := r.LLM.Run(ctx, taskID, claude.Spec{
			TaskType: TaskType, PromptVersion: p.ID(), Model: r.Model, SystemPrompt: p.System,
			Prompt: BuildPrompt(r.Profile, cards, order), Schema: p.Schema, MaxTurns: 2, Timeout: 10 * time.Minute,
		})
		if err != nil {
			return out, err
		}
		var m struct {
			Judgments []judgment `json:"judgments"`
		}
		if err := json.Unmarshal(res.Structured, &m); err != nil {
			return out, fmt.Errorf("%w: %v", claude.ErrInvalidOutput, err)
		}
		seen := map[int]bool{}
		for _, j := range m.Judgments {
			if j.Pair < 1 || j.Pair > len(order) || seen[j.Pair] {
				continue
			}
			seen[j.Pair] = true
			pr := order[j.Pair-1]
			w, l := pr.A, pr.B
			if j.Winner == "B" {
				w, l = l, w
			}
			wins = append(wins, [2]int{w, l})
			key := [2]int{min(pr.A, pr.B), max(pr.A, pr.B)}
			verdicts[key] = append(verdicts[key], w)
			raw = append(raw, j)
		}
	}
	agree, both := 0, 0
	for _, v := range verdicts {
		if len(v) == 2 {
			both++
			if v[0] == v[1] {
				agree++
			}
		}
	}
	if both > 0 {
		out.OrderAgreement = float64(agree) / float64(both)
	}
	strength := BradleyTerry(len(ids), wins)
	out.Scores = map[int64]float64{}
	for i, id := range ids {
		out.Scores[id] = strength[i]
	}
	out.Judgments = len(wins)

	err = pgx.BeginFunc(ctx, r.Store.DB, func(tx pgx.Tx) error {
		js, _ := json.Marshal(raw)
		if err := tx.QueryRow(ctx, `INSERT INTO ranking_run (task_id, opportunity_ids, judgments, order_agreement, model, prompt_version)
			VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`, taskID, ids, js, out.OrderAgreement, r.Model, PromptVersion).Scan(&out.RunID); err != nil {
			return err
		}
		// Scores are only comparable within a run: clear the rest.
		if _, err := tx.Exec(ctx, `UPDATE opportunity SET rank_score = NULL, ranked_at = NULL WHERE rank_score IS NOT NULL AND NOT (id = ANY($1))`, ids); err != nil {
			return err
		}
		for id, s := range out.Scores {
			if _, err := tx.Exec(ctx, `UPDATE opportunity SET rank_score = $2, ranked_at = now() WHERE id = $1`, id, s); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// BradleyTerry estimates strengths from [winner, loser] outcomes with the
// MM algorithm, adding half a win each way per pair so that an unbeaten or
// winless card keeps a finite strength. Strengths sum to n.
func BradleyTerry(n int, wins [][2]int) []float64 {
	w := make([]float64, n)
	games := make([][]float64, n)
	for i := range games {
		games[i] = make([]float64, n)
	}
	for _, g := range wins {
		w[g[0]]++
		games[g[0]][g[1]]++
		games[g[1]][g[0]]++
	}
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			if i != j && games[i][j] > 0 {
				w[i] += 0.5
				games[i][j]++ // the two half-wins add one game per pair
			}
		}
	}
	p := make([]float64, n)
	for i := range p {
		p[i] = 1
	}
	for iter := 0; iter < 200; iter++ {
		next := make([]float64, n)
		var sum, delta float64
		for i := 0; i < n; i++ {
			var den float64
			for j := 0; j < n; j++ {
				if i != j && games[i][j] > 0 {
					den += games[i][j] / (p[i] + p[j])
				}
			}
			next[i] = p[i]
			if den > 0 {
				next[i] = w[i] / den
			}
			sum += next[i]
		}
		for i := range next {
			next[i] *= float64(n) / sum
			delta = math.Max(delta, math.Abs(next[i]-p[i]))
		}
		p = next
		if delta < 1e-9 {
			break
		}
	}
	return p
}

// BuildPrompt renders one presentation order.
func BuildPrompt(profile string, cards []string, pairs []pair) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Founder profile\n\n%s\n\n## Cards\n\n", strings.TrimSpace(profile))
	for _, c := range cards {
		b.WriteString(c)
		b.WriteString("\n")
	}
	b.WriteString("## Pairs to judge\n\n")
	for k, pr := range pairs {
		fmt.Fprintf(&b, "%d. A = Card %s, B = Card %s\n", k+1, letter(pr.A), letter(pr.B))
	}
	return b.String()
}

func letter(i int) string { return string(rune('A' + i)) }

// Eligible lists the opportunities a digest could show as cards, best
// first by the digest's fallback order.
func Eligible(ctx context.Context, st *store.Store) ([]int64, error) {
	rows, err := st.DB.Query(ctx, `SELECT o.id FROM opportunity o JOIN problem p ON p.id = o.problem_id
		LEFT JOIN assessment a ON a.id = o.current_assessment_id
		WHERE o.state IN ('qualified', 'dossier', 'validating')
		ORDER BY a.p_ge_1k DESC NULLS LAST, p.t1_origin_count DESC, p.independent_origin_count DESC, o.id LIMIT $1`, MaxCandidates)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

// Schedule enqueues a ranking when more opportunities are eligible than the
// digest has cards for. The dedupe key covers the set and each one's current
// assessment, so a run happens once per distinct situation.
func Schedule(ctx context.Context, st *store.Store, cards int) (bool, error) {
	ids, err := Eligible(ctx, st)
	if err != nil || len(ids) <= cards {
		return false, err
	}
	rows, err := st.DB.Query(ctx, `SELECT id, coalesce(current_assessment_id, 0), state FROM opportunity WHERE id = ANY($1) ORDER BY id`, ids)
	if err != nil {
		return false, err
	}
	var parts []string
	for rows.Next() {
		var id, a int64
		var state string
		if err := rows.Scan(&id, &a, &state); err != nil {
			rows.Close()
			return false, err
		}
		parts = append(parts, fmt.Sprintf("%d:%d:%s", id, a, state))
	}
	rows.Close()
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, ",")))
	_, created, err := queue.Enqueue(ctx, st.DB, queue.NewTask{
		Type: TaskType, DedupeKey: "rank:" + PromptVersion + ":" + hex.EncodeToString(sum[:8]), Priority: 1, Inputs: Inputs{OpportunityIDs: ids},
	})
	return created, err
}
