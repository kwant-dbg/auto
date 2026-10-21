// Command scheduler enqueues scheduled AOE work. It regenerates the queue
// from state on every tick; it never runs tasks itself.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kwant-dbg/auto/internal/app"
	"github.com/kwant-dbg/auto/internal/jobs"
	"github.com/kwant-dbg/auto/internal/store"
)

func main() {
	log := app.Logger()
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, st, err := app.Open(ctx, app.ConfigPath())
	if err != nil {
		log.Error("startup failed", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	log.Info("scheduler started", "tick", cfg.Scheduler.Tick)
	tick := time.NewTicker(cfg.Scheduler.Tick)
	defer tick.Stop()
	for {
		runTick(ctx, st, log)
		select {
		case <-ctx.Done():
			log.Info("scheduler stopped")
			return
		case <-tick.C:
		}
	}
}

func runTick(ctx context.Context, st *store.Store, log *slog.Logger) {
	paused, err := st.Paused(ctx)
	if err != nil {
		log.Error("read pause flag", "err", err)
		return
	}
	if paused {
		return
	}
	n, err := jobs.ScheduleCollections(ctx, st, time.Now())
	if err != nil {
		log.Error("schedule collections", "err", err)
		return
	}
	if n > 0 {
		log.Info("collections enqueued", "count", n)
	}
}
