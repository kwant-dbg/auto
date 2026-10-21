package triage

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/kwant-dbg/auto/internal/claude"
	"github.com/kwant-dbg/auto/internal/founder"
	"github.com/kwant-dbg/auto/internal/llm"
	"github.com/kwant-dbg/auto/internal/store"
	"github.com/kwant-dbg/auto/internal/testdb"
)

func TestEnforceGates(t *testing.T) {
	ev := []Evidence{{ID: 1, Tier: 4}, {ID: 2, Tier: 1}}
	g := EnforceGates([]modelGate{
		{Code: "M1", Status: "PASS", Rationale: "named buyer", EvidenceIDs: nil},             // no citation → UNKNOWN
		{Code: "M2", Status: "PASS", Rationale: "complaint only", EvidenceIDs: []int64{1}},   // T4 only → UNKNOWN
		{Code: "F2", Status: "FAIL", Rationale: "legal advice", EvidenceIDs: []int64{1, 99}}, // 99 isn't a member
		{Code: "F1", Status: "FAIL", Rationale: "model must not decide F1"},
		{Code: "M9", Status: "PASS"},
	}, ev, false)
	if g["M1"].Status != "UNKNOWN" || !strings.Contains(g["M1"].Rationale, "downgraded") {
		t.Errorf("M1 = %+v", g["M1"])
	}
	if g["M2"].Status != "UNKNOWN" {
		t.Errorf("M2 = %+v", g["M2"])
	}
	if g["F2"].Status != "FAIL" || len(g["F2"].EvidenceIDs) != 1 {
		t.Errorf("F2 = %+v", g["F2"])
	}
	if g["F1"].Status != "SKIPPED" {
		t.Errorf("F1 = %+v (disabled in profile)", g["F1"])
	}
	if _, ok := g["M9"]; ok {
		t.Error("unknown gate accepted")
	}
	for _, code := range []string{"M3", "M4", "M5", "M6"} {
		if g[code].Status != "UNKNOWN" {
			t.Errorf("%s should start UNKNOWN", code)
		}
	}
	g = EnforceGates([]modelGate{{Code: "M2", Status: "PASS", EvidenceIDs: []int64{2}}}, ev, true)
	if g["M2"].Status != "PASS" || g["F1"].Status != "UNKNOWN" {
		t.Errorf("M2=%s F1=%s", g["M2"].Status, g["F1"].Status)
	}
}

func TestDecide(t *testing.T) {
	gates := func(kv ...string) map[string]Gate {
		m := EnforceGates(nil, nil, false)
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = Gate{Code: kv[i], Status: kv[i+1]}
		}
		return m
	}
	cases := []struct {
		addr  string
		g     map[string]Gate
		state string
		why   string
	}{
		{"no", gates("M1", "PASS", "M2", "PASS"), "killed", "out_of_scope"},
		{"yes", gates("F3", "FAIL", "M1", "PASS"), "killed", "founder_gate"},
		{"yes", gates("M1", "FAIL"), "parked", "no_buyer"},
		{"yes", gates("M1", "PASS", "M2", "FAIL"), "parked", "no_spend"},
		{"yes", gates("M1", "PASS", "M2", "PASS"), "qualified", ""},
		{"unclear", gates("M1", "PASS"), "candidate", ""},
	}
	for _, c := range cases {
		state, why := Decide(c.addr, c.g)
		if state != c.state || why != c.why {
			t.Errorf("Decide(%s, …) = %s/%s, want %s/%s", c.addr, state, why, c.state, c.why)
		}
	}
}

type fakeRunner struct{ out string }

func (f *fakeRunner) Run(_ context.Context, s claude.Spec) (claude.Result, error) {
	return claude.Result{Structured: json.RawMessage(f.out), NumTurns: 1}, nil
}

func seedProblem(t *testing.T, st *store.Store, origins int, tier int) (problemID int64, obsIDs []int64) {
	t.Helper()
	ctx := context.Background()
	st.SyncTOS(ctx, []store.TOSEntry{{SourceType: "t", AccessMethod: "a", TermsURL: "u", ReviewedAt: "2026-10-21", AllowedUse: "x", RateLimitPerMin: 1}}) //nolint:errcheck
	st.SyncSources(ctx, []store.SourceSpec{{Name: "s", Type: "t"}})                                                                                       //nolint:errcheck
	src, _ := st.GetSource(ctx, "s")
	d, _ := st.SaveDocument(ctx, src.ID, store.Document{CanonicalURL: "https://x", MIME: "text/plain", Content: []byte("x")})
	if err := st.DB.QueryRow(ctx, `INSERT INTO problem (canonical_statement, segment) VALUES ('agencies need to reconcile ad spend', 'agencies') RETURNING id`).Scan(&problemID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < origins; i++ {
		var id int64
		if err := st.DB.QueryRow(ctx, `INSERT INTO observation (raw_document_id, span_start, span_end, quote, category, evidence_tier,
			actor_segment, job, origin_key, model, prompt_version, input_hash)
			VALUES ($1, 0, 1, 'we pay a VA to reconcile ad spend', 'spend', $2, 'agencies', 'reconcile ad spend', $3, 'm', 'p', 'h') RETURNING id`,
			d.ID, tier, fmt.Sprint("origin-", i)).Scan(&id); err != nil {
			t.Fatal(err)
		}
		st.DB.Exec(ctx, `INSERT INTO problem_observation (problem_id, observation_id, attach_method) VALUES ($1, $2, 'auto')`, problemID, id) //nolint:errcheck
		obsIDs = append(obsIDs, id)
	}
	if _, err := st.DB.Exec(ctx, `UPDATE problem SET independent_origin_count = $2::int, observation_count = $2::int, best_tier = $3::int,
		t1_origin_count = CASE WHEN $3::int <= 1 THEN $2::int ELSE 0 END WHERE id = $1`, problemID, origins, tier); err != nil {
		t.Fatal(err)
	}
	return problemID, obsIDs
}

func triageOutput(m1, m2 string, evidence int64, addressable string) string {
	return fmt.Sprintf(`{"canonical_statement":"Digital agencies need to reconcile client ad spend across platforms but do it by hand, causing billing errors",
		"segment":"digital marketing agencies with 5-50 staff","addressable":{"status":%q,"rationale":"software can pull spend data"},
		"gates":[{"code":"M1","status":%q,"rationale":"agency owners pay","evidence_ids":[%d]},
		         {"code":"M2","status":%q,"rationale":"they pay VAs","evidence_ids":[%d]},
		         {"code":"F3","status":"PASS","rationale":"self-serve","evidence_ids":[]}],
		"questions":[{"text":"How many agencies reconcile spend weekly?","linked_code":"retention_12m","kind":"standard","p_kill":0.1,"cost_minutes":30}],
		"summary":"Agencies pay people to reconcile ad spend."}`, addressable, m1, evidence, m2, evidence)
}

func TestRunQualifiesAndCreatesKillQuestions(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	pid, obs := seedProblem(t, st, 3, 1)
	prof := &founder.Profile{}
	tr := &Triager{Store: st, LLM: &llm.Client{Runner: &fakeRunner{out: triageOutput("PASS", "PASS", obs[0], "yes")}, Store: st}, Model: "sonnet", Founder: prof}

	out, err := tr.Run(ctx, nil, pid)
	if err != nil {
		t.Fatal(err)
	}
	if out.State != "qualified" || out.Questions != 5 { // 4 kill questions + 1 from the model
		t.Fatalf("outcome = %+v", out)
	}
	var stmt, segment string
	var triaged int
	st.DB.QueryRow(ctx, `SELECT canonical_statement, segment, triaged_origin_count FROM problem WHERE id = $1`, pid).Scan(&stmt, &segment, &triaged) //nolint:errcheck
	if !strings.HasPrefix(stmt, "Digital agencies") || segment != "digital marketing agencies with 5-50 staff" || triaged != 3 {
		t.Fatalf("problem not updated: %q %q %d", stmt, segment, triaged)
	}
	var budget int
	st.DB.QueryRow(ctx, `SELECT budget_units_total FROM opportunity WHERE id = $1`, out.OpportunityID).Scan(&budget) //nolint:errcheck
	if budget != BudgetQualified {
		t.Fatalf("budget = %d", budget)
	}
	var kills int
	st.DB.QueryRow(ctx, `SELECT count(*) FROM question WHERE opportunity_id = $1 AND kind = 'kill' AND linked_code IN ('M3','M4','M5','M6')`, out.OpportunityID).Scan(&kills) //nolint:errcheck
	if kills != 4 {
		t.Fatalf("kill questions = %d", kills)
	}
	var decisions int
	var to string
	st.DB.QueryRow(ctx, `SELECT count(*), max(to_state) FROM decision WHERE subject_type = 'opportunity' AND subject_id = $1`, out.OpportunityID).Scan(&decisions, &to) //nolint:errcheck
	if decisions != 1 || to != "qualified" {
		t.Fatalf("decisions = %d to=%s", decisions, to)
	}

	// Re-triage replaces open triage questions instead of piling them up.
	if _, err := tr.Run(ctx, nil, pid); err != nil {
		t.Fatal(err)
	}
	var qs int
	st.DB.QueryRow(ctx, `SELECT count(*) FROM question WHERE opportunity_id = $1`, out.OpportunityID).Scan(&qs) //nolint:errcheck
	if qs != 5 {
		t.Fatalf("questions after re-triage = %d", qs)
	}
}

func TestRunDowngradesWeakSpendAndKillsUnaddressable(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	pid, obs := seedProblem(t, st, 2, 4) // only T4 evidence
	tr := &Triager{Store: st, LLM: &llm.Client{Runner: &fakeRunner{out: triageOutput("PASS", "PASS", obs[0], "yes")}, Store: st}, Model: "sonnet"}
	out, err := tr.Run(ctx, nil, pid)
	if err != nil || out.State != "candidate" {
		t.Fatalf("outcome = %+v err = %v; a T4-only M2 PASS must not qualify", out, err)
	}

	pid2, obs2 := seedProblem(t, st, 2, 1)
	tr.LLM = &llm.Client{Runner: &fakeRunner{out: triageOutput("PASS", "PASS", obs2[0], "no")}, Store: st}
	out, err = tr.Run(ctx, nil, pid2)
	if err != nil || out.State != "killed" || out.Reason != "out_of_scope" || out.Questions != 0 {
		t.Fatalf("outcome = %+v err = %v", out, err)
	}
}

func TestScheduleEligibilityAndRetriage(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	strong, _ := seedProblem(t, st, 3, 1)
	weak, _ := seedProblem(t, st, 1, 4) // below min origins

	n, err := Schedule(ctx, st, Options{MinIndependentOrigins: 2})
	if err != nil || n != 1 {
		t.Fatalf("scheduled %d err %v", n, err)
	}
	var inputs json.RawMessage
	st.DB.QueryRow(ctx, `SELECT inputs FROM task WHERE type = 'triage'`).Scan(&inputs) //nolint:errcheck
	if !strings.Contains(string(inputs), fmt.Sprint(strong)) || strings.Contains(string(inputs), fmt.Sprintf(":%d}", weak)) {
		t.Fatalf("inputs = %s", inputs)
	}
	// Ticking again doesn't duplicate.
	if n, _ := Schedule(ctx, st, Options{MinIndependentOrigins: 2}); n != 0 {
		t.Fatalf("duplicate triage scheduled: %d", n)
	}

	// After triage as a candidate, it's re-triaged only once evidence doubles.
	tr := &Triager{Store: st, LLM: &llm.Client{Runner: &fakeRunner{out: triageOutput("UNKNOWN", "UNKNOWN", 0, "yes")}, Store: st}, Model: "sonnet"}
	if _, err := tr.Run(ctx, nil, strong); err != nil {
		t.Fatal(err)
	}
	st.DB.Exec(ctx, `UPDATE problem SET independent_origin_count = 5 WHERE id = $1`, strong) //nolint:errcheck
	if n, _ := Schedule(ctx, st, Options{MinIndependentOrigins: 2}); n != 0 {
		t.Fatalf("re-triaged before evidence doubled: %d", n)
	}
	st.DB.Exec(ctx, `UPDATE problem SET independent_origin_count = 6 WHERE id = $1`, strong) //nolint:errcheck
	if n, _ := Schedule(ctx, st, Options{MinIndependentOrigins: 2}); n != 1 {
		t.Fatalf("re-triage not scheduled after evidence doubled: %d", n)
	}
	// Daily cap.
	if n, _ := Schedule(ctx, st, Options{MinIndependentOrigins: 1, MaxPerDay: 2}); n != 0 {
		t.Fatalf("scheduled past the daily cap: %d", n)
	}
}
