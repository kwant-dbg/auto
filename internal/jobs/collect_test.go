package jobs_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/kwant-dbg/auto/internal/collect"
	"github.com/kwant-dbg/auto/internal/connectors"
	"github.com/kwant-dbg/auto/internal/jobs"
	"github.com/kwant-dbg/auto/internal/queue"
	"github.com/kwant-dbg/auto/internal/store"
	"github.com/kwant-dbg/auto/internal/testdb"
	"github.com/kwant-dbg/auto/internal/worker"
)

// stubConnector emits n documents per run, numbered from the cursor.
type stubConnector struct {
	n   int
	err error
}

func (s stubConnector) Fetch(_ context.Context, _ connectors.Env, raw json.RawMessage, emit connectors.Emit) (json.RawMessage, error) {
	var start int
	if len(raw) > 0 {
		json.Unmarshal(raw, &start) //nolint:errcheck
	}
	for i := start; i < start+s.n; i++ {
		if err := emit(store.Document{CanonicalURL: fmt.Sprintf("stub://%d", i), MIME: "text/plain", Content: []byte(fmt.Sprint(i))}); err != nil {
			return nil, err
		}
	}
	next, _ := json.Marshal(start + s.n)
	return next, s.err
}

func init() {
	connectors.Register("stub", func(raw json.RawMessage) (connectors.Connector, error) {
		var cfg struct {
			N    int    `json:"n"`
			Fail string `json:"fail"`
		}
		json.Unmarshal(raw, &cfg) //nolint:errcheck
		var err error
		if cfg.Fail != "" {
			err = errors.New(cfg.Fail)
		}
		return stubConnector{n: cfg.N, err: err}, nil
	})
}

func setup(t *testing.T, cfg map[string]any) *store.Store {
	t.Helper()
	st := testdb.New(t)
	ctx := context.Background()
	if err := st.SyncTOS(ctx, []store.TOSEntry{{SourceType: "stub", AccessMethod: "official_api",
		TermsURL: "https://example.com", ReviewedAt: "2026-10-21", AllowedUse: "test", RateLimitPerMin: 6000}}); err != nil {
		t.Fatal(err)
	}
	if err := st.SyncSources(ctx, []store.SourceSpec{{Name: "stub1", Type: "stub", Enabled: true, Cadence: time.Hour, Config: cfg}}); err != nil {
		t.Fatal(err)
	}
	return st
}

func newWorker(st *store.Store) *worker.Worker {
	w := &worker.Worker{Store: st, Concurrency: 1, Lease: time.Minute,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	w.Handle(jobs.TaskCollect, jobs.CollectHandler(st, collect.Options{}))
	return w
}

func TestScheduleAndCollectEndToEnd(t *testing.T) {
	st := setup(t, map[string]any{"n": 3})
	ctx := context.Background()

	n, err := jobs.ScheduleCollections(ctx, st, time.Now())
	if err != nil || n != 1 {
		t.Fatalf("enqueued %d err %v", n, err)
	}
	// Ticking again in the same window does not enqueue a duplicate.
	if n, _ := jobs.ScheduleCollections(ctx, st, time.Now()); n != 0 {
		t.Fatalf("duplicate enqueue: %d", n)
	}

	w := newWorker(st)
	ran, err := w.Once(ctx, []string{jobs.TaskCollect})
	if err != nil || !ran {
		t.Fatalf("ran=%v err=%v", ran, err)
	}
	var docs int
	st.DB.QueryRow(ctx, `SELECT count(*) FROM raw_document`).Scan(&docs) //nolint:errcheck
	if docs != 3 {
		t.Fatalf("raw documents = %d, want 3", docs)
	}
	src, _ := st.GetSource(ctx, "stub1")
	if string(src.Cursor) != "3" || src.LastSuccessAt == nil || time.Until(src.NextRunAt) < 50*time.Minute {
		t.Fatalf("source after run: cursor=%s next=%s", src.Cursor, src.NextRunAt)
	}
	var state string
	st.DB.QueryRow(ctx, `SELECT state FROM task LIMIT 1`).Scan(&state) //nolint:errcheck
	if state != "done" {
		t.Fatalf("task state = %s", state)
	}
	// Not due again until next_run_at.
	if n, _ := jobs.ScheduleCollections(ctx, st, time.Now()); n != 0 {
		t.Fatalf("enqueued before next run: %d", n)
	}
}

func TestFailedCollectionReschedulesSooner(t *testing.T) {
	st := setup(t, map[string]any{"n": 2, "fail": "upstream down"})
	ctx := context.Background()
	jobs.ScheduleCollections(ctx, st, time.Now()) //nolint:errcheck
	if _, err := newWorker(st).Once(ctx, []string{jobs.TaskCollect}); err != nil {
		t.Fatal(err)
	}
	src, _ := st.GetSource(ctx, "stub1")
	if src.ConsecutiveErrors != 1 || src.LastError == nil || string(src.Cursor) != "2" {
		t.Fatalf("after failure: errors=%d cursor=%s", src.ConsecutiveErrors, src.Cursor)
	}
	if d := time.Until(src.NextRunAt); d > 16*time.Minute {
		t.Fatalf("retry in %s, want ≤ 15m", d)
	}
	var docs int
	st.DB.QueryRow(ctx, `SELECT count(*) FROM raw_document`).Scan(&docs) //nolint:errcheck
	if docs != 2 {
		t.Fatalf("documents saved before the failure were lost: %d", docs)
	}
}

func TestDeadTaskUnsticksSource(t *testing.T) {
	st := setup(t, map[string]any{"n": 1})
	ctx := context.Background()
	jobs.ScheduleCollections(ctx, st, time.Now()) //nolint:errcheck
	// Simulate a worker crash: the task dies without recording a run.
	if _, err := st.DB.Exec(ctx, `UPDATE task SET state = 'dead'`); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.ScheduleCollections(ctx, st, time.Now()); err != nil {
		t.Fatal(err)
	}
	src, _ := st.GetSource(ctx, "stub1")
	if src.ConsecutiveErrors != 1 || !src.NextRunAt.After(time.Now()) {
		t.Fatalf("source not rescheduled after a dead task: %+v", src)
	}
}

func TestPauseStopsWorker(t *testing.T) {
	st := setup(t, map[string]any{"n": 1})
	ctx := context.Background()
	jobs.ScheduleCollections(ctx, st, time.Now()) //nolint:errcheck
	st.SetPaused(ctx, true)                       //nolint:errcheck
	ran, err := newWorker(st).Once(ctx, []string{jobs.TaskCollect})
	if err != nil || ran {
		t.Fatalf("paused worker ran a task: ran=%v err=%v", ran, err)
	}
	stats, _ := queue.Stats(ctx, st.DB)
	if len(stats) != 1 || stats[0].State != "queued" {
		t.Fatalf("queue = %+v", stats)
	}
}

func TestCollectionsDoNotOverlap(t *testing.T) {
	st := setup(t, map[string]any{"n": 1})
	ctx := context.Background()
	src, _ := st.GetSource(ctx, "stub1")
	conn, err := st.DB.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	// Hold the source's collection lock as if another run were in progress.
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(7004134, $1)`, int32(src.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := collect.Run(ctx, st, src, collect.Options{}); !errors.Is(err, collect.ErrAlreadyRunning) {
		t.Fatalf("err = %v, want ErrAlreadyRunning", err)
	}
	conn.Exec(ctx, `SELECT pg_advisory_unlock(7004134, $1)`, int32(src.ID)) //nolint:errcheck
	if _, err := collect.Run(ctx, st, src, collect.Options{}); err != nil {
		t.Fatalf("after unlock: %v", err)
	}
}

func TestShutdownRequeuesInterruptedTask(t *testing.T) {
	st := setup(t, map[string]any{"n": 1})
	ctx, cancel := context.WithCancel(context.Background())
	if _, _, err := queue.Enqueue(ctx, st.DB, queue.NewTask{Type: "slow", MaxAttempts: 1}); err != nil {
		t.Fatal(err)
	}
	w := &worker.Worker{Store: st, Concurrency: 1, Lease: time.Minute, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	w.Handle("slow", func(ctx context.Context, _ *queue.Task) (any, error) {
		cancel()
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if _, err := w.Once(ctx, []string{"slow"}); err != nil {
		t.Fatal(err)
	}
	var state string
	var attempts int
	st.DB.QueryRow(context.Background(), `SELECT state, attempts FROM task`).Scan(&state, &attempts) //nolint:errcheck
	if state != "queued" || attempts != 0 {
		t.Fatalf("interrupted task is %s with %d attempts, want queued with 0", state, attempts)
	}
}
