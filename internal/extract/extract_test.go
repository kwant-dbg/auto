package extract_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kwant-dbg/auto/internal/claude"
	"github.com/kwant-dbg/auto/internal/evidence"
	"github.com/kwant-dbg/auto/internal/extract"
	"github.com/kwant-dbg/auto/internal/llm"
	"github.com/kwant-dbg/auto/internal/store"
	"github.com/kwant-dbg/auto/internal/testdb"
	"github.com/kwant-dbg/auto/internal/worker"
)

// fakeRunner returns a canned structured output and records the spec.
type fakeRunner struct {
	output func(prompt string) string
	err    error
	specs  []claude.Spec
}

func (f *fakeRunner) Run(_ context.Context, s claude.Spec) (claude.Result, error) {
	f.specs = append(f.specs, s)
	if f.err != nil {
		return claude.Result{NumTurns: 1}, f.err
	}
	return claude.Result{Structured: json.RawMessage(f.output(s.Prompt)), NumTurns: 2, SessionID: "s", Duration: time.Second}, nil
}

func seed(t *testing.T, st *store.Store, sourceType string, docs ...store.Document) []int64 {
	t.Helper()
	ctx := context.Background()
	if err := st.SyncTOS(ctx, []store.TOSEntry{{SourceType: sourceType, AccessMethod: "official_api", TermsURL: "https://x",
		ReviewedAt: "2026-10-21", AllowedUse: "test", RateLimitPerMin: 60}}); err != nil {
		t.Fatal(err)
	}
	if err := st.SyncSources(ctx, []store.SourceSpec{{Name: sourceType + "-src", Type: sourceType}}); err != nil {
		t.Fatal(err)
	}
	src, _ := st.GetSource(ctx, sourceType+"-src")
	var ids []int64
	for _, d := range docs {
		r, err := st.SaveDocument(ctx, src.ID, d)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.ID)
	}
	return ids
}

func obsJSON(docID int64, quote, category string) string {
	q, _ := json.Marshal(quote)
	return fmt.Sprintf(`{"document_id":%d,"quote":%s,"category":%q,"actor_segment":"Jira admins at software companies",
		"job":"export sprint reports to finance","obstacle":"exports break at month-end","consequence":"hours of re-keying",
		"specificity":4,"spend_hints":{"amount_usd":null,"period":"monthly","unit":null,"headcount":null,"tools":["Excel"]}}`,
		docID, q, category)
}

func TestExtractVerifiesSpansAndAssignsTiers(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	review := `{
  "kind": "review",
  "platform": "atlassian",
  "review": {
    "review": "Exports break every month-end, so we re-key the data into Excel. It said \"done\" but\nthe file was empty.",
    "stars": 2
  }
}`
	ids := seed(t, st, "app_marketplace",
		store.Document{CanonicalURL: "https://m/app#review-1", MIME: "application/json", Content: []byte(review), AuthorHash: "abc123"},
		store.Document{CanonicalURL: "https://m/app2", MIME: "application/json", Content: []byte(`{"kind":"listing","app":{"name":"Nothing here"}}`)},
	)

	fake := &fakeRunner{output: func(prompt string) string {
		if !strings.Contains(prompt, fmt.Sprintf(`<document id="%d" kind="marketplace_review"`, ids[0])) {
			t.Errorf("prompt does not label the review document: %s", prompt)
		}
		return `{"observations":[` + strings.Join([]string{
			obsJSON(ids[0], "Exports break every month-end, so we re-key the data into Excel.", "complaint"),
			obsJSON(ids[0], `It said "done" but the file was empty.`, "complaint"),                           // JSON-escaped in the snapshot
			obsJSON(ids[0], "Exports fail at the end of every month.", "complaint"),                          // paraphrase: rejected
			obsJSON(ids[0], "Exports break every month-end, so we re-key the data into Excel.", "complaint"), // duplicate
			obsJSON(9999, "Exports break every month-end", "complaint"),                                      // unknown document
		}, ",") + `]}`
	}}
	client := &llm.Client{Runner: fake, Store: st, MaxTurnsPerDay: 100}
	ex := &extract.Extractor{Store: st, LLM: client, Model: "haiku"}

	out, err := ex.Run(ctx, nil, extract.Inputs{DocumentIDs: ids, PromptVersion: extract.PromptVersion})
	if err != nil {
		t.Fatal(err)
	}
	if out.Observations != 2 || out.Rejected != 2 || out.Duplicates != 1 || out.Documents != 2 {
		t.Fatalf("outputs = %+v", out)
	}
	spec := fake.specs[0]
	if spec.Model != "haiku" || spec.PromptVersion != "extract/"+extract.PromptVersion || len(spec.Schema) == 0 || len(spec.Tools) != 0 || spec.SystemPrompt == "" {
		t.Fatalf("spec: model=%s prompt=%s schema=%d tools=%v", spec.Model, spec.PromptVersion, len(spec.Schema), spec.Tools)
	}

	// Every stored quote is exactly the snapshot bytes at its span.
	rows, err := st.DB.Query(ctx, `SELECT o.span_start, o.span_end, o.quote, o.evidence_tier, o.origin_key, o.verified, r.blob_path
		FROM observation o JOIN raw_document r ON r.id = o.raw_document_id ORDER BY o.id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var s, e, tier int
		var quote, origin, blob string
		var verified bool
		if err := rows.Scan(&s, &e, &quote, &tier, &origin, &verified, &blob); err != nil {
			t.Fatal(err)
		}
		content, _ := st.Blobs.Get(blob)
		if string(content[s:e]) != quote || !verified {
			t.Errorf("span %d:%d = %q, stored quote %q", s, e, content[s:e], quote)
		}
		if tier != evidence.T4SpecificComplaint || origin != "app_marketplace:author:abc123" {
			t.Errorf("tier=%d origin=%s", tier, origin)
		}
		n++
	}
	if n != 2 {
		t.Fatalf("stored %d observations", n)
	}

	var rejected int
	st.DB.QueryRow(ctx, `SELECT count(*) FROM rejected_extraction`).Scan(&rejected) //nolint:errcheck
	if rejected != 1 {                                                              // the unknown-document quote has no document to attach to
		t.Fatalf("rejected_extraction rows = %d", rejected)
	}
	var done, obsCount int
	st.DB.QueryRow(ctx, `SELECT count(*), sum(observation_count) FROM extraction WHERE status = 'done'`).Scan(&done, &obsCount) //nolint:errcheck
	if done != 2 || obsCount != 2 {
		t.Fatalf("extraction rows done=%d observations=%d", done, obsCount)
	}
	var calls int
	st.DB.QueryRow(ctx, `SELECT count(*) FROM llm_call WHERE status = 'ok' AND task_type = 'extract'`).Scan(&calls) //nolint:errcheck
	if calls != 1 {
		t.Fatalf("llm_call rows = %d", calls)
	}
}

func TestProcurementSpendIsTierOne(t *testing.T) {
	st := testdb.New(t)
	award := `{"Award Amount": 42000, "Awarding Agency": "Department of Examples", "Description": "RECORDS MANAGEMENT AND SCANNING SERVICES"}`
	ids := seed(t, st, "usaspending_awards", store.Document{CanonicalURL: "https://www.usaspending.gov/award/X", MIME: "application/json",
		Content: []byte(award), OrgHint: "Department of Examples"})
	fake := &fakeRunner{output: func(string) string {
		return `{"observations":[` + obsJSON(ids[0], "RECORDS MANAGEMENT AND SCANNING SERVICES", "spend") + `]}`
	}}
	ex := &extract.Extractor{Store: st, LLM: &llm.Client{Runner: fake, Store: st}, Model: "haiku"}
	if _, err := ex.Run(context.Background(), nil, extract.Inputs{DocumentIDs: ids}); err != nil {
		t.Fatal(err)
	}
	var tier int
	var origin string
	st.DB.QueryRow(context.Background(), `SELECT evidence_tier, origin_key FROM observation`).Scan(&tier, &origin) //nolint:errcheck
	if tier != evidence.T1ObservedSpend || origin != "usaspending_awards:org:department of examples" {
		t.Fatalf("tier=%d origin=%s", tier, origin)
	}
}

func TestTurnCapAndRateLimitDefer(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	ids := seed(t, st, "web_fetch", store.Document{CanonicalURL: "https://e.com", MIME: "text/plain", Content: []byte("some text here")})

	// Cap already used up today.
	if err := st.RecordLLMCall(ctx, store.LLMCall{TaskType: "x", Model: "m", PromptVersion: "v", NumTurns: 10, Status: "ok"}); err != nil {
		t.Fatal(err)
	}
	fake := &fakeRunner{output: func(string) string { return `{"observations":[]}` }}
	ex := &extract.Extractor{Store: st, LLM: &llm.Client{Runner: fake, Store: st, MaxTurnsPerDay: 10}, Model: "haiku"}
	_, err := ex.Run(ctx, nil, extract.Inputs{DocumentIDs: ids})
	var de *worker.DeferError
	if !errors.As(err, &de) || !strings.Contains(de.Reason, "daily turn cap") || len(fake.specs) != 0 {
		t.Fatalf("err = %v, calls = %d; want a deferral without calling Claude", err, len(fake.specs))
	}

	rl := &fakeRunner{err: fmt.Errorf("%w: 429", claude.ErrRateLimited)}
	ex = &extract.Extractor{Store: st, LLM: &llm.Client{Runner: rl, Store: st}, Model: "haiku"}
	_, err = ex.Run(ctx, nil, extract.Inputs{DocumentIDs: ids})
	if !errors.As(err, &de) || !strings.Contains(de.Reason, "rate limited") || time.Until(de.Until) < 10*time.Minute {
		t.Fatalf("err = %v; want a rate-limit deferral", err)
	}
	var status string
	st.DB.QueryRow(ctx, `SELECT status FROM llm_call ORDER BY id DESC LIMIT 1`).Scan(&status) //nolint:errcheck
	if status != "error" {
		t.Fatalf("rate-limited call recorded as %q", status)
	}
}

func TestBuildPromptTruncatesLongDocuments(t *testing.T) {
	long := strings.Repeat("é", extract.MaxDocChars) // 2 bytes each
	p := extract.BuildPrompt([]extract.Doc{{ID: 7, SourceType: "web_fetch", CanonicalURL: "https://e", Content: []byte(long)}})
	if !strings.Contains(p, `truncated="true"`) || len(p) > extract.MaxDocChars+500 {
		t.Fatalf("prompt length %d", len(p))
	}
}

func TestReextractionSupersedesOldObservations(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	ids := seed(t, st, "web_fetch", store.Document{CanonicalURL: "https://e.com", MIME: "text/plain",
		Content: []byte("Agencies re-key invoices by hand every week.")})
	// An observation from an older prompt version, already in a problem.
	var oldObs, pid int64
	st.DB.QueryRow(ctx, `INSERT INTO observation (raw_document_id, span_start, span_end, quote, category, evidence_tier,
		actor_segment, job, origin_key, model, prompt_version, input_hash)
		VALUES ($1, 0, 5, 'Agenc', 'complaint', 4, 'a', 'b', 'o', 'm', 'extract/v0', 'h') RETURNING id`, ids[0]).Scan(&oldObs) //nolint:errcheck
	st.DB.QueryRow(ctx, `INSERT INTO problem (canonical_statement, segment, observation_count) VALUES ('x', 'y', 1) RETURNING id`).Scan(&pid) //nolint:errcheck
	st.DB.Exec(ctx, `INSERT INTO problem_observation (problem_id, observation_id, attach_method) VALUES ($1, $2, 'auto')`, pid, oldObs)       //nolint:errcheck

	fake := &fakeRunner{output: func(string) string {
		return `{"observations":[` + obsJSON(ids[0], "Agencies re-key invoices by hand every week.", "complaint") + `]}`
	}}
	ex := &extract.Extractor{Store: st, LLM: &llm.Client{Runner: fake, Store: st}, Model: "haiku"}
	if _, err := ex.Run(ctx, nil, extract.Inputs{DocumentIDs: ids}); err != nil {
		t.Fatal(err)
	}
	var superseded bool
	st.DB.QueryRow(ctx, `SELECT superseded FROM observation WHERE id = $1`, oldObs).Scan(&superseded) //nolint:errcheck
	var members int
	var status string
	st.DB.QueryRow(ctx, `SELECT observation_count, status FROM problem WHERE id = $1`, pid).Scan(&members, &status) //nolint:errcheck
	if !superseded || members != 0 || status != "retired" {
		t.Fatalf("superseded=%v members=%d status=%s", superseded, members, status)
	}
	var current int
	st.DB.QueryRow(ctx, `SELECT count(*) FROM observation WHERE NOT superseded`).Scan(&current) //nolint:errcheck
	if current != 1 {
		t.Fatalf("current observations = %d", current)
	}
}
