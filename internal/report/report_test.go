package report_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kwant-dbg/auto/internal/feedback"
	"github.com/kwant-dbg/auto/internal/lifecycle"
	"github.com/kwant-dbg/auto/internal/report"
	"github.com/kwant-dbg/auto/internal/store"
	"github.com/kwant-dbg/auto/internal/testdb"
)

const page = `We pay $40 a month for Acme Templates. Templates can't be shared across projects, so we rebuild them by hand. ` +
	`Acme Templates is rated 4.8 stars from 900 reviews.`

type seeded struct {
	st          *store.Store
	opp, doc    int64
	problem     int64
	observation int64
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// seed creates an opportunity with two independent observations quoted from
// one stored snapshot, gates, an assessment and a contradicting finding.
func seed(t *testing.T, st *store.Store, state string) *seeded {
	t.Helper()
	ctx := context.Background()
	must(t, st.SyncTOS(ctx, []store.TOSEntry{{SourceType: "app_marketplace", AccessMethod: "a", TermsURL: "u", ReviewedAt: "2026-10-22", AllowedUse: "x", RateLimitPerMin: 60}}))
	must(t, st.SyncSources(ctx, []store.SourceSpec{{Name: "jira", Type: "app_marketplace", Enabled: true}}))
	src, err := st.GetSource(ctx, "jira")
	must(t, err)
	d, err := st.SaveDocument(ctx, src.ID, store.Document{CanonicalURL: fmt.Sprintf("https://marketplace.example/acme?v=%d", time.Now().UnixNano()), MIME: "text/plain", Content: []byte(page)})
	must(t, err)
	s := &seeded{st: st, doc: d.ID}
	must(t, st.DB.QueryRow(ctx, `INSERT INTO problem (canonical_statement, segment, independent_origin_count, observation_count)
		VALUES ('Jira admins need to share templates across projects but templates are per project, causing rework', 'Jira admins', 2, 2) RETURNING id`).Scan(&s.problem))
	for i, q := range []string{"We pay $40 a month for Acme Templates.", "Templates can't be shared across projects, so we rebuild them by hand."} {
		start := strings.Index(page, q)
		tier := []int{1, 4}[i]
		var oid int64
		must(t, st.DB.QueryRow(ctx, `INSERT INTO observation (raw_document_id, span_start, span_end, quote, category, evidence_tier, actor_segment, job,
			origin_key, verified, model, prompt_version, input_hash) VALUES ($1, $2, $3, $4, 'complaint', $5, 'Jira admins', 'share templates across projects',
			$6, true, 'm', 'p', 'h') RETURNING id`, d.ID, start, start+len(q), q, tier, fmt.Sprint("origin-", i, state)).Scan(&oid))
		if i == 1 {
			s.observation = oid
		}
		_, err := st.DB.Exec(ctx, `INSERT INTO problem_observation (problem_id, observation_id, attach_method) VALUES ($1, $2, 'auto')`, s.problem, oid)
		must(t, err)
	}
	must(t, st.DB.QueryRow(ctx, `INSERT INTO opportunity (problem_id, segment, state, budget_units_total) VALUES ($1, 'Jira admins', $2, $3) RETURNING id`,
		s.problem, state, lifecycle.Budget[state]).Scan(&s.opp))
	for _, g := range []string{"F1", "F2", "F3", "F4", "M1", "M2", "M3", "M4", "M5", "M6"} {
		status := map[string]string{"F1": "SKIPPED", "M1": "PASS", "M2": "PASS"}[g]
		if status == "" {
			status = "UNKNOWN"
		}
		_, err := st.DB.Exec(ctx, `INSERT INTO gate_status (opportunity_id, gate_code, status) VALUES ($1, $2, $3)`, s.opp, g, status)
		must(t, err)
	}
	s.assess(t, 0.42)
	var qid int64
	must(t, st.DB.QueryRow(ctx, `INSERT INTO question (opportunity_id, text, linked_code, origin) VALUES ($1, 'incumbent?', 'M4', 'triage') RETURNING id`, s.opp).Scan(&qid))
	q := "Acme Templates is rated 4.8 stars from 900 reviews."
	start := strings.Index(page, q)
	spans, _ := json.Marshal([]map[string]any{{"raw_document_id": d.ID, "start": start, "end": start + len(q), "quote": q, "url": "https://marketplace.example/acme"}})
	_, err = st.DB.Exec(ctx, `INSERT INTO finding (question_id, opportunity_id, claim, verdict, stance, bears_on, citation_spans, model, prompt_version, input_hash)
		VALUES ($1, $2, 'Acme is a strong incumbent', 'supported', 'contradicts', 'M4', $3, 'm', 'p', 'h')`, qid, s.opp, spans)
	must(t, err)
	return s
}

// assess adds a new current assessment (a material change).
func (s *seeded) assess(t *testing.T, p1k float64) {
	t.Helper()
	var aid int64
	must(t, s.st.DB.QueryRow(context.Background(), `INSERT INTO assessment (opportunity_id, ev_p10, ev_p50, ev_p90, p_ge_1k, p_ge_5k, weakest_factor, paths,
		next_experiments, model, prompt_version, input_hash) VALUES ($1, 0, 1400, 6000, $2, 0.14, 'p_pay',
		'[{"path":"service_first","channel":"outreach","weeks_to_first_dollar":3,"p_build":0.9}]',
		'[{"hypothesis":"admins pay","method":"8 problem interviews (O12, O13).","founder_hours":4,"kill_threshold":"fewer than 2 report spend"}]',
		'm', 'p', 'h') RETURNING id`, s.opp, p1k).Scan(&aid))
	_, err := s.st.DB.Exec(context.Background(), `UPDATE opportunity SET current_assessment_id = $2 WHERE id = $1`, s.opp, aid)
	must(t, err)
}

func TestCardTemplateAndSpanCheck(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	s := seed(t, st, "qualified")
	chk := report.NewChecker(st)
	c, err := report.LoadCard(ctx, chk, s.opp)
	must(t, err)
	md := c.Markdown(report.RenderOptions{})
	for _, want := range []string{
		"share templates across projects — qualified", fmt.Sprintf("card %d", s.opp), "2 independent origins",
		"T1 — “We pay $40 a month for Acme Templates.”", "[snapshot", "Acme is a strong incumbent", "F2–F4 ?",
		"M1 ✓ · M2 ✓", "P50 $1.4k", "P(≥$1k)=0.42", "service-first via outreach (first $ in ~3 wks)",
		"8 problem interviews; kill if fewer than 2 report spend", "marketplace.example, 2026", "~4 h", fmt.Sprintf("aoe decide %d", s.opp),
	} {
		if !strings.Contains(md, want) {
			t.Errorf("card lacks %q:\n%s", want, md)
		}
	}
	if chk.Checked != 3 || chk.Failed != 0 {
		t.Fatalf("checked %d, failed %d", chk.Checked, chk.Failed)
	}
	blind := c.Markdown(report.RenderOptions{Blind: true, Label: "Card A"})
	for _, leak := range []string{"card ", "snapshot", "aoe decide", "qualified", "O12"} {
		if strings.Contains(blind, leak) {
			t.Errorf("blind card leaks %q:\n%s", leak, blind)
		}
	}

	// A quote that no longer matches its snapshot is never shown.
	_, err = st.DB.Exec(ctx, `UPDATE observation SET quote = 'Templates are fine.' WHERE id = $1`, s.observation)
	must(t, err)
	chk = report.NewChecker(st)
	c, err = report.LoadCard(ctx, chk, s.opp)
	must(t, err)
	if chk.Failed != 1 || len(c.Evidence) != 1 || len(c.MoreOrigins) != 1 || strings.Contains(c.Markdown(report.RenderOptions{}), "Templates are fine") {
		t.Fatalf("tampered quote: failed=%d evidence=%d more=%v", chk.Failed, len(c.Evidence), c.MoreOrigins)
	}
}

func addBaseline(t *testing.T, st *store.Store, week string, n int) {
	t.Helper()
	ctx := context.Background()
	var run int64
	must(t, st.DB.QueryRow(ctx, `INSERT INTO baseline_run (week, model, prompt_version) VALUES ($1, 'opus', 'v1') RETURNING id`, week).Scan(&run))
	for i := 0; i < n; i++ {
		card, _ := json.Marshal(report.Card{Segment: "Dental clinics", Job: fmt.Sprint("insurance follow-ups ", i), Problem: "Clinics need to chase claims",
			Gates: map[string]string{"M1": "PASS"}, Evidence: []report.Evidence{{Tier: 4, Quote: "We lose a day a week chasing claims", Source: "reddit.com", URL: "https://reddit.example/x"}}})
		_, err := st.DB.Exec(ctx, `INSERT INTO baseline_card (run_id, card) VALUES ($1, $2)`, run, card)
		must(t, err)
	}
}

func TestWeeklyDigestCycle(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	s := seed(t, st, "qualified")
	killed := seed(t, st, "candidate")
	_, _, err := feedback.Decide(ctx, st, feedback.Decision{OpportunityID: killed.opp, Action: "kill", Reason: "founder_veto"}, time.Now())
	must(t, err)
	now := time.Date(2026, 10, 22, 23, 0, 0, 0, time.UTC)
	addBaseline(t, st, report.ISOWeek(now), 2)

	dir := t.TempDir()
	b := &report.Builder{Store: st, Opt: report.Options{OutDir: dir}, AOEFirst: func() bool { return true }}
	d, err := b.Build(ctx, now, "system", true)
	must(t, err)
	md := d.Markdown
	for _, want := range []string{
		"# Weekly digest — 2026-W43", "1/1 sources healthy", fmt.Sprintf("card %d", s.opp), "Card A", "Card B", "aoe pick",
		"Acme is a strong incumbent", "aoe grade", "`founder_veto`, by you", "## 7. Killed this week",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("digest lacks %q", want)
		}
	}
	// Both pairs use the one AOE opportunity in progress; the AOE card is A.
	if d.Stats.Cards != 1 || d.Stats.Pairs != 1 || d.Stats.SpotChecks < 1 || d.Stats.QuotesChecked == 0 || d.Stats.QuotesFailed != 0 {
		t.Fatalf("stats = %+v", d.Stats)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "digest-2026-W43.md")); err != nil || string(b) != md {
		t.Fatalf("markdown copy: %v", err)
	}
	var pair int64
	var side string
	must(t, st.DB.QueryRow(ctx, `SELECT id, aoe_side FROM card_pair WHERE digest_id = $1`, d.ID).Scan(&pair, &side))
	if side != "A" {
		t.Fatalf("aoe_side = %s", side)
	}
	res, err := feedback.Pick(ctx, st, pair, "a", "more evidence")
	must(t, err)
	if !res.AOEWon {
		t.Fatal("picking the AOE side must count as a win")
	}

	// The same week returns the stored digest.
	again, err := b.Build(ctx, now.Add(time.Hour), "system", true)
	must(t, err)
	if !again.Existing || again.ID != d.ID {
		t.Fatalf("second build = %+v", again)
	}

	// Next week the unchanged card is held back; a new assessment brings it back.
	next, err := b.Build(ctx, now.AddDate(0, 0, 7), "system", true)
	must(t, err)
	if next.Stats.Cards != 0 || !strings.Contains(next.Markdown, "No opportunity met the bar this week. Closest:") {
		t.Fatalf("unchanged card resurfaced:\n%s", next.Markdown)
	}
	s.assess(t, 0.5)
	third, err := b.Build(ctx, now.AddDate(0, 0, 14), "system", true)
	must(t, err)
	if third.Stats.Cards != 1 {
		t.Fatal("a materially changed card must come back")
	}

	ms, err := report.Metrics(ctx, st, now.AddDate(0, 0, 15), 4*7*24*time.Hour)
	must(t, err)
	got := map[string]report.Metric{}
	for _, m := range ms {
		got[m.Code] = m
	}
	if got["A6"].N != 1 || got["A6"].Met == nil || !*got["A6"].Met || got["A9"].N != 3 || got["A1"].Met == nil {
		t.Fatalf("metrics = %+v", got)
	}
	rule, err := report.StopRule(ctx, st)
	must(t, err)
	if !strings.Contains(rule, "week 3 of 8") || !strings.Contains(rule, "Too early") {
		t.Fatalf("stop rule = %q", rule)
	}
}

func TestPreviewWritesNothing(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	seed(t, st, "qualified")
	addBaseline(t, st, "2026-W43", 1)
	d, err := (&report.Builder{Store: st}).Build(ctx, time.Now(), "human", false)
	must(t, err)
	var n int
	must(t, st.DB.QueryRow(ctx, `SELECT (SELECT count(*) FROM digest) + (SELECT count(*) FROM card_pair) + (SELECT count(*) FROM digest_item)`).Scan(&n))
	if n != 0 || d.Stats.Pairs != 1 {
		t.Fatalf("preview wrote %d rows, pairs %d", n, d.Stats.Pairs)
	}
}

func TestFounderDecisions(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	s := seed(t, st, "qualified")
	now := time.Now()

	// An early probe needs ≥ 20 prospects.
	_, _, err := feedback.Decide(ctx, st, feedback.Decision{OpportunityID: s.opp, Action: "advance"}, now)
	if err == nil || !strings.Contains(err.Error(), "≥ 20 prospects (has 0)") {
		t.Fatalf("advance without prospects: %v", err)
	}
	if _, _, err := feedback.Decide(ctx, st, feedback.Decision{OpportunityID: s.opp, Action: "kill", Reason: "meh"}, now); err == nil {
		t.Fatal("kill with an unknown reason must fail")
	}
	if _, _, err := feedback.Decide(ctx, st, feedback.Decision{OpportunityID: s.opp, Action: "park", Reason: "not_now", Wake: []string{"competitor:Acme"}}, now); err == nil {
		t.Fatal("park without a checkable wake condition must fail")
	}
	from, to, err := feedback.Decide(ctx, st, feedback.Decision{OpportunityID: s.opp, Action: "park", Reason: "not_now",
		Wake: []string{"date:2026-01-01", "evidence:+3"}}, now)
	must(t, err)
	if from != "qualified" || to != "parked" {
		t.Fatalf("park: %s → %s", from, to)
	}
	var wake []lifecycle.Wake
	var raw []byte
	must(t, st.DB.QueryRow(ctx, `SELECT wake_conditions FROM decision WHERE subject_id = $1 AND to_state = 'parked'`, s.opp).Scan(&raw))
	must(t, json.Unmarshal(raw, &wake))
	if len(wake) != 2 || wake[1].MinIndependentOrigins != 5 {
		t.Fatalf("wake = %+v", wake)
	}

	// The past date fires: back to candidate with a fresh research unit.
	n, err := lifecycle.WakeDue(ctx, st.DB, now)
	must(t, err)
	var state string
	var total, used int
	must(t, st.DB.QueryRow(ctx, `SELECT state, budget_units_total, budget_units_used FROM opportunity WHERE id = $1`, s.opp).Scan(&state, &total, &used))
	if n != 1 || state != "candidate" || total-used != 1 {
		t.Fatalf("wake: n=%d state=%s budget %d/%d", n, state, used, total)
	}

	// Advancing a candidate qualifies it and adds the kill questions.
	_, to, err = feedback.Decide(ctx, st, feedback.Decision{OpportunityID: s.opp, Action: "advance"}, now)
	must(t, err)
	var kills int
	must(t, st.DB.QueryRow(ctx, `SELECT count(*) FROM question WHERE opportunity_id = $1 AND kind = 'kill'`, s.opp).Scan(&kills))
	if to != "qualified" || kills != len(lifecycle.KillQuestions("x")) {
		t.Fatalf("advance: %s, %d kill questions", to, kills)
	}
	// An override is allowed and recorded.
	_, to, err = feedback.Decide(ctx, st, feedback.Decision{OpportunityID: s.opp, Action: "advance", Override: true, Notes: "warm intro"}, now)
	must(t, err)
	var notes string
	must(t, st.DB.QueryRow(ctx, `SELECT notes FROM decision WHERE subject_id = $1 AND to_state = 'validating'`, s.opp).Scan(&notes))
	if to != "validating" || !strings.Contains(notes, "override: ≥ 20 prospects") {
		t.Fatalf("override: %s %q", to, notes)
	}
}

func TestAnswerUnblocksDossier(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	s := seed(t, st, "qualified")
	var qid int64
	must(t, st.DB.QueryRow(ctx, `INSERT INTO question (opportunity_id, text, linked_code, kind, status, origin)
		VALUES ($1, 'Will your employer object?', 'F2', 'kill', 'needs_human', 'triage') RETURNING id`, s.opp).Scan(&qid))
	state, err := feedback.Answer(ctx, st, qid, "No conflict.")
	must(t, err)
	if state != "dossier" {
		t.Fatalf("after the last kill question: %s", state)
	}
	must(t, feedback.Rate(ctx, st, s.opp, "not", "too small", 2))
	if err := feedback.Grade(ctx, st, 999999, "supported", ""); err == nil {
		t.Fatal("grading a missing finding must fail")
	}
	if _, err := feedback.ReviewMinutes(ctx, st, "", 30); err == nil {
		t.Fatal("review time without a digest must fail")
	}
}

func TestDueAt(t *testing.T) {
	now := time.Date(2026, 10, 22, 18, 0, 0, 0, time.UTC) // Thursday, 2026-W43
	due, err := report.DueAt(report.Options{Weekday: "thursday", Hour: 23}, now)
	must(t, err)
	if !due.Equal(time.Date(2026, 10, 22, 23, 0, 0, 0, time.UTC)) {
		t.Fatalf("due = %v", due)
	}
	due, _ = report.DueAt(report.Options{}, now) // Monday 00:00 by default
	if !due.Equal(time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("default due = %v", due)
	}
	if report.ISOWeek(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) != "2026-W53" {
		t.Fatal("ISO week across the year boundary")
	}
	if g := report.GateLine(map[string]string{"F1": "PASS", "F2": "PASS", "F3": "PASS", "F4": "PASS", "M3": "PASS"}, 63); !strings.Contains(g, "F1–F4 ✓") || !strings.Contains(g, "M3 ✓ (63 prospects)") {
		t.Fatalf("gate line = %s", g)
	}
	if report.Money(297) != "$297" || report.Money(1400) != "$1.4k" || report.Money(12000) != "$12k" {
		t.Fatal("money formatting")
	}
}
