// Package worker runs queued tasks with a fixed pool of goroutines. Task
// types map to handlers; there are no per-role processes (docs/04 §1).
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/kwant-dbg/auto/internal/queue"
	"github.com/kwant-dbg/auto/internal/store"
)

// Handler executes one task and returns its outputs.
type Handler func(ctx context.Context, t *queue.Task) (any, error)

// DeferError asks the worker to re-queue the task at Until without spending
// an attempt (e.g. a capacity cap or a rate limit).
type DeferError struct {
	Until  time.Time
	Reason string
}

func (d *DeferError) Error() string { return "deferred: " + d.Reason }

// Worker pulls tasks of its registered types.
type Worker struct {
	Store        *store.Store
	Concurrency  int
	Lease        time.Duration
	PollInterval time.Duration
	Log          *slog.Logger

	mu       sync.RWMutex
	handlers map[string]Handler
}

// Handle registers a handler for a task type.
func (w *Worker) Handle(taskType string, h Handler) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.handlers == nil {
		w.handlers = map[string]Handler{}
	}
	w.handlers[taskType] = h
}

func (w *Worker) types() []string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	out := make([]string, 0, len(w.handlers))
	for t := range w.handlers {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// Run processes tasks until ctx is cancelled, then waits for in-flight tasks.
func (w *Worker) Run(ctx context.Context) error {
	if w.Concurrency <= 0 {
		w.Concurrency = 1
	}
	if w.Lease <= 0 {
		w.Lease = 10 * time.Minute
	}
	if w.PollInterval <= 0 {
		w.PollInterval = 5 * time.Second
	}
	if w.Log == nil {
		w.Log = slog.Default()
	}
	types := w.types()
	if len(types) == 0 {
		return errors.New("worker: no handlers registered")
	}
	w.Log.Info("worker started", "concurrency", w.Concurrency, "types", types)

	var wg sync.WaitGroup
	for i := 0; i < w.Concurrency; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			w.loop(ctx, slot, types)
		}(i)
	}
	wg.Wait()
	w.Log.Info("worker stopped")
	return nil
}

func (w *Worker) loop(ctx context.Context, slot int, types []string) {
	for ctx.Err() == nil {
		ran, err := w.Once(ctx, types)
		if err != nil && ctx.Err() == nil {
			w.Log.Error("worker iteration failed", "slot", slot, "err", err)
		}
		if ran {
			continue
		}
		select {
		case <-ctx.Done():
		case <-time.After(w.PollInterval):
		}
	}
}

// Once claims and runs at most one task. It reports whether a task ran.
func (w *Worker) Once(ctx context.Context, types []string) (bool, error) {
	paused, err := w.Store.Paused(ctx)
	if err != nil {
		return false, err
	}
	if paused {
		return false, nil
	}
	t, err := queue.Claim(ctx, w.Store.DB, types, w.Lease)
	if err != nil || t == nil {
		return false, err
	}
	w.mu.RLock()
	h := w.handlers[t.Type]
	w.mu.RUnlock()

	log := w.Log.With("task", t.ID, "type", t.Type, "attempt", t.Attempts)
	log.Info("task started")

	// Keep the lease alive while the handler runs.
	hbCtx, stopHB := context.WithCancel(ctx)
	go func() {
		tick := time.NewTicker(w.Lease / 3)
		defer tick.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-tick.C:
				if err := queue.Heartbeat(hbCtx, w.Store.DB, t.ID, w.Lease); err != nil && hbCtx.Err() == nil {
					log.Warn("heartbeat failed", "err", err)
				}
			}
		}
	}()

	out, runErr := runSafely(ctx, h, t)
	stopHB()

	// Finish bookkeeping even if shutdown began mid-task.
	bg := context.WithoutCancel(ctx)
	var deferErr *DeferError
	switch {
	case runErr == nil:
		log.Info("task done")
		return true, queue.Complete(bg, w.Store.DB, t.ID, out)
	case errors.As(runErr, &deferErr):
		log.Info("task deferred", "until", deferErr.Until, "reason", deferErr.Reason)
		return true, queue.Defer(bg, w.Store.DB, t, deferErr.Until, deferErr.Reason)
	case ctx.Err() != nil && errors.Is(runErr, context.Canceled):
		// Shutdown interrupted the task: put it back without spending an attempt.
		log.Info("task interrupted by shutdown; re-queued")
		return true, queue.Defer(bg, w.Store.DB, t, time.Now(), "interrupted by worker shutdown")
	default:
		dead, err := queue.Fail(bg, w.Store.DB, t, runErr)
		log.Warn("task failed", "err", runErr, "dead", dead)
		return true, err
	}
}

func runSafely(ctx context.Context, h Handler, t *queue.Task) (out any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = queue.Permanent(fmt.Errorf("panic: %v", r))
		}
	}()
	return h(ctx, t)
}
