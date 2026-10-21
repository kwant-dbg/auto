// Command aoe is the operator CLI for the Autonomous Opportunity Engine.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/kwant-dbg/auto/internal/app"
	"github.com/kwant-dbg/auto/internal/collect"
	"github.com/kwant-dbg/auto/internal/config"
	"github.com/kwant-dbg/auto/internal/labour"
	"github.com/kwant-dbg/auto/internal/queue"
	"github.com/kwant-dbg/auto/internal/store"
)

const usage = `aoe — Autonomous Opportunity Engine

Usage: aoe <command> [flags]

Setup
  migrate                         apply migrations and sync the ToS register and sources
  claude check [--probe-websearch] verify Claude Code version, subscription auth and WebSearch

Sources
  sources                         list sources with health
  collect <source>                run one source now (outside the queue)

Labour-market data (discovery lens 1)
  import onet --release R <dir|zip>              O*NET text database
  import oews --year Y <file.xlsx|csv>           BLS OEWS national or national-by-industry file
  import ai-usage --release R --task-col C --value-col C [--metric M] <file.csv>
  seeds [--limit N] [--naics PREFIX] [--cross-industry]

Evidence
  extract [--dry-run] [--source S] [--limit N]   extract one batch now (--dry-run prints the prompt only)
  observations [--limit N] [--tier T]           recent verified observations
  kb <query>                                    search collected evidence (the agents' kb_search)
  fetch <url>                                   snapshot a public page (the agents' fetch_and_snapshot)
  hn <query>                                    search Hacker News comments and snapshot hits (the agents' search_hn)

Problems and opportunities
  cluster                                       group unassigned observations into problems now
  consolidate [--all]                           merge duplicate problems now (--all re-checks every problem)
  problems [--min-origins N] [--limit N]        problems ranked by independent evidence
  problem <id>                                  one problem and its evidence
  triage [--problem ID] [--limit N]             triage eligible problems now (or one problem)
  opportunities [--state S]                     opportunities with gates and open questions
  opportunity <id>                              one opportunity in full

Operations
  stats                           pipeline health: documents, extraction, rejection rate, observations by tier
  tasks                           queue summary and recent dead letters
  pause | resume                  kill switch for all admission and actions

Config: $AOE_CONFIG or config/aoe.yaml. Database: database_url or $AOE_DATABASE_URL.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "aoe:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(usage)
		return nil
	}
	cmd, rest := args[0], args[1:]

	if cmd == "claude" {
		return claudeCmd(ctx, rest)
	}

	cfg, st, err := app.Open(ctx, app.ConfigPath())
	if err != nil {
		return err
	}
	defer st.Close()

	switch cmd {
	case "migrate":
		fmt.Println("migrations applied; ToS register and sources synced")
		return nil
	case "sources":
		return sourcesCmd(ctx, st)
	case "collect":
		return collectCmd(ctx, cfg, st, rest)
	case "import":
		return importCmd(ctx, st, rest)
	case "seeds":
		return seedsCmd(ctx, st, rest)
	case "tasks":
		return tasksCmd(ctx, st)
	case "stats":
		return statsCmd(ctx, st)
	case "extract":
		return extractCmd(ctx, cfg, st, rest)
	case "observations":
		return observationsCmd(ctx, st, rest)
	case "kb":
		return kbCmd(ctx, cfg, st, rest)
	case "fetch":
		return fetchCmd(ctx, cfg, st, rest)
	case "hn":
		return hnCmd(ctx, cfg, st, rest)
	case "cluster":
		return clusterCmd(ctx, cfg, st)
	case "consolidate":
		return consolidateCmd(ctx, cfg, st, rest)
	case "problems":
		return problemsCmd(ctx, st, rest)
	case "problem":
		return problemCmd(ctx, st, rest)
	case "triage":
		return triageCmd(ctx, cfg, st, rest)
	case "opportunities":
		return opportunitiesCmd(ctx, st, rest)
	case "opportunity":
		return opportunityCmd(ctx, st, rest)
	case "pause", "resume":
		if err := st.SetPaused(ctx, cmd == "pause"); err != nil {
			return err
		}
		fmt.Printf("%sd\n", cmd)
		return nil
	default:
		return fmt.Errorf("unknown command %q (run `aoe help`)", cmd)
	}
}

func claudeCmd(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "check" {
		return errors.New("usage: aoe claude check [--probe-websearch]")
	}
	fs := flag.NewFlagSet("claude check", flag.ContinueOnError)
	probe := fs.Bool("probe-websearch", false, "run one headless WebSearch call (uses the subscription)")
	probeTools := fs.Bool("probe-tools", false, "run one headless call to AOE's kb_search tool via aoe-mcp (uses the subscription)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	cfg, st, err := app.Open(ctx, app.ConfigPath())
	if err != nil {
		return err
	}
	defer st.Close()
	_, r, err := app.NewLLM(ctx, cfg, st, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if r == nil {
		return err
	}
	h, err := r.Check(ctx)
	fmt.Printf("claude code version: %s (pin: %s)\nauth method: %s\nlogged in: %v\n",
		h.Version, orNone(cfg.Claude.VersionPin), orNone(h.AuthMethod), h.LoggedIn)
	if err != nil {
		return err
	}
	if *probeTools {
		ok, hits, res, err := r.ProbeTools(ctx, cfg.Models.Fast, "invoice")
		fmt.Printf("tools probe: kb_search worked=%v hits=%d turns=%d duration=%s\n", ok, hits, res.NumTurns, res.Duration.Round(time.Millisecond))
		if err != nil {
			return fmt.Errorf("tools probe failed: %w", err)
		}
		if !ok {
			return errors.New("kb_search was not reachable through aoe-mcp")
		}
	}
	if !*probe {
		if !*probeTools {
			fmt.Println("ok (add --probe-websearch or --probe-tools for live checks)")
		}
		return nil
	}
	ok, res, err := r.ProbeWebSearch(ctx, cfg.Models.Fast)
	fmt.Printf("websearch probe: worked=%v turns=%d est_cost_usd=%.4f duration=%s\n",
		ok, res.NumTurns, res.CostUSD, res.Duration.Round(time.Millisecond))
	if err != nil {
		return fmt.Errorf("websearch probe failed: %w", err)
	}
	if !ok {
		return errors.New("WebSearch is not available headless under this auth; configure the paid search fallback")
	}
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func sourcesCmd(ctx context.Context, st *store.Store) error {
	srcs, err := st.ListSources(ctx)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tTYPE\tENABLED\tCADENCE\tBACKOFF\tNEXT RUN\tLAST SUCCESS\tERRORS\tLAST ERROR")
	for _, s := range srcs {
		lastErr := ""
		if s.LastError != nil {
			lastErr = truncate(*s.LastError, 60)
		}
		fmt.Fprintf(tw, "%s\t%s\t%v\t%s\t×%d\t%s\t%s\t%d\t%s\n", s.Name, s.Type, s.Enabled, s.Cadence,
			s.BackoffFactor, s.NextRunAt.Format(time.RFC3339), fmtTime(s.LastSuccessAt), s.ConsecutiveErrors, lastErr)
	}
	return tw.Flush()
}

func collectCmd(ctx context.Context, cfg *config.Config, st *store.Store, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: aoe collect <source>")
	}
	src, err := st.GetSource(ctx, args[0])
	if err != nil {
		return err
	}
	if !src.Enabled {
		return fmt.Errorf("source %q is disabled; enable it in the config after reviewing its ToS entry", src.Name)
	}
	stats, err := collect.Run(ctx, st, src, app.CollectOptions(cfg))
	fmt.Printf("%s: emitted=%d new=%d changed=%d requests=%d elapsed=%s\n", stats.Source, stats.Emitted,
		stats.New, stats.Changed, stats.Requests, time.Duration(stats.ElapsedMS)*time.Millisecond)
	return err
}

func importCmd(ctx context.Context, st *store.Store, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: aoe import onet|oews|ai-usage …")
	}
	kind, args := args[0], args[1:]
	fs := flag.NewFlagSet("import "+kind, flag.ContinueOnError)
	release := fs.String("release", "", "dataset release (e.g. O*NET 29.3, or an AI-usage release date)")
	year := fs.Int("year", 0, "OEWS survey year (e.g. 2024)")
	taskCol := fs.String("task-col", "", "AI usage: task text column")
	valueCol := fs.String("value-col", "", "AI usage: value column")
	metric := fs.String("metric", "share", "AI usage: name of the metric being imported")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("import %s: exactly one path is required", kind)
	}
	path := fs.Arg(0)

	switch kind {
	case "onet":
		if *release == "" {
			return errors.New("import onet: --release is required")
		}
		files, err := labour.LoadONet(path)
		if err != nil {
			return err
		}
		s, err := labour.ImportONet(ctx, st.DB, files, *release)
		if err != nil {
			return err
		}
		fmt.Printf("O*NET %s: %d occupations, %d tasks, %d ratings\n", *release, s.Occupations, s.Tasks, s.Ratings)
	case "oews":
		if *year == 0 {
			return errors.New("import oews: --year is required")
		}
		t, err := labour.OpenTable(path)
		if err != nil {
			return err
		}
		s, err := labour.ImportOEWS(ctx, st.DB, t, *year)
		if err != nil {
			return err
		}
		fmt.Printf("OEWS %d: %d rows (%d skipped)\n", *year, s.Rows, s.Skipped)
	case "ai-usage":
		if *release == "" || *taskCol == "" || *valueCol == "" {
			return errors.New("import ai-usage: --release, --task-col and --value-col are required")
		}
		t, err := labour.OpenTable(path)
		if err != nil {
			return err
		}
		s, err := labour.ImportAIUsage(ctx, st.DB, t, *release, *taskCol, *valueCol, *metric)
		if err != nil {
			return err
		}
		fmt.Printf("AI usage %s: %d tasks, %d matched to O*NET\n", *release, s.Rows, s.Matched)
	default:
		return fmt.Errorf("unknown import kind %q", kind)
	}
	return nil
}

func seedsCmd(ctx context.Context, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("seeds", flag.ContinueOnError)
	limit := fs.Int("limit", 50, "number of seeds")
	naics := fs.String("naics", "", "NAICS prefix filter (e.g. 54 for professional services)")
	cross := fs.Bool("cross-industry", false, "use all-industry OEWS rows instead of industry-specific ones")
	if err := fs.Parse(args); err != nil {
		return err
	}
	seeds, err := labour.Seeds(ctx, st.DB, *limit, *cross, *naics)
	if err != nil {
		return err
	}
	if len(seeds) == 0 {
		fmt.Println("no seeds: import O*NET and OEWS first (industry-specific OEWS rows unless --cross-industry)")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "EST $/YR\tSHARE\tEMPLOYMENT\tMEDIAN\tAI USAGE\tINDUSTRY\tOCCUPATION\tTASK")
	for _, s := range seeds {
		usage := "-"
		if s.AIUsage != nil {
			usage = fmt.Sprintf("%.4g", *s.AIUsage)
		}
		fmt.Fprintf(tw, "%s\t%.3f\t%.0f\t%.0f\t%s\t%s %s\t%s\t%s\n", money(s.EstSpend), s.Share, s.Employment,
			s.MedianWage, usage, s.NAICS, truncate(s.NAICSTitle, 30), truncate(s.Occupation, 30), truncate(s.Task, 90))
	}
	fmt.Fprintln(tw, "\nEstimates rank where to look; they are hypothesis seeds, not evidence of demand.")
	return tw.Flush()
}

func tasksCmd(ctx context.Context, st *store.Store) error {
	stats, err := queue.Stats(ctx, st.DB)
	if err != nil {
		return err
	}
	paused, err := st.Paused(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("paused: %v\n\n", paused)
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "TYPE\tSTATE\tCOUNT")
	for _, s := range stats {
		fmt.Fprintf(tw, "%s\t%s\t%d\n", s.Type, s.State, s.Count)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	dead, err := queue.DeadLetters(ctx, st.DB, 10)
	if err != nil {
		return err
	}
	if len(dead) == 0 {
		return nil
	}
	fmt.Println("\nrecent dead letters:")
	for _, d := range dead {
		fmt.Printf("  #%d %s (attempts %d, %s): %s\n", d.ID, d.Type, d.Attempts, d.FinishedAt.Format(time.RFC3339), truncate(d.LastError, 200))
	}
	return nil
}

func fmtTime(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return t.Format(time.RFC3339)
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func money(v float64) string {
	switch {
	case v >= 1e9:
		return fmt.Sprintf("$%.1fB", v/1e9)
	case v >= 1e6:
		return fmt.Sprintf("$%.1fM", v/1e6)
	case v >= 1e3:
		return fmt.Sprintf("$%.0fk", v/1e3)
	default:
		return fmt.Sprintf("$%.0f", v)
	}
}
