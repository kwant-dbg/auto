// Command worker runs queued AOE tasks.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/kwant-dbg/auto/internal/app"
	"github.com/kwant-dbg/auto/internal/claude"
	"github.com/kwant-dbg/auto/internal/jobs"
	"github.com/kwant-dbg/auto/internal/worker"
)

func main() {
	log := app.Logger()
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Refuse to start with credentials that would move Claude calls to API billing.
	if err := claude.CheckEnv(os.Environ()); err != nil {
		log.Error("environment check failed", "err", err)
		os.Exit(1)
	}

	cfg, st, err := app.Open(ctx, app.ConfigPath())
	if err != nil {
		log.Error("startup failed", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	w := &worker.Worker{
		Store:        st,
		Concurrency:  cfg.Worker.Concurrency,
		Lease:        cfg.Worker.Lease,
		PollInterval: cfg.Worker.PollInterval,
		Log:          log,
	}
	w.Handle(jobs.TaskCollect, jobs.CollectHandler(st, app.CollectOptions(cfg)))

	if err := w.Run(ctx); err != nil {
		log.Error("worker failed", "err", err)
		os.Exit(1)
	}
}
