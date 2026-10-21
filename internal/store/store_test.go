package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/kwant-dbg/auto/internal/store"
	"github.com/kwant-dbg/auto/internal/testdb"
)

func TestBlobStoreRoundTrip(t *testing.T) {
	b := &store.BlobStore{Root: t.TempDir()}
	hash, rel, err := b.Put([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if hash != store.Hash([]byte("hello")) || rel != store.RelPath(hash) {
		t.Fatalf("hash/rel mismatch: %s %s", hash, rel)
	}
	if _, rel2, _ := b.Put([]byte("hello")); rel2 != rel {
		t.Fatalf("identical content stored at a different path")
	}
	got, err := b.Get(rel)
	if err != nil || string(got) != "hello" {
		t.Fatalf("get: %q %v", got, err)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	st := testdb.New(t)
	applied, err := store.Migrate(context.Background(), st.DB)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 0 {
		t.Fatalf("second migrate applied %v, want nothing", applied)
	}
}

func seedSource(t *testing.T, st *store.Store, enabled bool) store.Source {
	t.Helper()
	ctx := context.Background()
	if err := st.SyncTOS(ctx, []store.TOSEntry{{
		SourceType: "test", AccessMethod: "official_api", TermsURL: "https://example.com/terms",
		ReviewedAt: "2026-10-21", AllowedUse: "research", RateLimitPerMin: 120,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := st.SyncSources(ctx, []store.SourceSpec{{Name: "s1", Type: "test", Enabled: enabled, Cadence: time.Hour}}); err != nil {
		t.Fatal(err)
	}
	src, err := st.GetSource(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	return src
}

func TestEnabledSourceRequiresTOS(t *testing.T) {
	st := testdb.New(t)
	err := st.SyncSources(context.Background(), []store.SourceSpec{{Name: "nope", Type: "unregistered", Enabled: true}})
	if err == nil {
		t.Fatal("enabling a source without a ToS entry must fail")
	}
}

func TestSaveDocumentVersions(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	src := seedSource(t, st, true)

	doc := store.Document{CanonicalURL: "https://example.com/p", MIME: "text/plain", Content: []byte("v1")}
	r1, err := st.SaveDocument(ctx, src.ID, doc)
	if err != nil || !r1.New || r1.Changed {
		t.Fatalf("first save: %+v %v", r1, err)
	}
	r2, err := st.SaveDocument(ctx, src.ID, doc)
	if err != nil || r2.New || r2.ID != r1.ID {
		t.Fatalf("unchanged save: %+v %v, want no-op on %d", r2, err, r1.ID)
	}
	doc.Content = []byte("v2")
	r3, err := st.SaveDocument(ctx, src.ID, doc)
	if err != nil || !r3.New || !r3.Changed {
		t.Fatalf("changed save: %+v %v", r3, err)
	}
	var prev int64
	if err := st.DB.QueryRow(ctx, `SELECT prev_version_id FROM raw_document WHERE id = $1`, r3.ID).Scan(&prev); err != nil {
		t.Fatal(err)
	}
	if prev != r1.ID {
		t.Fatalf("prev_version_id = %d, want %d", prev, r1.ID)
	}
	// Reverting to v1 reuses the original row rather than duplicating it.
	doc.Content = []byte("v1")
	r4, err := st.SaveDocument(ctx, src.ID, doc)
	if err != nil || r4.New || r4.ID != r1.ID {
		t.Fatalf("revert save: %+v %v", r4, err)
	}
}

func TestRecordRunBackoff(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	src := seedSource(t, st, true)

	cursor := json.RawMessage(`{"next_start":"2026-01-02"}`)
	for i := 1; i <= store.EmptyRunsBeforeBackoff; i++ {
		if err := st.RecordRun(ctx, src.ID, store.RunOutcome{Cursor: cursor}); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := st.GetSource(ctx, "s1")
	if got.BackoffFactor != 2 || got.ConsecutiveEmptyRuns != store.EmptyRunsBeforeBackoff {
		t.Fatalf("after %d empty runs: backoff=%d empty=%d, want backoff=2", store.EmptyRunsBeforeBackoff, got.BackoffFactor, got.ConsecutiveEmptyRuns)
	}
	if string(got.Cursor) != `{"next_start": "2026-01-02"}` && string(got.Cursor) != string(cursor) {
		t.Fatalf("cursor = %s", got.Cursor)
	}
	if d := time.Until(got.NextRunAt); d < 110*time.Minute || d > 130*time.Minute {
		t.Fatalf("next run in %s, want ~2h (cadence × backoff)", d)
	}

	if err := st.RecordRun(ctx, src.ID, store.RunOutcome{NewDocuments: 3}); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetSource(ctx, "s1")
	if got.BackoffFactor != 1 || got.ConsecutiveEmptyRuns != 0 || got.LastNewDocumentAt == nil {
		t.Fatalf("after productive run: %+v", got)
	}

	if err := st.RecordRun(ctx, src.ID, store.RunOutcome{Err: errors.New("down")}); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetSource(ctx, "s1")
	if got.ConsecutiveErrors != 1 || got.LastError == nil || *got.LastError != "down" {
		t.Fatalf("after error: %+v", got)
	}
	if d := time.Until(got.NextRunAt); d > 16*time.Minute {
		t.Fatalf("error retry in %s, want ≤ 15m", d)
	}
}

func TestDueSources(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	seedSource(t, st, true)
	due, err := st.DueSources(ctx, time.Now().Add(time.Second))
	if err != nil || len(due) != 1 {
		t.Fatalf("due = %v, err = %v", due, err)
	}
	if due[0].RateLimitPerMin != 120 {
		t.Fatalf("rate limit not joined from ToS entry: %d", due[0].RateLimitPerMin)
	}
	if err := st.SyncSources(ctx, []store.SourceSpec{{Name: "s1", Type: "test", Enabled: false}}); err != nil {
		t.Fatal(err)
	}
	if due, _ := st.DueSources(ctx, time.Now().Add(time.Second)); len(due) != 0 {
		t.Fatalf("disabled source is due: %v", due)
	}
}

func TestPauseAndLedger(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	if p, err := st.Paused(ctx); err != nil || p {
		t.Fatalf("initially paused=%v err=%v", p, err)
	}
	if err := st.SetPaused(ctx, true); err != nil {
		t.Fatal(err)
	}
	if p, _ := st.Paused(ctx); !p {
		t.Fatal("pause did not stick")
	}
	start := time.Now().Add(-time.Minute)
	for _, turns := range []int{3, 4} {
		if err := st.RecordLLMCall(ctx, store.LLMCall{TaskType: "x", Model: "haiku", PromptVersion: "v1",
			NumTurns: turns, Latency: time.Second, Status: "ok", Usage: json.RawMessage(`{"input_tokens":1}`)}); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := st.TurnsSince(ctx, start); err != nil || n != 7 {
		t.Fatalf("turns = %d err = %v, want 7", n, err)
	}
}
