// Command aoe-mcp serves AOE's read-only research tools over MCP (stdio).
// Claude Code starts it for agent tasks via the MCP config the worker writes.
// stdout carries the protocol, so all logging goes to stderr.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kwant-dbg/auto/internal/app"
	"github.com/kwant-dbg/auto/internal/config"
	"github.com/kwant-dbg/auto/internal/store"
	"github.com/kwant-dbg/auto/internal/tools"
)

func main() {
	log := app.Logger()
	slog.SetDefault(log)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(app.ConfigPath())
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	// No migrations or source sync here: the worker owns those. This process
	// only reads and snapshots.
	st, err := store.Open(ctx, cfg.DatabaseURL, cfg.BlobDir)
	if err != nil {
		log.Error("database", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	srv := tools.NewServer(st, cfg.HTTP.UserAgent)
	srv.GitHubToken = os.Getenv("GITHUB_TOKEN")
	if err := srv.MCP().Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		log.Error("mcp server", "err", err)
		os.Exit(1)
	}
}
