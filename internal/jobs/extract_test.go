package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/kwant-dbg/auto/internal/extract"
	"github.com/kwant-dbg/auto/internal/store"
	"github.com/kwant-dbg/auto/internal/testdb"
)

func TestBatches(t *testing.T) {
	c := func(id, src int64, size int) candidate { return candidate{id: id, sourceID: src, size: size} }
	got := Batches([]candidate{c(1, 1, 10), c(2, 1, 10), c(3, 1, 10), c(4, 2, 10), c(5, 2, 100), c(6, 2, 5)}, 2, 50)
	want := [][]int64{{1, 2}, {3}, {4}, {5}, {6}}
	if len(got) != len(want) {
		t.Fatalf("got %d batches: %v", len(got), got)
	}
	for i, b := range got {
		for j, cand := range b {
			if cand.id != want[i][j] {
				t.Fatalf("batch %d = %v, want %v", i, b, want[i])
			}
		}
	}
}

func seedDocs(t *testing.T, st *store.Store, n int) {
	t.Helper()
	ctx := context.Background()
	st.SyncTOS(ctx, []store.TOSEntry{{SourceType: "t", AccessMethod: "a", TermsURL: "u", ReviewedAt: "2026-10-21", AllowedUse: "x", RateLimitPerMin: 1}}) //nolint:errcheck
	st.SyncSources(ctx, []store.SourceSpec{{Name: "s", Type: "t"}})                                                                                       //nolint:errcheck
	src, _ := st.GetSource(ctx, "s")
	for i := 0; i < n; i++ {
		if _, err := st.SaveDocument(ctx, src.ID, store.Document{CanonicalURL: fmt.Sprint("u", i), MIME: "text/plain", Content: []byte(fmt.Sprint("doc ", i))}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestScheduleExtraction(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	seedDocs(t, st, 7)
	opt := ExtractOptions{MaxQueuedTasks: 2, MaxDocsPerBatch: 3}

	n, err := ScheduleExtraction(ctx, st, opt)
	if err != nil || n != 2 {
		t.Fatalf("enqueued %d err %v, want 2 (backpressure)", n, err)
	}
	var queued int
	st.DB.QueryRow(ctx, `SELECT count(*) FROM extraction WHERE status = 'queued'`).Scan(&queued) //nolint:errcheck
	if queued != 6 {
		t.Fatalf("marked %d documents queued, want 6", queued)
	}
	// Backpressure: both slots are taken.
	if n, _ := ScheduleExtraction(ctx, st, opt); n != 0 {
		t.Fatalf("enqueued %d while at capacity", n)
	}

	// Finish one task; the remaining document is batched next, never a queued one again.
	st.DB.Exec(ctx, `UPDATE task SET state = 'done' WHERE id = (SELECT min(id) FROM task)`) //nolint:errcheck
	n, _ = ScheduleExtraction(ctx, st, opt)
	if n != 1 {
		t.Fatalf("enqueued %d, want 1", n)
	}
	var inputs json.RawMessage
	st.DB.QueryRow(ctx, `SELECT inputs FROM task ORDER BY id DESC LIMIT 1`).Scan(&inputs) //nolint:errcheck
	var in extract.Inputs
	json.Unmarshal(inputs, &in) //nolint:errcheck
	if len(in.DocumentIDs) != 1 || in.PromptVersion != extract.PromptVersion {
		t.Fatalf("last batch = %+v", in)
	}
}

func TestDeadExtractionTasksRetryThenFail(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	seedDocs(t, st, 1)
	opt := ExtractOptions{MaxQueuedTasks: 5, MaxAttempts: 2}
	for attempt := 1; attempt <= 2; attempt++ {
		if n, err := ScheduleExtraction(ctx, st, opt); err != nil || n != 1 {
			t.Fatalf("attempt %d: enqueued %d err %v", attempt, n, err)
		}
		st.DB.Exec(ctx, `UPDATE task SET state = 'dead'`) //nolint:errcheck
	}
	if n, _ := ScheduleExtraction(ctx, st, opt); n != 0 {
		t.Fatalf("re-enqueued after attempts were spent: %d", n)
	}
	var status string
	var attempts int
	st.DB.QueryRow(ctx, `SELECT status, attempts FROM extraction`).Scan(&status, &attempts) //nolint:errcheck
	if status != "failed" || attempts != 2 {
		t.Fatalf("extraction = %s after %d attempts", status, attempts)
	}
}
