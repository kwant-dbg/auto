package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/kwant-dbg/auto/internal/app"
	"github.com/kwant-dbg/auto/internal/baseline"
	"github.com/kwant-dbg/auto/internal/config"
	"github.com/kwant-dbg/auto/internal/feedback"
	"github.com/kwant-dbg/auto/internal/founder"
	"github.com/kwant-dbg/auto/internal/report"
	"github.com/kwant-dbg/auto/internal/store"
)

// parseArgs parses flags wherever they appear, so `aoe rate 26 useful
// --reason x` works, and returns the positional arguments.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// multi is a repeatable string flag.
type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func idArg(s, what string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimPrefix(strings.TrimPrefix(s, "F"), "#"), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s id %q: %w", what, s, err)
	}
	return id, nil
}

func digestCmd(ctx context.Context, cfg *config.Config, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("digest", flag.ContinueOnError)
	preview := fs.Bool("preview", false, "render this week's digest without saving it")
	show := fs.String("show", "", "print a stored digest (ISO week, e.g. 2026-W43; 'latest' for the newest)")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	if *show != "" {
		var md string
		err := st.DB.QueryRow(ctx, `SELECT markdown FROM digest WHERE $1 = 'latest' OR week = $1 ORDER BY period_end DESC LIMIT 1`, *show).Scan(&md)
		if err != nil {
			return fmt.Errorf("digest %s: %w", *show, err)
		}
		fmt.Print(md)
		return nil
	}
	b := &report.Builder{Store: st, Opt: cfg.Digest}
	d, err := b.Build(ctx, time.Now(), "human", !*preview)
	if err != nil {
		return err
	}
	fmt.Print(d.Markdown)
	switch {
	case *preview:
		fmt.Fprintln(os.Stderr, "\n(preview: nothing saved; pair numbers are placeholders)")
	case d.Existing:
		fmt.Fprintf(os.Stderr, "\n(this week's digest already existed: %s)\n", d.Week)
	default:
		fmt.Fprintf(os.Stderr, "\nsaved digest %d for %s %s\n", d.ID, d.Week, d.Path)
	}
	return nil
}

func cardCmd(ctx context.Context, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("card", flag.ContinueOnError)
	blind := fs.Bool("blind", false, "render as in a blind pair")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: aoe card <opportunity> [--blind]")
	}
	id, err := idArg(pos[0], "opportunity")
	if err != nil {
		return err
	}
	chk := report.NewChecker(st)
	c, err := report.LoadCard(ctx, chk, id)
	if err != nil {
		return err
	}
	fmt.Print(c.Markdown(report.RenderOptions{Blind: *blind}))
	if chk.Failed > 0 {
		fmt.Fprintf(os.Stderr, "\n%d quotes failed the span check and were left out\n", chk.Failed)
	}
	return nil
}

func rateCmd(ctx context.Context, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("rate", flag.ContinueOnError)
	reason := fs.String("reason", "", "reason code or short note")
	novelty := fs.Int("novelty", 0, "would you have thought of this yourself? 1 (obviously) to 5 (never)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 2 {
		return errors.New("usage: aoe rate <opportunity> useful|not|duplicate [--novelty 1-5] [--reason R]")
	}
	id, err := idArg(pos[0], "opportunity")
	if err != nil {
		return err
	}
	r := *reason
	if r == "" && len(pos) > 2 {
		r = strings.Join(pos[2:], " ")
	}
	if err := feedback.Rate(ctx, st, id, pos[1], r, *novelty); err != nil {
		return err
	}
	fmt.Printf("recorded: opportunity %d rated %s\n", id, pos[1])
	return nil
}

func pickCmd(ctx context.Context, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("pick", flag.ContinueOnError)
	reason := fs.String("reason", "", "reason code or short note")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 2 {
		return errors.New("usage: aoe pick <pair> A|B [--reason R]")
	}
	id, err := idArg(pos[0], "pair")
	if err != nil {
		return err
	}
	r := *reason
	if r == "" && len(pos) > 2 {
		r = strings.Join(pos[2:], " ")
	}
	res, err := feedback.Pick(ctx, st, id, pos[1], r)
	if err != nil {
		return err
	}
	// Revealed only after the pick is stored, so it can't sway it.
	who := "the baseline (B0)"
	if res.AOEWon {
		who = "AOE"
	}
	fmt.Printf("recorded: pair %d → %s, which was %s's card\n", id, strings.ToUpper(pos[1]), who)
	return nil
}

func gradeCmd(ctx context.Context, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("grade", flag.ContinueOnError)
	reason := fs.String("reason", "", "short note")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 2 {
		return errors.New("usage: aoe grade <finding> supported|overclaimed|wrong [--reason R]")
	}
	id, err := idArg(pos[0], "finding")
	if err != nil {
		return err
	}
	if err := feedback.Grade(ctx, st, id, pos[1], *reason); err != nil {
		return err
	}
	fmt.Printf("recorded: finding %d graded %s\n", id, pos[1])
	return nil
}

func decideCmd(ctx context.Context, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("decide", flag.ContinueOnError)
	reason := fs.String("reason", "", "reason code (kill: "+strings.Join([]string{"no_buyer", "no_spend", "unreachable", "incumbent_strong", "negative_margin", "infeasible", "founder_veto", "out_of_scope", "..."}, ", ")+"; park: also not_now)")
	notes := fs.String("notes", "", "free-text notes")
	override := fs.Bool("override", false, "advance even though a transition check fails (recorded)")
	var wake multi
	fs.Var(&wake, "wake", "park: wake condition, repeatable: date:YYYY-MM-DD, evidence:+N, competitor:..., capability:..., regulation:...")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return errors.New("usage: aoe decide <opportunity> advance|park|kill|revive --reason <code> [--wake ...] [--notes ...] [--override]")
	}
	id, err := idArg(pos[0], "opportunity")
	if err != nil {
		return err
	}
	from, to, err := feedback.Decide(ctx, st, feedback.Decision{OpportunityID: id, Action: pos[1], Reason: *reason, Notes: *notes,
		Wake: wake, Override: *override}, time.Now())
	if err != nil {
		return err
	}
	fmt.Printf("opportunity %d: %s → %s\n", id, from, to)
	return nil
}

func answerCmd(ctx context.Context, st *store.Store, args []string) error {
	if len(args) < 2 {
		return errors.New(`usage: aoe answer <question> "<answer>"`)
	}
	id, err := idArg(args[0], "question")
	if err != nil {
		return err
	}
	state, err := feedback.Answer(ctx, st, id, strings.Join(args[1:], " "))
	if err != nil {
		return err
	}
	fmt.Printf("recorded; the opportunity is now %s\n", state)
	return nil
}

func reviewTimeCmd(ctx context.Context, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("review-time", flag.ContinueOnError)
	week := fs.String("week", "", "ISO week (default: the latest digest)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: aoe review-time <minutes> [--week 2026-W43]")
	}
	m, err := strconv.Atoi(pos[0])
	if err != nil {
		return err
	}
	w, err := feedback.ReviewMinutes(ctx, st, *week, m)
	if err != nil {
		return err
	}
	fmt.Printf("recorded: %d minutes on the %s digest\n", m, w)
	return nil
}

func metricsCmd(ctx context.Context, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("metrics", flag.ContinueOnError)
	weeks := fs.Int("weeks", 4, "trailing window in weeks")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	ms, err := report.Metrics(ctx, st, time.Now(), time.Duration(*weeks)*7*24*time.Hour)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintf(tw, "\tCRITERION\tVALUE\tTARGET\n")
	for _, m := range ms {
		mark := "·"
		if m.Met != nil && *m.Met {
			mark = "✓"
		} else if m.Met != nil {
			mark = "✗"
		}
		fmt.Fprintf(tw, "%s %s\t%s\t%s\t%s\n", mark, m.Code, m.Name, m.Value, m.Target)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, m := range ms {
		if m.How != "" {
			fmt.Printf("  %s: %s\n", m.Code, m.How)
		}
	}
	rule, err := report.StopRule(ctx, st)
	if err != nil {
		return err
	}
	fmt.Println("\n" + rule)
	return nil
}

func baselineCmd(ctx context.Context, cfg *config.Config, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("baseline", flag.ContinueOnError)
	week := fs.String("week", report.ISOWeek(time.Now()), "ISO week to run for")
	dry := fs.Bool("dry-run", false, "print the prompt only")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	prof, err := founder.Load(cfg.FounderProfile)
	if err != nil {
		return err
	}
	if *dry {
		fmt.Print(baseline.BuildPrompt(prof.Summary(), cfg.Baseline.Cards, nil))
		return nil
	}
	client := llmOrNil(ctx, cfg, st)
	if client == nil {
		return errors.New("the baseline needs Claude")
	}
	out, err := app.Baseline(cfg, st, client, prof).Run(ctx, nil, *week)
	if err != nil {
		return err
	}
	fmt.Printf("baseline run %d for %s: %d cards\n", out.RunID, *week, out.Cards)
	return nil
}
