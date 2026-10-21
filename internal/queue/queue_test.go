package queue_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kwant-dbg/auto/internal/queue"
	"github.com/kwant-dbg/auto/internal/testdb"
)

func TestEnqueueDedupe(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()

	id1, created, err := queue.Enqueue(ctx, st.DB, queue.NewTask{Type: "x", DedupeKey: "k1"})
	if err != nil || !created {
		t.Fatalf("first enqueue: id=%d created=%v err=%v", id1, created, err)
	}
	id2, created, err := queue.Enqueue(ctx, st.DB, queue.NewTask{Type: "x", DedupeKey: "k1"})
	if err != nil || created || id2 != id1 {
		t.Fatalf("duplicate enqueue: id=%d created=%v err=%v, want id=%d created=false", id2, created, err, id1)
	}
	// No dedupe key: always inserted.
	for i := 0; i < 2; i++ {
		if _, created, err := queue.Enqueue(ctx, st.DB, queue.NewTask{Type: "x"}); err != nil || !created {
			t.Fatalf("keyless enqueue %d: created=%v err=%v", i, created, err)
		}
	}
}

func TestClaimOrderAndTypes(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	mustEnqueue(t, st.DB, queue.NewTask{Type: "a", Priority: 1})
	hi := mustEnqueue(t, st.DB, queue.NewTask{Type: "a", Priority: 5})
	other := mustEnqueue(t, st.DB, queue.NewTask{Type: "b", Priority: 9})
	future := mustEnqueue(t, st.DB, queue.NewTask{Type: "a", Priority: 99, RunAfter: time.Now().Add(time.Hour)})

	got, err := queue.Claim(ctx, st.DB, []string{"a"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != hi || got.Attempts != 1 {
		t.Fatalf("claimed %+v, want task %d (highest priority runnable of type a)", got, hi)
	}
	got, _ = queue.Claim(ctx, st.DB, []string{"b"}, time.Minute)
	if got == nil || got.ID != other {
		t.Fatalf("claimed %+v, want task %d", got, other)
	}
	got, _ = queue.Claim(ctx, st.DB, nil, time.Minute)
	if got == nil || got.ID == future {
		t.Fatalf("claimed %+v; the future task must not run yet", got)
	}
	if got, _ := queue.Claim(ctx, st.DB, nil, time.Minute); got != nil {
		t.Fatalf("queue should be idle, claimed %+v", got)
	}
}

func TestConcurrentClaimsAreExclusive(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	const n = 40
	for i := 0; i < n; i++ {
		mustEnqueue(t, st.DB, queue.NewTask{Type: "c"})
	}
	var mu sync.Mutex
	seen := map[int64]int{}
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				task, err := queue.Claim(ctx, st.DB, []string{"c"}, time.Minute)
				if err != nil {
					t.Error(err)
					return
				}
				if task == nil {
					return
				}
				mu.Lock()
				seen[task.ID]++
				mu.Unlock()
				if err := queue.Complete(ctx, st.DB, task.ID, map[string]int{"ok": 1}); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	if len(seen) != n {
		t.Fatalf("claimed %d distinct tasks, want %d", len(seen), n)
	}
	for id, c := range seen {
		if c != 1 {
			t.Fatalf("task %d claimed %d times", id, c)
		}
	}
}

func TestFailRetriesThenDeadLetters(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	id := mustEnqueue(t, st.DB, queue.NewTask{Type: "f", MaxAttempts: 2})

	task, _ := queue.Claim(ctx, st.DB, nil, time.Minute)
	dead, err := queue.Fail(ctx, st.DB, task, errors.New("boom 1"))
	if err != nil || dead {
		t.Fatalf("first failure: dead=%v err=%v, want a retry", dead, err)
	}
	// Retry is scheduled after a back-off, so it isn't claimable yet.
	if got, _ := queue.Claim(ctx, st.DB, nil, time.Minute); got != nil {
		t.Fatalf("retry claimed before its back-off: %+v", got)
	}
	if _, err := st.DB.Exec(ctx, `UPDATE task SET run_after = now() WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	task, _ = queue.Claim(ctx, st.DB, nil, time.Minute)
	if task == nil || task.Attempts != 2 {
		t.Fatalf("second claim: %+v", task)
	}
	dead, err = queue.Fail(ctx, st.DB, task, errors.New("boom 2"))
	if err != nil || !dead {
		t.Fatalf("second failure: dead=%v err=%v, want dead", dead, err)
	}
	letters, err := queue.DeadLetters(ctx, st.DB, 5)
	if err != nil || len(letters) != 1 || letters[0].LastError != "boom 2" {
		t.Fatalf("dead letters: %+v err=%v", letters, err)
	}
}

func TestPermanentErrorSkipsRetries(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	mustEnqueue(t, st.DB, queue.NewTask{Type: "p"})
	task, _ := queue.Claim(ctx, st.DB, nil, time.Minute)
	dead, err := queue.Fail(ctx, st.DB, task, queue.Permanent(errors.New("bad input")))
	if err != nil || !dead {
		t.Fatalf("dead=%v err=%v, want dead on first attempt", dead, err)
	}
}

func TestExpiredLeaseIsReclaimedOrReaped(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	retry := mustEnqueue(t, st.DB, queue.NewTask{Type: "l", MaxAttempts: 3})
	once := mustEnqueue(t, st.DB, queue.NewTask{Type: "l1", MaxAttempts: 1})

	for _, typ := range []string{"l", "l1"} {
		if task, _ := queue.Claim(ctx, st.DB, []string{typ}, time.Minute); task == nil {
			t.Fatalf("claim %s", typ)
		}
	}
	// Simulate a crashed worker: leases expire.
	if _, err := st.DB.Exec(ctx, `UPDATE task SET lease_until = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	task, err := queue.Claim(ctx, st.DB, nil, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if task == nil || task.ID != retry || task.Attempts != 2 {
		t.Fatalf("reclaimed %+v, want task %d on attempt 2", task, retry)
	}
	var state string
	if err := st.DB.QueryRow(ctx, `SELECT state FROM task WHERE id = $1`, once).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "dead" {
		t.Fatalf("single-attempt task with expired lease is %q, want dead", state)
	}
}

func TestDeferDoesNotSpendAttempt(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	mustEnqueue(t, st.DB, queue.NewTask{Type: "d"})
	task, _ := queue.Claim(ctx, st.DB, nil, time.Minute)
	if err := queue.Defer(ctx, st.DB, task, time.Now().Add(-time.Second), "cap"); err != nil {
		t.Fatal(err)
	}
	task, _ = queue.Claim(ctx, st.DB, nil, time.Minute)
	if task == nil || task.Attempts != 1 {
		t.Fatalf("after defer: %+v, want attempts=1", task)
	}
}

func TestBackoff(t *testing.T) {
	cases := map[int]time.Duration{1: time.Minute, 2: 4 * time.Minute, 3: 16 * time.Minute, 4: time.Hour, 10: time.Hour}
	for attempt, want := range cases {
		if got := queue.Backoff(attempt); got != want {
			t.Errorf("Backoff(%d) = %s, want %s", attempt, got, want)
		}
	}
}

func mustEnqueue(t *testing.T, db queue.DB, nt queue.NewTask) int64 {
	t.Helper()
	id, _, err := queue.Enqueue(context.Background(), db, nt)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
