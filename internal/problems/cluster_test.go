package problems_test

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/kwant-dbg/auto/internal/claude"
	"github.com/kwant-dbg/auto/internal/embed"
	"github.com/kwant-dbg/auto/internal/llm"
	"github.com/kwant-dbg/auto/internal/problems"
	"github.com/kwant-dbg/auto/internal/store"
	"github.com/kwant-dbg/auto/internal/testdb"
)

type fakeRunner struct {
	respond func(prompt string) string
	prompts []string
}

func (f *fakeRunner) Run(_ context.Context, s claude.Spec) (claude.Result, error) {
	f.prompts = append(f.prompts, s.Prompt)
	return claude.Result{Structured: json.RawMessage(f.respond(s.Prompt)), NumTurns: 1}, nil
}

type fixture struct {
	t   *testing.T
	st  *store.Store
	doc int64
}

func newFixture(t *testing.T) *fixture {
	st := testdb.New(t)
	ctx := context.Background()
	st.SyncTOS(ctx, []store.TOSEntry{{SourceType: "t", AccessMethod: "a", TermsURL: "u", ReviewedAt: "2026-10-21", AllowedUse: "x", RateLimitPerMin: 1}}) //nolint:errcheck
	st.SyncSources(ctx, []store.SourceSpec{{Name: "s", Type: "t"}})                                                                                       //nolint:errcheck
	src, _ := st.GetSource(ctx, "s")
	d, err := st.SaveDocument(ctx, src.ID, store.Document{CanonicalURL: "u", MIME: "text/plain", Content: []byte("x")})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, st: st, doc: d.ID}
}

func (f *fixture) obs(actor, job, obstacle, origin string, tier int, vec []float32) int64 {
	f.t.Helper()
	var emb any
	if vec != nil {
		emb = embed.VectorLiteral(vec)
	}
	var id int64
	if err := f.st.DB.QueryRow(context.Background(), `INSERT INTO observation (raw_document_id, span_start, span_end, quote, category,
		evidence_tier, actor_segment, job, obstacle, origin_key, embedding, model, prompt_version, input_hash)
		VALUES ($1, 0, 1, $2, 'complaint', $3, $4, $5, $6, $7, $8::vector, 'm', 'p', 'h') RETURNING id`,
		f.doc, "quote: "+job, tier, actor, job, obstacle, origin, emb).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *fixture) problemOf(obsID int64) int64 {
	var pid int64
	f.st.DB.QueryRow(context.Background(), `SELECT problem_id FROM problem_observation WHERE observation_id = $1`, obsID).Scan(&pid) //nolint:errcheck
	return pid
}

var obsRe = regexp.MustCompile(`<observation id="(\d+)">\nstatement: ([^\n]*)\n[\s\S]*?candidates:\n  - problem_id (\d+)`)

func TestTrigramClustering(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := f.obs("WooCommerce store owners", "print invoices with correct currency symbols", "the plugin shows USD codes", "author:1", 4, nil)
	b := f.obs("WooCommerce store owners", "print invoices with correct currency symbols", "the plugin shows USD codes", "author:2", 4, nil)
	c := f.obs("WooCommerce store owners", "print invoices with the right currency symbol after updates", "updates reset settings", "author:3", 4, nil)
	d := f.obs("federal facilities offices", "procure HVAC maintenance services", "", "org:gsa", 1, nil)
	e := f.obs("WooCommerce shop owners", "print packing slips in Dutch", "translations are missing", "author:4", 4, nil)
	g := f.obs("WooCommerce shop owners", "print packing slips in German", "translations are missing", "author:5", 4, nil)

	fake := &fakeRunner{respond: func(prompt string) string {
		var decisions []string
		for _, m := range obsRe.FindAllStringSubmatch(prompt, -1) {
			id, stmt, cand := m[1], m[2], m[3]
			switch {
			case strings.Contains(stmt, "currency"):
				decisions = append(decisions, fmt.Sprintf(`{"observation_id":%s,"relation":"same","problem_id":%s,"new_group":""}`, id, cand))
			case strings.Contains(stmt, "packing slips"):
				// Both translation observations are one new problem.
				decisions = append(decisions, fmt.Sprintf(`{"observation_id":%s,"relation":"different","problem_id":0,"new_group":"g1"}`, id))
			default:
				decisions = append(decisions, fmt.Sprintf(`{"observation_id":%s,"relation":"same","problem_id":99999,"new_group":""}`, id))
			}
		}
		return `{"decisions":[` + strings.Join(decisions, ",") + `]}`
	}}
	cl := &problems.Clusterer{Store: f.st, LLM: &llm.Client{Runner: fake, Store: f.st}, Model: "sonnet",
		Cfg: problems.Config{TrigramHigh: 0.95, TrigramLow: 0.3}}
	st, err := cl.Run(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.Considered != 6 || st.AutoAttach < 1 || st.Waiting != 0 {
		t.Fatalf("stats = %+v", st)
	}
	pa, pb, pc, pd, pe, pg := f.problemOf(a), f.problemOf(b), f.problemOf(c), f.problemOf(d), f.problemOf(e), f.problemOf(g)
	if pa == 0 || pa != pb || pa != pc {
		t.Fatalf("currency observations split across problems: %d %d %d", pa, pb, pc)
	}
	if pd == pa || pd == 0 {
		t.Fatalf("unrelated observation joined the currency problem: %d", pd)
	}
	if pe == 0 || pe != pg || pe == pa {
		t.Fatalf("translation observations: %d %d (want one new problem)", pe, pg)
	}

	var origins, n, best int
	f.st.DB.QueryRow(ctx, `SELECT independent_origin_count, observation_count, best_tier FROM problem WHERE id = $1`, pa).Scan(&origins, &n, &best) //nolint:errcheck
	if origins != 3 || n != 3 || best != 4 {
		t.Fatalf("problem stats origins=%d n=%d best=%d", origins, n, best)
	}
	var logged int
	f.st.DB.QueryRow(ctx, `SELECT count(*) FROM cluster_decision`).Scan(&logged) //nolint:errcheck
	if logged != 6 {
		t.Fatalf("cluster decisions logged = %d", logged)
	}
	// A second run has nothing left to do.
	if s, _ := cl.Run(ctx, nil); s.Considered != 0 {
		t.Fatalf("second run considered %d", s.Considered)
	}
}

func TestVectorClusteringAndHierarchy(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := f.obs("agencies", "reconcile client ad spend", "", "host:a", 4, []float32{1, 0, 0})
	b := f.obs("agencies", "reconcile client ad spend monthly", "", "host:b", 4, []float32{0.99, 0.01, 0})
	c := f.obs("agencies", "reconcile Google Ads spend for clients", "", "host:c", 3, []float32{0.85, 0.5, 0})
	d := f.obs("dentists", "verify insurance", "", "host:d", 4, []float32{0, 0, 1})
	unembedded := f.obs("x", "not embedded yet", "", "host:e", 4, nil)

	fake := &fakeRunner{respond: func(prompt string) string {
		m := obsRe.FindStringSubmatch(prompt)
		return fmt.Sprintf(`{"decisions":[{"observation_id":%s,"relation":"narrower","problem_id":%s,"new_group":"g1"}]}`, m[1], m[3])
	}}
	cl := &problems.Clusterer{Store: f.st, LLM: &llm.Client{Runner: fake, Store: f.st}, Model: "sonnet",
		Cfg: problems.Config{UseVectors: true, VectorHigh: 0.95, VectorLow: 0.6}}
	st, err := cl.Run(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.Considered != 4 {
		t.Fatalf("considered %d, want 4 (the unembedded one waits)", st.Considered)
	}
	if f.problemOf(unembedded) != 0 {
		t.Fatal("unembedded observation was clustered in vector mode")
	}
	pa, pb, pc, pd := f.problemOf(a), f.problemOf(b), f.problemOf(c), f.problemOf(d)
	if pa != pb {
		t.Fatalf("near-identical vectors not auto-attached: %d %d", pa, pb)
	}
	if pd == pa {
		t.Fatal("orthogonal vector joined the problem")
	}
	var parent *int64
	f.st.DB.QueryRow(ctx, `SELECT parent_id FROM problem WHERE id = $1`, pc).Scan(&parent) //nolint:errcheck
	if pc == pa || parent == nil || *parent != pa {
		t.Fatalf("narrower observation should be a sub-problem of %d, got problem %d parent %v", pa, pc, parent)
	}
}

func TestWithoutLLMAmbiguousObservationsWait(t *testing.T) {
	f := newFixture(t)
	f.obs("WooCommerce store owners", "print invoices with correct currency symbols", "", "author:1", 4, nil)
	f.obs("WooCommerce store owners", "print invoices with the right currency", "", "author:2", 4, nil)
	cl := &problems.Clusterer{Store: f.st, Cfg: problems.Config{TrigramHigh: 0.99, TrigramLow: 0.2}}
	st, err := cl.Run(context.Background(), nil)
	if err != nil || st.Waiting != 1 || st.AutoNew != 1 {
		t.Fatalf("stats = %+v err = %v", st, err)
	}
}

func TestSchedule(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if ok, _ := problems.Schedule(ctx, f.st, false); ok {
		t.Fatal("nothing to cluster yet")
	}
	f.obs("a", "b", "", "o", 4, nil)
	if ok, err := problems.Schedule(ctx, f.st, false); !ok || err != nil {
		t.Fatalf("schedule = %v %v", ok, err)
	}
	if ok, _ := problems.Schedule(ctx, f.st, false); ok {
		t.Fatal("a second sweeper was queued")
	}
}
