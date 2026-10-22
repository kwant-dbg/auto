package rank_test

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/kwant-dbg/auto/internal/claude"
	"github.com/kwant-dbg/auto/internal/llm"
	"github.com/kwant-dbg/auto/internal/rank"
	"github.com/kwant-dbg/auto/internal/report"
	"github.com/kwant-dbg/auto/internal/testdb"
)

func TestBradleyTerryOrdersConsistentWinners(t *testing.T) {
	// 0 beats everyone, 1 beats 2, 2 beats nobody; every pair judged twice.
	wins := [][2]int{{0, 1}, {0, 1}, {0, 2}, {0, 2}, {1, 2}, {1, 2}}
	p := rank.BradleyTerry(3, wins)
	if !(p[0] > p[1] && p[1] > p[2]) || p[2] <= 0 {
		t.Fatalf("strengths = %v", p)
	}
	// Split verdicts give equal strength.
	q := rank.BradleyTerry(2, [][2]int{{0, 1}, {1, 0}})
	if d := q[0] - q[1]; d > 1e-6 || d < -1e-6 {
		t.Fatalf("split = %v", q)
	}
}

// prefersLater always picks the card with the later letter, in either order.
type prefersLater struct{ calls int }

var pairLine = regexp.MustCompile(`(?m)^(\d+)\. A = Card ([A-Z]), B = Card ([A-Z])$`)

func (f *prefersLater) Run(_ context.Context, s claude.Spec) (claude.Result, error) {
	f.calls++
	var js []string
	for _, m := range pairLine.FindAllStringSubmatch(s.Prompt, -1) {
		w := "A"
		if m[3] > m[2] {
			w = "B"
		}
		js = append(js, fmt.Sprintf(`{"pair":%s,"winner":%q,"reason":"r"}`, m[1], w))
	}
	return claude.Result{Structured: json.RawMessage(`{"judgments":[` + strings.Join(js, ",") + `]}`)}, nil
}

func TestRankingDecidesDigestCards(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	var ids []int64
	for i := 0; i < 4; i++ {
		var pid, oid, aid int64
		if err := st.DB.QueryRow(ctx, `INSERT INTO problem (canonical_statement, segment) VALUES ($1, 'teams') RETURNING id`, fmt.Sprint("problem ", i)).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		st.DB.QueryRow(ctx, `INSERT INTO opportunity (problem_id, segment, state) VALUES ($1, $2, 'qualified') RETURNING id`, pid, fmt.Sprint("segment ", i)).Scan(&oid) //nolint:errcheck
		// Fallback order: opportunity 0 first (highest P(≥$1k)).
		st.DB.QueryRow(ctx, `INSERT INTO assessment (opportunity_id, ev_p10, ev_p50, ev_p90, p_ge_1k, p_ge_5k, model, prompt_version, input_hash)
			VALUES ($1, 0, 1, 2, $2, 0, 'm', 'p', 'h') RETURNING id`, oid, 0.9-0.1*float64(i)).Scan(&aid) //nolint:errcheck
		st.DB.Exec(ctx, `UPDATE opportunity SET current_assessment_id = $2 WHERE id = $1`, oid, aid) //nolint:errcheck
		ids = append(ids, oid)
	}
	if ok, err := rank.Schedule(ctx, st, 3); err != nil || !ok {
		t.Fatalf("4 eligible for 3 cards must schedule a ranking: %v %v", ok, err)
	}
	if ok, _ := rank.Schedule(ctx, st, 3); ok {
		t.Fatal("the same situation must not be ranked twice")
	}
	eligible, err := rank.Eligible(ctx, st)
	if err != nil || len(eligible) != 4 || eligible[0] != ids[0] {
		t.Fatalf("eligible = %v, %v", eligible, err)
	}

	f := &prefersLater{}
	r := &rank.Ranker{Store: st, LLM: &llm.Client{Runner: f, Store: st}, Model: "sonnet", Profile: "solo"}
	out, err := r.Run(ctx, nil, eligible)
	if err != nil {
		t.Fatal(err)
	}
	if f.calls != 2 || out.Judgments != 12 || out.OrderAgreement != 1 {
		t.Fatalf("calls=%d out=%+v", f.calls, out)
	}
	if !(out.Scores[ids[3]] > out.Scores[ids[2]] && out.Scores[ids[1]] > out.Scores[ids[0]]) {
		t.Fatalf("scores = %v", out.Scores)
	}

	// The ranking, not P(≥$1k), now picks the digest's three cards.
	d, err := (&report.Builder{Store: st, Opt: report.Options{Cards: 3}}).Build(ctx, time.Now(), "human", false)
	if err != nil {
		t.Fatal(err)
	}
	first := strings.Index(d.Markdown, "segment 3")
	second := strings.Index(d.Markdown, "segment 2")
	if first < 0 || second < 0 || first > second || strings.Contains(d.Markdown, "card "+fmt.Sprint(ids[0])+" ") {
		t.Fatalf("digest ignored the ranking:\n%s", d.Markdown)
	}
}
