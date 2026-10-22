package research_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/kwant-dbg/auto/internal/assess"
	"github.com/kwant-dbg/auto/internal/claude"
	"github.com/kwant-dbg/auto/internal/debate"
	"github.com/kwant-dbg/auto/internal/lifecycle"
	"github.com/kwant-dbg/auto/internal/llm"
	"github.com/kwant-dbg/auto/internal/research"
	"github.com/kwant-dbg/auto/internal/store"
	"github.com/kwant-dbg/auto/internal/testdb"
	"github.com/kwant-dbg/auto/internal/verify"
)

// router answers each call by task type.
type router struct {
	mu     sync.Mutex
	routes map[string]func(prompt string) string
	calls  []claude.Spec
}

func (r *router) Run(_ context.Context, s claude.Spec) (claude.Result, error) {
	r.mu.Lock()
	r.calls = append(r.calls, s)
	f := r.routes[s.TaskType]
	r.mu.Unlock()
	if f == nil {
		return claude.Result{}, fmt.Errorf("unexpected task type %s", s.TaskType)
	}
	return claude.Result{Structured: json.RawMessage(f(s.Prompt)), NumTurns: 3}, nil
}

// verifyAll marks every claim supported, except claims containing "WEAK".
func verifyAll(prompt string) string {
	var vs []string
	for _, line := range strings.Split(prompt, "\n") {
		if strings.HasPrefix(line, "<claim key=") {
			key := strings.TrimSuffix(strings.TrimPrefix(line, "<claim key="), ">")
			vs = append(vs, fmt.Sprintf(`{"key":%s,"verdict":"supported","note":""}`, key))
		}
	}
	return `{"verdicts":[` + strings.Join(vs, ",") + `]}`
}

type env struct {
	st    *store.Store
	oppID int64
	doc   int64
}

const page = "Acme Templates costs $10 per month for 10 users. It has 4.8 stars from 900 reviews. Templates can be shared across projects."

func setup(t *testing.T, state string) *env {
	t.Helper()
	st := testdb.New(t)
	ctx := context.Background()
	st.SyncTOS(ctx, []store.TOSEntry{{SourceType: "web_fetch", AccessMethod: "a", TermsURL: "u", ReviewedAt: "2026-10-21", AllowedUse: "x", RateLimitPerMin: 60}}) //nolint:errcheck
	st.SyncSources(ctx, []store.SourceSpec{{Name: "web", Type: "web_fetch"}})                                                                                      //nolint:errcheck
	src, _ := st.GetSource(ctx, "web")
	d, _ := st.SaveDocument(ctx, src.ID, store.Document{CanonicalURL: "https://acme.example/pricing", MIME: "text/plain", Content: []byte(page)})
	var pid, oid int64
	st.DB.QueryRow(ctx, `INSERT INTO problem (canonical_statement, segment) VALUES ('Jira admins need better issue templates', 'Jira admins') RETURNING id`).Scan(&pid) //nolint:errcheck
	st.DB.QueryRow(ctx, `INSERT INTO opportunity (problem_id, segment, state, budget_units_total) VALUES ($1, 'Jira admins', $2, $3) RETURNING id`,
		pid, state, lifecycle.Budget[state]).Scan(&oid) //nolint:errcheck
	for _, g := range []string{"F1", "F2", "F3", "F4", "M1", "M2", "M3", "M4", "M5", "M6"} {
		status := "UNKNOWN"
		if g == "M1" || g == "M2" {
			status = "PASS"
		}
		st.DB.Exec(ctx, `INSERT INTO gate_status (opportunity_id, gate_code, status) VALUES ($1, $2, $3)`, oid, g, status) //nolint:errcheck
	}
	return &env{st: st, oppID: oid, doc: d.ID}
}

func (e *env) question(t *testing.T, code, kind string) int64 {
	t.Helper()
	var id int64
	if err := e.st.DB.QueryRow(context.Background(), `INSERT INTO question (opportunity_id, text, linked_code, kind, origin, voe_estimate)
		VALUES ($1, $2, $3, $4, 'triage', 0.04) RETURNING id`, e.oppID, "Is there a strong incumbent? ("+code+")", code, kind).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func researchOutput(doc int64, gate, status string, quote string) string {
	q, _ := json.Marshal(quote)
	return fmt.Sprintf(`{"status":"answered","answer":"Acme is a strong, cheap incumbent.",
		"findings":[{"key":"f1","claim":"Acme Templates is rated 4.8 stars from 900 reviews","stance":"contradicts","bears_on":"M4","citations":[{"document_id":%d,"quote":%s}]},
		            {"key":"f2","claim":"An invented claim","stance":"supports","bears_on":"M4","citations":[{"document_id":%d,"quote":"this text is not on the page"}]}],
		"gate_verdict":{"code":%q,"status":%q,"rationale":"well-rated cheap incumbent","finding_keys":["f1","f2"]},
		"follow_up_questions":[{"text":"Does Acme serve Jira Cloud?","linked_code":"M4","kind":"standard","p_kill":0.2,"cost_minutes":10}]}`,
		doc, q, doc, gate, status)
}

func TestResearchVerifiesFindingsAndKillsOnFailedGate(t *testing.T) {
	e := setup(t, "qualified")
	ctx := context.Background()
	qid := e.question(t, "M4", "kill")
	rt := &router{routes: map[string]func(string) string{
		research.TaskType: func(string) string {
			return researchOutput(e.doc, "M4", "FAIL", "It has 4.8 stars from 900 reviews.")
		},
		research.TaskType + ".verify": verifyAll,
	}}
	client := &llm.Client{Runner: rt, Store: e.st}
	r := &research.Researcher{Store: e.st, LLM: client, Verifier: &verify.Verifier{Store: e.st, LLM: client, Model: "sonnet"}, Model: "opus"}
	out, err := r.Run(ctx, nil, qid)
	if err != nil {
		t.Fatal(err)
	}
	if out.Findings != 2 || out.Supported != 1 || out.Rejected != 1 {
		t.Fatalf("findings = %+v (the invented quote must be rejected without a model call)", out)
	}
	if out.GateChanged != "M4:FAIL" || out.State != "killed" {
		t.Fatalf("gate=%s state=%s; a supported FAIL on M4 must kill the opportunity", out.GateChanged, out.State)
	}
	var reason string
	e.st.DB.QueryRow(ctx, `SELECT reason_code FROM decision WHERE subject_id = $1 ORDER BY id DESC LIMIT 1`, e.oppID).Scan(&reason) //nolint:errcheck
	if reason != "incumbent_strong" {
		t.Fatalf("reason = %s", reason)
	}
	var spans json.RawMessage
	e.st.DB.QueryRow(ctx, `SELECT citation_spans FROM finding WHERE verdict = 'supported'`).Scan(&spans) //nolint:errcheck
	if !strings.Contains(string(spans), "4.8 stars") {
		t.Fatalf("spans = %s", spans)
	}
	// Agent tools were allowed; extraction-style toolless calls were not used.
	spec := rt.calls[0]
	if !spec.UseMCP || len(spec.Tools) != 1 || spec.Tools[0] != "WebSearch" {
		t.Fatalf("research spec tools = %+v mcp=%v", spec.Tools, spec.UseMCP)
	}
	var used int
	e.st.DB.QueryRow(ctx, `SELECT budget_units_used FROM opportunity WHERE id = $1`, e.oppID).Scan(&used) //nolint:errcheck
	if used != 1 {
		t.Fatalf("budget used = %d", used)
	}
}

func TestUnbackedGateVerdictIsIgnored(t *testing.T) {
	e := setup(t, "qualified")
	ctx := context.Background()
	qid := e.question(t, "M4", "kill")
	rt := &router{routes: map[string]func(string) string{
		research.TaskType: func(string) string {
			// Only the invented finding backs the FAIL.
			return strings.Replace(researchOutput(e.doc, "M4", "FAIL", "not on the page either"), `"finding_keys":["f1","f2"]`, `"finding_keys":["f2"]`, 1)
		},
		research.TaskType + ".verify": verifyAll,
	}}
	client := &llm.Client{Runner: rt, Store: e.st}
	r := &research.Researcher{Store: e.st, LLM: client, Verifier: &verify.Verifier{Store: e.st, LLM: client}, Model: "opus"}
	out, err := r.Run(ctx, nil, qid)
	if err != nil {
		t.Fatal(err)
	}
	if out.GateChanged != "" || out.State != "qualified" || out.Status != "saturated" { // the unanswered kill question holds it
		t.Fatalf("outcome = %+v; an unverified verdict must not change a gate, and nothing verified isn't an answer", out)
	}
}

func TestQualifiedBecomesDossierWhenKillQuestionsAreAnswered(t *testing.T) {
	e := setup(t, "qualified")
	ctx := context.Background()
	qid := e.question(t, "M4", "kill")
	rt := &router{routes: map[string]func(string) string{
		research.TaskType: func(string) string {
			return researchOutput(e.doc, "M4", "PASS", "Templates can be shared across projects.")
		},
		research.TaskType + ".verify": verifyAll,
	}}
	client := &llm.Client{Runner: rt, Store: e.st}
	r := &research.Researcher{Store: e.st, LLM: client, Verifier: &verify.Verifier{Store: e.st, LLM: client}, Model: "opus"}
	out, err := r.Run(ctx, nil, qid)
	if err != nil {
		t.Fatal(err)
	}
	if out.State != "dossier" || out.FollowUps != 1 {
		t.Fatalf("outcome = %+v", out)
	}
	var budget int
	e.st.DB.QueryRow(ctx, `SELECT budget_units_total FROM opportunity WHERE id = $1`, e.oppID).Scan(&budget) //nolint:errcheck
	if budget != lifecycle.Budget["dossier"] {
		t.Fatalf("dossier budget = %d", budget)
	}
}

func TestSaturationAfterTwoEmptyRounds(t *testing.T) {
	e := setup(t, "qualified")
	ctx := context.Background()
	e.question(t, "M4", "kill") // an open kill check keeps it qualified
	empty := `{"status":"cannot_answer","answer":"nothing found","findings":[],"gate_verdict":{"code":"","status":"NONE","rationale":"","finding_keys":[]},"follow_up_questions":[]}`
	rt := &router{routes: map[string]func(string) string{research.TaskType: func(string) string { return empty }}}
	client := &llm.Client{Runner: rt, Store: e.st}
	r := &research.Researcher{Store: e.st, LLM: client, Verifier: &verify.Verifier{Store: e.st, LLM: client}, Model: "opus"}
	var out research.Outcome
	for i := 0; i < 2; i++ {
		var err error
		if out, err = r.Run(ctx, nil, e.question(t, "price", "standard")); err != nil {
			t.Fatal(err)
		}
	}
	if !out.Saturated {
		t.Fatalf("not saturated after two empty rounds: %+v", out)
	}
	// Saturated opportunities get no more research.
	out, _ = r.Run(ctx, nil, e.question(t, "price", "standard"))
	if out.SkippedReason == "" {
		t.Fatalf("saturated opportunity was researched again: %+v", out)
	}
}

func TestScheduleOnePerOpportunityKillFirstWithinBudget(t *testing.T) {
	e := setup(t, "qualified")
	ctx := context.Background()
	e.question(t, "price", "standard")
	kill := e.question(t, "M4", "kill")
	// A killed opportunity elsewhere must not break scheduling.
	var pid int64
	e.st.DB.QueryRow(ctx, `INSERT INTO problem (canonical_statement, segment) VALUES ('other', 'x') RETURNING id`).Scan(&pid) //nolint:errcheck
	e.st.DB.Exec(ctx, `INSERT INTO opportunity (problem_id, segment, state) VALUES ($1, 'x', 'killed')`, pid)                 //nolint:errcheck
	n, err := research.Schedule(ctx, e.st, research.Options{})
	if err != nil || n != 1 {
		t.Fatalf("scheduled %d err %v", n, err)
	}
	var qid int64
	e.st.DB.QueryRow(ctx, `SELECT (inputs->>'question_id')::bigint FROM task WHERE type = 'research'`).Scan(&qid) //nolint:errcheck
	if qid != kill {
		t.Fatalf("scheduled question %d, want the kill question %d", qid, kill)
	}
	if n, _ := research.Schedule(ctx, e.st, research.Options{}); n != 0 {
		t.Fatal("a second task for the same opportunity while one is in flight")
	}
	e.st.DB.Exec(ctx, `UPDATE task SET state = 'done'`)                                //nolint:errcheck
	e.st.DB.Exec(ctx, `UPDATE opportunity SET budget_units_used = budget_units_total`) //nolint:errcheck
	if n, _ := research.Schedule(ctx, e.st, research.Options{}); n != 0 {
		t.Fatal("scheduled past the earned budget")
	}
}

func TestAssessAndDebate(t *testing.T) {
	e := setup(t, "dossier")
	ctx := context.Background()
	// One supported finding to cite.
	var fid int64
	e.st.DB.QueryRow(ctx, `INSERT INTO question (opportunity_id, text, linked_code, origin) VALUES ($1, 'q', 'price', 'triage') RETURNING id`, e.oppID).Scan(&fid) //nolint:errcheck
	e.st.DB.QueryRow(ctx, `INSERT INTO finding (question_id, opportunity_id, claim, verdict, citation_spans, model, prompt_version, input_hash)
		VALUES ($1, $2, 'Acme charges $10 per month', 'supported', '[]', 'm', 'p', 'h') RETURNING id`, fid, e.oppID).Scan(&fid) //nolint:errcheck

	rt := &router{routes: map[string]func(string) string{
		assess.TaskType: func(prompt string) string {
			if strings.Contains(prompt, "EV") || strings.Contains(prompt, "P50") {
				t.Error("assessor must not see earlier numbers")
			}
			return fmt.Sprintf(`{"factors":[
				{"code":"N_reachable","low":300,"base":600,"high":1200,"refs":["F%d"],"rationale":"marketplace installs"},
				{"code":"price","low":10,"base":20,"high":40,"refs":["F%d"],"rationale":"incumbent price"},
				{"code":"p_pay","low":0.3,"base":0.5,"high":0.6,"refs":["X99"],"rationale":"guess"}],
				"paths":[{"path":"distribution_first","description":"plugin","channel":"marketplace","weeks_to_first_dollar":8,"p_build":0.8},
				         {"path":"service_first","description":"done for you","channel":"outreach","weeks_to_first_dollar":3,"p_build":0.9}],
				"first10":{"primary_channel":"marketplace","backup_channel":"community","prospect_sources":["reviews"],"founder_can_run":"yes","notes":""},
				"next_experiments":[{"hypothesis":"admins pay $20","method":"pre-order page","factors":["p_pay"],"founder_hours":3,"success_threshold":"3 pre-orders","kill_threshold":"0 of 40"}],
				"summary":"A cheap marketplace app."}`, fid, fid)
		},
	}}
	client := &llm.Client{Runner: rt, Store: e.st}
	a := &assess.Assessor{Store: e.st, LLM: client, Model: "opus"}
	out, err := a.Run(ctx, nil, e.oppID)
	if err != nil {
		t.Fatal(err)
	}
	// p_pay cited an unknown reference and every unlisted factor is a prior.
	if out.Clamped < 8 || out.AssessmentID == 0 || !(out.P10 <= out.P50 && out.P50 <= out.P90) {
		t.Fatalf("assessment = %+v", out)
	}
	var lo, hi float64
	var clamped bool
	e.st.DB.QueryRow(ctx, `SELECT low, high, clamped_to_prior FROM factor_estimate WHERE assessment_id = $1 AND factor_code = 'p_pay'`, out.AssessmentID).Scan(&lo, &hi, &clamped) //nolint:errcheck
	if !clamped || lo != 0.01 || hi != 0.2 {
		t.Fatalf("p_pay with a fake citation = [%v, %v] clamped=%v, want the wide prior", lo, hi, clamped)
	}

	// Debate.
	q, _ := json.Marshal("It has 4.8 stars from 900 reviews.")
	argument := func(claim string, quote json.RawMessage) string {
		return fmt.Sprintf(`{"arguments":[{"key":"a","claim":%q,"bears_on":"M4","rebuts":"","citations":[{"document_id":%d,"quote":%s}]},
			{"key":"b","claim":"An uncited flourish","bears_on":"M4","rebuts":"","citations":[{"document_id":%d,"quote":"nowhere on the page"}]}]}`,
			claim, e.doc, quote, e.doc)
	}
	rt.routes[debate.TaskType+".bull"] = func(p string) string {
		if strings.Contains(p, "P50") || strings.Contains(p, "MRR") {
			t.Error("Bull must be blinded to the assessment")
		}
		return argument("Buyers already pay for templates apps", q)
	}
	rt.routes[debate.TaskType+".bear"] = func(string) string { return argument("A 4.8-star incumbent already serves this segment", q) }
	rt.routes[debate.TaskType+".verify"] = verifyAll
	rt.routes[debate.TaskType+".judge"] = func(p string) string {
		if strings.Contains(p, "An uncited flourish") {
			t.Error("struck arguments must not reach the Judge")
		}
		return `{"rulings":[{"claim":"Is the segment already served?","verdict":"bear_holds","rationale":"strong incumbent","argument_keys":["B1.1"]}],
			"objections":[{"text":"A strong incumbent at $10","severity":3,"argument_keys":["B1.1"]},{"text":"unbacked","severity":2,"argument_keys":["U9.9"]}],
			"kill_questions":[{"text":"Does the incumbent lack any feature these admins ask for?","linked_code":"M4","p_kill":0.5,"cost_minutes":20}],
			"summary":"The Bear's incumbent argument holds."}`
	}
	d := &debate.Debater{Store: e.st, LLM: client, Verifier: &verify.Verifier{Store: e.st, LLM: client}, Model: "opus"}
	dout, err := d.Run(ctx, nil, e.oppID)
	if err != nil {
		t.Fatal(err)
	}
	if dout.Arguments != 8 || dout.Struck != 4 || dout.BearHolds != 1 || dout.Objections != 1 || dout.Questions != 1 {
		t.Fatalf("debate = %+v", dout)
	}
	var sat bool
	var total int
	e.st.DB.QueryRow(ctx, `SELECT saturated, budget_units_total FROM opportunity WHERE id = $1`, e.oppID).Scan(&sat, &total) //nolint:errcheck
	if sat || total <= lifecycle.Budget["dossier"] {
		t.Fatalf("new kill questions must reopen research: saturated=%v budget=%d", sat, total)
	}
	// Scheduling: the debated assessment isn't debated again.
	if n, _ := debate.Schedule(ctx, e.st, 2); n != 0 {
		t.Fatalf("re-debated the same assessment: %d", n)
	}
}

func TestLifecycleNext(t *testing.T) {
	cases := []struct {
		state string
		gates map[string]string
		kills int
		want  string
		why   string
	}{
		{"candidate", map[string]string{"M1": "PASS", "M2": "PASS"}, 0, "qualified", ""},
		{"candidate", map[string]string{"M1": "UNKNOWN", "M2": "PASS"}, 0, "candidate", ""},
		{"qualified", map[string]string{"M1": "PASS", "M2": "PASS"}, 2, "qualified", ""},
		{"qualified", map[string]string{"M1": "PASS", "M2": "PASS"}, 0, "dossier", ""},
		{"dossier", map[string]string{"M5": "FAIL"}, 0, "killed", "negative_margin"},
		{"qualified", map[string]string{"M3": "FAIL", "F2": "FAIL"}, 0, "killed", "founder_gate"},
		{"candidate", map[string]string{"M1": "FAIL"}, 0, "parked", "no_buyer"},
		{"killed", map[string]string{"M1": "PASS", "M2": "PASS"}, 0, "killed", ""},
	}
	for _, c := range cases {
		got, why := lifecycle.Next(c.state, c.gates, c.kills)
		if got != c.want || why != c.why {
			t.Errorf("Next(%s, %v, %d) = %s/%s, want %s/%s", c.state, c.gates, c.kills, got, why, c.want, c.why)
		}
	}
}

func TestFailedRunRefundsBudget(t *testing.T) {
	e := setup(t, "qualified")
	ctx := context.Background()
	qid := e.question(t, "M4", "kill")
	rt := &router{routes: map[string]func(string) string{research.TaskType: func(string) string { return `not json` }}}
	client := &llm.Client{Runner: rt, Store: e.st}
	r := &research.Researcher{Store: e.st, LLM: client, Verifier: &verify.Verifier{Store: e.st, LLM: client}, Model: "opus"}
	if _, err := r.Run(ctx, nil, qid); err == nil {
		t.Fatal("want an error for unparseable output")
	}
	var used int
	e.st.DB.QueryRow(ctx, `SELECT budget_units_used FROM opportunity WHERE id = $1`, e.oppID).Scan(&used) //nolint:errcheck
	if used != 0 {
		t.Fatalf("budget used = %d after a failed run, want 0", used)
	}
}

func TestNoResearchOnKilledOpportunities(t *testing.T) {
	e := setup(t, "killed")
	qid := e.question(t, "M4", "kill")
	rt := &router{routes: map[string]func(string) string{}}
	client := &llm.Client{Runner: rt, Store: e.st}
	r := &research.Researcher{Store: e.st, LLM: client, Verifier: &verify.Verifier{Store: e.st, LLM: client}, Model: "opus"}
	out, err := r.Run(context.Background(), nil, qid)
	if err != nil || out.SkippedReason == "" || len(rt.calls) != 0 {
		t.Fatalf("out=%+v err=%v calls=%d", out, err, len(rt.calls))
	}
}

func TestCandidatesResearchQualifyingGatesFirst(t *testing.T) {
	e := setup(t, "candidate")
	ctx := context.Background()
	e.question(t, "M4", "kill")
	m1 := e.question(t, "M1", "standard")
	if _, err := research.Schedule(ctx, e.st, research.Options{}); err != nil {
		t.Fatal(err)
	}
	var qid int64
	e.st.DB.QueryRow(ctx, `SELECT (inputs->>'question_id')::bigint FROM task WHERE type = 'research'`).Scan(&qid) //nolint:errcheck
	if qid != m1 {
		t.Fatalf("candidate researched question %d first, want the M1 question %d", qid, m1)
	}
}

func TestQualifiedOpportunitiesAreAssessedOnce(t *testing.T) {
	e := setup(t, "qualified")
	ctx := context.Background()
	if n, err := assess.Schedule(ctx, e.st, 5); err != nil || n != 1 {
		t.Fatalf("a qualified opportunity without an assessment must be assessed: %d %v", n, err)
	}
	e.st.DB.Exec(ctx, `UPDATE task SET state = 'done'`) //nolint:errcheck
	var aid int64
	e.st.DB.QueryRow(ctx, `INSERT INTO assessment (opportunity_id, model, prompt_version, input_hash) VALUES ($1, 'm', 'p', 'h') RETURNING id`, e.oppID).Scan(&aid) //nolint:errcheck
	e.st.DB.Exec(ctx, `UPDATE opportunity SET current_assessment_id = $2 WHERE id = $1`, e.oppID, aid)                                                              //nolint:errcheck
	if n, _ := assess.Schedule(ctx, e.st, 5); n != 0 {
		t.Fatal("an assessed qualified opportunity waits for dossier before re-assessment")
	}
}
