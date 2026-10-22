package baseline_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kwant-dbg/auto/internal/baseline"
	"github.com/kwant-dbg/auto/internal/claude"
	"github.com/kwant-dbg/auto/internal/llm"
	"github.com/kwant-dbg/auto/internal/report"
	"github.com/kwant-dbg/auto/internal/testdb"
)

type fake struct {
	calls []claude.Spec
	out   string
}

func (f *fake) Run(_ context.Context, s claude.Spec) (claude.Result, error) {
	f.calls = append(f.calls, s)
	return claude.Result{Structured: json.RawMessage(f.out), NumTurns: 12}, nil
}

const output = `{"cards":[{"segment":"Dental clinics","job":"chase insurance claims","problem":"Clinics need to chase claims but payers stall, causing late cash",
	"evidence":[{"tier":4,"quote":"We lose a day a week chasing claims","source":"Reddit","url":"https://www.reddit.com/r/dentistry/1","claim":"staff time"},
	            {"tier":7,"quote":"probably big","source":"me","url":"https://x.example","claim":"inference"},
	            {"tier":1,"quote":"$2,000 per month for billing","source":"Vendor","url":"https://vendor.example/pricing","claim":"spend"},
	            {"tier":3,"quote":"Claims follow-up from $499/mo","source":"Rival","url":"https://rival.example","claim":"offer"},
	            {"tier":6,"quote":"billing is a mess","source":"Blog","url":"https://blog.example/post","claim":"commentary"}],
	"counter_evidence":[{"tier":3,"quote":"Free claim tracking","source":"Big","url":"https://big.example","claim":"free rival"}],
	"gates":{"F1":"UNKNOWN","F2":"PASS","F3":"PASS","F4":"PASS","M1":"PASS","M2":"PASS","M3":"UNKNOWN","M4":"UNKNOWN","M5":"PASS","M6":"PASS"},
	"prospects":0,"paths":[{"path":"service_first","channel":"cold email","weeks_to_first_dollar":4}],
	"mrr":{"p10":0,"p50":900,"p90":4000,"p_ge_1k":0.45,"p_ge_5k":0.08},
	"weakest_link":"willingness to pay","next_experiment":"10 calls; kill if none pay","founder_hours":6}]}`

func TestRunStoresCardsOncePerWeek(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	f := &fake{out: output}
	r := &baseline.Runner{Store: st, LLM: &llm.Client{Runner: f, Store: st}, Model: "opus", Profile: "Solo SRE.", Cards: 3, MaxTurns: 40, SkipF1: true}

	week := report.ISOWeek(time.Now())
	if created, err := baseline.Schedule(ctx, st, time.Now()); err != nil || !created {
		t.Fatalf("schedule: %v %v", created, err)
	}
	if p, _ := baseline.Pending(ctx, st, week); !p {
		t.Fatal("a queued run is pending")
	}
	out, err := r.Run(ctx, nil, week)
	if err != nil {
		t.Fatal(err)
	}
	if out.Cards != 1 || len(f.calls) != 1 {
		t.Fatalf("out = %+v, calls = %d", out, len(f.calls))
	}
	s := f.calls[0]
	if len(s.Tools) != 1 || s.Tools[0] != "WebSearch" || s.UseMCP || !strings.Contains(s.Prompt, "Solo SRE.") {
		t.Fatalf("B0 must be one web-search session with the founder profile: %+v", s)
	}
	if p, _ := baseline.Pending(ctx, st, week); p {
		t.Fatal("a stored run is not pending")
	}

	var raw []byte
	if err := st.DB.QueryRow(ctx, `SELECT card FROM baseline_card`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var c report.Card
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	// The T7 inference is dropped; three quotes plus one research line remain.
	if len(c.Evidence) != 3 || len(c.Findings) != 1 || c.Origins != 4 || c.Gates["F1"] != "SKIPPED" || c.Evidence[0].Snapshot != nil {
		t.Fatalf("card = %+v", c)
	}
	md := c.Markdown(report.RenderOptions{Blind: true, Label: "Card B"})
	if !strings.Contains(md, "Card B: Dental clinics — chase insurance claims") || strings.Contains(md, "probably big") {
		t.Fatalf("blind card:\n%s", md)
	}

	// A second run for the week reuses the stored one; the next week's
	// prompt lists earlier ideas so B0 doesn't repeat itself.
	if again, err := r.Run(ctx, nil, week); err != nil || again.RunID != out.RunID || len(f.calls) != 1 {
		t.Fatalf("rerun = %+v, %v, calls %d", again, err, len(f.calls))
	}
	if _, err := r.Run(ctx, nil, "2099-W01"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.calls[1].Prompt, "Dental clinics — chase insurance claims") {
		t.Fatal("earlier ideas must be listed")
	}
}
