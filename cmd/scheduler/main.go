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
	"github.com/kwant-dbg/auto/internal/assess"
	"github.com/kwant-dbg/auto/internal/config"
	"github.com/kwant-dbg/auto/internal/debate"
	"github.com/kwant-dbg/auto/internal/embed"
	"github.com/kwant-dbg/auto/internal/jobs"
	"github.com/kwant-dbg/auto/internal/problems"
	"github.com/kwant-dbg/auto/internal/research"
	"github.com/kwant-dbg/auto/internal/store"
	"github.com/kwant-dbg/auto/internal/triage"
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
		runTick(ctx, cfg, st, log)
		select {
		case <-ctx.Done():
			log.Info("scheduler stopped")
			return
		case <-tick.C:
		}
	}
}

func runTick(ctx context.Context, cfg *config.Config, st *store.Store, log *slog.Logger) {
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
	if cfg.Extraction.On() {
		n, err := jobs.ScheduleExtraction(ctx, st, app.ExtractOptions(cfg))
		if err != nil {
			log.Error("schedule extraction", "err", err)
		} else if n > 0 {
			log.Info("extraction batches enqueued", "count", n)
		}
	}
	if cfg.Embeddings.Enabled() {
		if _, err := embed.Schedule(ctx, st); err != nil {
			log.Error("schedule embedding", "err", err)
		}
	}
	if _, err := problems.Schedule(ctx, st, cfg.Clustering.UseVectors); err != nil {
		log.Error("schedule clustering", "err", err)
	}
	if _, err := problems.ScheduleConsolidation(ctx, st); err != nil {
		log.Error("schedule consolidation", "err", err)
	}
	// Triage waits until clustering and consolidation have caught up, so
	// fragments of one problem don't become separate opportunities.
	if settled, err := problems.Settled(ctx, st); err != nil {
		log.Error("check clustering settled", "err", err)
	} else if settled {
		if n, err := triage.Schedule(ctx, st, cfg.Triage); err != nil {
			log.Error("schedule triage", "err", err)
		} else if n > 0 {
			log.Info("triage enqueued", "count", n)
		}
	}
	if n, err := research.Schedule(ctx, st, cfg.Research); err != nil {
		log.Error("schedule research", "err", err)
	} else if n > 0 {
		log.Info("research enqueued", "count", n)
	}
	if _, err := assess.Schedule(ctx, st, 5); err != nil {
		log.Error("schedule assessment", "err", err)
	}
	if _, err := debate.Schedule(ctx, st, 2); err != nil {
		log.Error("schedule debate", "err", err)
	}
}
