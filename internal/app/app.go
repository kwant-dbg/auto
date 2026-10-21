// Package app holds start-up wiring shared by the aoe, worker and scheduler
// binaries.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/kwant-dbg/auto/internal/collect"
	"github.com/kwant-dbg/auto/internal/config"
	"github.com/kwant-dbg/auto/internal/store"

	// Register connectors.
	_ "github.com/kwant-dbg/auto/internal/connectors/marketplace"
	_ "github.com/kwant-dbg/auto/internal/connectors/usaspending"
)

// ConfigPath returns the config path from AOE_CONFIG or the default.
func ConfigPath() string {
	if p := os.Getenv("AOE_CONFIG"); p != "" {
		return p
	}
	return "config/aoe.yaml"
}

// Open loads config, connects, applies migrations and syncs the ToS register
// and sources from config.
func Open(ctx context.Context, path string) (*config.Config, *store.Store, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, nil, err
	}
	st, err := store.Open(ctx, cfg.DatabaseURL, cfg.BlobDir)
	if err != nil {
		return nil, nil, err
	}
	applied, err := store.Migrate(ctx, st.DB)
	if err != nil {
		st.Close()
		return nil, nil, err
	}
	for _, m := range applied {
		slog.Info("migration applied", "name", m)
	}
	if err := st.SyncTOS(ctx, cfg.TOS); err != nil {
		st.Close()
		return nil, nil, fmt.Errorf("sync tos register: %w", err)
	}
	if err := st.SyncSources(ctx, cfg.Sources); err != nil {
		st.Close()
		return nil, nil, fmt.Errorf("sync sources: %w", err)
	}
	return cfg, st, nil
}

// CollectOptions builds connector HTTP options from config.
func CollectOptions(cfg *config.Config) collect.Options {
	return collect.Options{
		HTTP:      &http.Client{Timeout: cfg.HTTP.Timeout},
		UserAgent: cfg.HTTP.UserAgent,
	}
}

// Logger returns the JSON logger used by long-running services.
func Logger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
}
