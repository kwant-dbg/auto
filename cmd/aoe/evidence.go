package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/jackc/pgx/v5"

	"github.com/kwant-dbg/auto/internal/app"
	"github.com/kwant-dbg/auto/internal/config"
	"github.com/kwant-dbg/auto/internal/extract"
	"github.com/kwant-dbg/auto/internal/store"
	"github.com/kwant-dbg/auto/internal/tools"
)

func extractCmd(ctx context.Context, cfg *config.Config, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("extract", flag.ContinueOnError)
	dry := fs.Bool("dry-run", false, "print the prompt for the next batch without calling Claude")
	source := fs.String("source", "", "only documents from this source")
	limit := fs.Int("limit", 5, "documents in the batch")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rows, err := st.DB.Query(ctx, `
		SELECT r.id FROM raw_document r JOIN source s ON s.id = r.source_id
		LEFT JOIN extraction e ON e.raw_document_id = r.id AND e.prompt_version = $1
		WHERE e.raw_document_id IS NULL AND ($2 = '' OR s.name = $2)
		ORDER BY r.id LIMIT $3`, extract.PromptVersion, *source, *limit)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		fmt.Println("nothing to extract")
		return nil
	}
	if *dry {
		docs, err := extract.LoadDocs(ctx, st, ids)
		if err != nil {
			return err
		}
		fmt.Print(extract.BuildPrompt(docs))
		return nil
	}
	client, _, err := app.NewLLM(ctx, cfg, st, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		return err
	}
	ex := &extract.Extractor{Store: st, LLM: client, Model: cfg.Models.Fast}
	out, err := ex.Run(ctx, nil, extract.Inputs{DocumentIDs: ids, PromptVersion: extract.PromptVersion})
	fmt.Printf("documents=%d observations=%d rejected=%d duplicates=%d\n", out.Documents, out.Observations, out.Rejected, out.Duplicates)
	return err
}

func observationsCmd(ctx context.Context, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("observations", flag.ContinueOnError)
	limit := fs.Int("limit", 20, "number to show")
	tier := fs.Int("tier", 7, "show tiers at or below this (0 = strongest)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rows, err := st.DB.Query(ctx, `
		SELECT o.id, o.evidence_tier, o.category, o.specificity, s.name,
		       o.actor_segment || ' need to ' || o.job || CASE WHEN o.obstacle <> '' THEN ' but ' || o.obstacle ELSE '' END,
		       o.quote, o.injection_suspect
		FROM observation o JOIN raw_document r ON r.id = o.raw_document_id JOIN source s ON s.id = r.source_id
		WHERE o.evidence_tier <= $2 ORDER BY o.id DESC LIMIT $1`, *limit, *tier)
	if err != nil {
		return err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var id int64
		var t, spec int
		var cat, src, stmt, quote string
		var suspect bool
		if err := rows.Scan(&id, &t, &cat, &spec, &src, &stmt, &quote, &suspect); err != nil {
			return err
		}
		flagText := ""
		if suspect {
			flagText = "  ⚠ injection-suspect source"
		}
		fmt.Printf("#%d  T%d %s  specificity %d  [%s]%s\n  %s\n  “%s”\n\n", id, t, cat, spec, src, flagText, stmt, truncate(quote, 300))
		n++
	}
	if n == 0 {
		fmt.Println("no observations yet")
	}
	return rows.Err()
}

func toolServer(cfg *config.Config, st *store.Store) *tools.Server {
	s := tools.NewServer(st, cfg.HTTP.UserAgent)
	s.GitHubToken = os.Getenv("GITHUB_TOKEN")
	return s
}

func kbCmd(ctx context.Context, cfg *config.Config, st *store.Store, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: aoe kb <query>")
	}
	out, err := toolServer(cfg, st).KBSearch(ctx, tools.KBInput{Query: strings.Join(args, " "), Limit: 15})
	if err != nil {
		return err
	}
	if len(out.Hits) == 0 {
		fmt.Println("no matches")
	}
	for _, h := range out.Hits {
		fmt.Printf("obs #%d (doc %d)  T%d %s  [%s]\n  %s\n  “%s”\n  %s\n\n", h.ObservationID, h.DocumentID, h.EvidenceTier,
			h.Category, h.Source, h.Statement, truncate(h.Quote, 300), h.URL)
	}
	return nil
}

func fetchCmd(ctx context.Context, cfg *config.Config, st *store.Store, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: aoe fetch <url>")
	}
	out, err := toolServer(cfg, st).Fetch(ctx, tools.FetchInput{URL: args[0]})
	if err != nil {
		return err
	}
	fmt.Printf("document %d  %s  (%s, %d bytes, injection_suspect=%v)\n%s\n\n%s\n", out.DocumentID, out.URL, out.ContentType,
		out.TotalBytes, out.InjectionSuspect, out.Title, truncate(out.Text, 2000))
	return nil
}

func statsCmd(ctx context.Context, st *store.Store) error {
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "SOURCE\tDOCUMENTS\tEXTRACTED\tQUEUED\tFAILED\tOBSERVATIONS\tREJECTED QUOTES\tREJECTION RATE")
	rows, err := st.DB.Query(ctx, `
		SELECT s.name, count(DISTINCT r.id),
		       count(DISTINCT e.raw_document_id) FILTER (WHERE e.status = 'done'),
		       count(DISTINCT e.raw_document_id) FILTER (WHERE e.status = 'queued'),
		       count(DISTINCT e.raw_document_id) FILTER (WHERE e.status = 'failed'),
		       coalesce(sum(e.observation_count) FILTER (WHERE e.status = 'done'), 0),
		       coalesce(sum(e.rejected_count) FILTER (WHERE e.status = 'done'), 0)
		FROM source s
		LEFT JOIN raw_document r ON r.source_id = s.id
		LEFT JOIN extraction e ON e.raw_document_id = r.id AND e.prompt_version = $1
		GROUP BY s.name ORDER BY s.name`, extract.PromptVersion)
	if err != nil {
		return err
	}
	for rows.Next() {
		var name string
		var docs, done, queued, failed, obs, rej int
		if err := rows.Scan(&name, &docs, &done, &queued, &failed, &obs, &rej); err != nil {
			return err
		}
		rate := "-"
		if obs+rej > 0 {
			rate = fmt.Sprintf("%.1f%%", 100*float64(rej)/float64(obs+rej))
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%d\t%s\n", name, docs, done, queued, failed, obs, rej, rate)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	fmt.Println("\nobservations by evidence tier:")
	trows, err := st.DB.Query(ctx, `SELECT evidence_tier, category, count(*), count(*) FILTER (WHERE embedding IS NOT NULL)
		FROM observation GROUP BY 1, 2 ORDER BY 1, 2`)
	if err != nil {
		return err
	}
	for trows.Next() {
		var tier, n, embedded int
		var cat string
		if err := trows.Scan(&tier, &cat, &n, &embedded); err != nil {
			return err
		}
		fmt.Printf("  T%d %-10s %6d  (embedded %d)\n", tier, cat, n, embedded)
	}
	if err := trows.Err(); err != nil {
		return err
	}

	var calls, turns int
	var cost float64
	if err := st.DB.QueryRow(ctx, `SELECT count(*), coalesce(sum(num_turns), 0), coalesce(sum(est_cost_usd), 0)
		FROM llm_call WHERE created_at >= date_trunc('day', now() AT TIME ZONE 'utc') AT TIME ZONE 'utc'`).Scan(&calls, &turns, &cost); err != nil {
		return err
	}
	fmt.Printf("\nClaude today (UTC): %d calls, %d turns, est. $%.2f (estimate only; runs on the subscription)\n", calls, turns, cost)
	return nil
}

func hnCmd(ctx context.Context, cfg *config.Config, st *store.Store, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: aoe hn <query>")
	}
	out, err := toolServer(cfg, st).SearchHN(ctx, tools.HNInput{Query: strings.Join(args, " "), Kind: "comment", Limit: 5})
	if err != nil {
		return err
	}
	for _, h := range out.Hits {
		fmt.Printf("doc %d  %s  %s\n  %s\n  %s\n\n", h.DocumentID, h.CreatedAt, h.Title, truncate(h.Text, 240), h.URL)
	}
	return nil
}
