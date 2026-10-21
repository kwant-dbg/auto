package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/jackc/pgx/v5"

	"github.com/kwant-dbg/auto/internal/app"
	"github.com/kwant-dbg/auto/internal/config"
	"github.com/kwant-dbg/auto/internal/llm"
	"github.com/kwant-dbg/auto/internal/problems"
	"github.com/kwant-dbg/auto/internal/store"
)

func llmOrNil(ctx context.Context, cfg *config.Config, st *store.Store) *llm.Client {
	client, _, err := app.NewLLM(ctx, cfg, st, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		fmt.Fprintln(os.Stderr, "Claude unavailable, ambiguous matches will wait:", err)
		return nil
	}
	return client
}

func clusterCmd(ctx context.Context, cfg *config.Config, st *store.Store) error {
	c := app.Clusterer(cfg, st, llmOrNil(ctx, cfg, st))
	total := 0
	for {
		s, err := c.Run(ctx, nil)
		fmt.Printf("considered=%d attached=%d new=%d adjudicated=%d waiting=%d new_problems=%d\n",
			s.Considered, s.AutoAttach, s.AutoNew, s.Adjudicated, s.Waiting, s.NewProblems)
		if err != nil {
			return err
		}
		total += s.Considered
		if s.Considered < c.Cfg.MaxPerRun || s.Considered == s.Waiting {
			break
		}
	}
	mode := "trigram"
	if cfg.Clustering.UseVectors {
		mode = "vector"
	}
	fmt.Printf("done (%s similarity, %d observations considered)\n", mode, total)
	return nil
}

func consolidateCmd(ctx context.Context, cfg *config.Config, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("consolidate", flag.ContinueOnError)
	all := fs.Bool("all", false, "re-consider every problem (e.g. after a new consolidation prompt)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *all {
		if err := problems.ResetConsolidation(ctx, st); err != nil {
			return err
		}
	}
	client := llmOrNil(ctx, cfg, st)
	if client == nil {
		return errors.New("consolidation needs Claude")
	}
	c := app.Clusterer(cfg, st, client)
	total := 0
	for {
		s, err := c.Consolidate(ctx, nil, 40)
		if err != nil {
			return err
		}
		fmt.Printf("anchors=%d merged=%d\n", s.Anchors, s.Merged)
		total += s.Merged
		if s.Anchors == 0 {
			break
		}
	}
	fmt.Printf("done: %d problems merged\n", total)
	return nil
}

func problemsCmd(ctx context.Context, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("problems", flag.ContinueOnError)
	minOrigins := fs.Int("min-origins", 1, "minimum independent origins")
	limit := fs.Int("limit", 30, "rows")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rows, err := st.DB.Query(ctx, `
		SELECT p.id, p.independent_origin_count, p.observation_count, coalesce(p.best_tier, 7), p.t1_origin_count,
		       coalesce(o.state, '-'), p.canonical_statement
		FROM problem p LEFT JOIN opportunity o ON o.problem_id = p.id
		WHERE p.status = 'active' AND p.independent_origin_count >= $1
		ORDER BY p.independent_origin_count DESC, p.t1_origin_count DESC, p.best_tier, p.id LIMIT $2`, *minOrigins, *limit)
	if err != nil {
		return err
	}
	defer rows.Close()
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tORIGINS\tOBS\tBEST\tT1 ORIGINS\tOPPORTUNITY\tSTATEMENT")
	for rows.Next() {
		var id int64
		var origins, n, best, t1 int
		var state, stmt string
		if err := rows.Scan(&id, &origins, &n, &best, &t1, &state, &stmt); err != nil {
			return err
		}
		fmt.Fprintf(tw, "%d\t%d\t%d\tT%d\t%d\t%s\t%s\n", id, origins, n, best, t1, state, truncate(stmt, 110))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return tw.Flush()
}

func problemCmd(ctx context.Context, st *store.Store, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: aoe problem <id>")
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return err
	}
	var stmt, segment string
	var origins, n int
	var parent *int64
	if err := st.DB.QueryRow(ctx, `SELECT canonical_statement, segment, independent_origin_count, observation_count, parent_id
		FROM problem WHERE id = $1`, id).Scan(&stmt, &segment, &origins, &n, &parent); err != nil {
		return fmt.Errorf("problem %d: %w", id, err)
	}
	fmt.Printf("Problem %d: %s\nSegment: %s\nEvidence: %d observations from %d independent origins\n", id, stmt, segment, n, origins)
	if parent != nil {
		fmt.Printf("Sub-problem of %d\n", *parent)
	}
	rows, err := st.DB.Query(ctx, `
		SELECT o.id, o.evidence_tier, o.category, s.name, o.quote, r.canonical_url, cd.method
		FROM problem_observation po JOIN observation o ON o.id = po.observation_id
		JOIN raw_document r ON r.id = o.raw_document_id JOIN source s ON s.id = r.source_id
		LEFT JOIN cluster_decision cd ON cd.observation_id = o.id
		WHERE po.problem_id = $1 ORDER BY o.evidence_tier, o.id`, id)
	if err != nil {
		return err
	}
	defer rows.Close()
	fmt.Println()
	for rows.Next() {
		var oid int64
		var tier int
		var cat, src, quote, url string
		var method *string
		if err := rows.Scan(&oid, &tier, &cat, &src, &quote, &url, &method); err != nil {
			return err
		}
		m := ""
		if method != nil {
			m = " via " + *method
		}
		fmt.Printf("  obs #%d  T%d %s  [%s]%s\n    “%s”\n    %s\n", oid, tier, cat, src, m, truncate(quote, 240), url)
	}
	return rows.Err()
}

func triageCmd(ctx context.Context, cfg *config.Config, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("triage", flag.ContinueOnError)
	problemID := fs.Int64("problem", 0, "triage this problem regardless of eligibility")
	limit := fs.Int("limit", 5, "eligible problems to triage")
	if err := fs.Parse(args); err != nil {
		return err
	}
	client := llmOrNil(ctx, cfg, st)
	if client == nil {
		return errors.New("triage needs Claude")
	}
	tr, err := app.Triager(cfg, st, client)
	if err != nil {
		return err
	}
	ids := []int64{*problemID}
	if *problemID == 0 {
		rows, err := st.DB.Query(ctx, `SELECT id FROM problem WHERE status = 'active' AND last_triaged_at IS NULL
			AND independent_origin_count >= $1 ORDER BY t1_origin_count DESC, independent_origin_count DESC, id LIMIT $2`,
			cfg.Triage.MinIndependentOrigins, *limit)
		if err != nil {
			return err
		}
		ids, err = pgx.CollectRows(rows, pgx.RowTo[int64])
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			fmt.Printf("no untriaged problems with ≥ %d independent origins\n", cfg.Triage.MinIndependentOrigins)
			return nil
		}
	}
	for _, id := range ids {
		out, err := tr.Run(ctx, nil, id)
		if err != nil {
			return fmt.Errorf("problem %d: %w", id, err)
		}
		reason := ""
		if out.Reason != "" {
			reason = " (" + out.Reason + ")"
		}
		fmt.Printf("problem %d → opportunity %d: %s%s, %d questions\n", id, out.OpportunityID, out.State, reason, out.Questions)
	}
	return nil
}

func opportunitiesCmd(ctx context.Context, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("opportunities", flag.ContinueOnError)
	state := fs.String("state", "", "filter by state")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rows, err := st.DB.Query(ctx, `
		SELECT o.id, o.state, p.independent_origin_count, coalesce(p.best_tier, 7),
		       (SELECT string_agg(g.gate_code || ':' || left(g.status, 1), ' ' ORDER BY g.gate_code) FROM gate_status g WHERE g.opportunity_id = o.id),
		       (SELECT count(*) FROM question q WHERE q.opportunity_id = o.id AND q.status = 'open'),
		       p.canonical_statement
		FROM opportunity o JOIN problem p ON p.id = o.problem_id
		WHERE $1 = '' OR o.state = $1
		ORDER BY array_position(ARRAY['qualified','dossier','validating','candidate','parked','killed'], o.state),
		         p.t1_origin_count DESC, p.independent_origin_count DESC`, *state)
	if err != nil {
		return err
	}
	defer rows.Close()
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATE\tORIGINS\tBEST\tGATES (P/F/U/S)\tOPEN Qs\tPROBLEM")
	for rows.Next() {
		var id int64
		var st, gates, stmt string
		var origins, best, qs int
		var g *string
		if err := rows.Scan(&id, &st, &origins, &best, &g, &qs, &stmt); err != nil {
			return err
		}
		if g != nil {
			gates = *g
		}
		fmt.Fprintf(tw, "%d\t%s\t%d\tT%d\t%s\t%d\t%s\n", id, st, origins, best, gates, qs, truncate(stmt, 80))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return tw.Flush()
}

func opportunityCmd(ctx context.Context, st *store.Store, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: aoe opportunity <id>")
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return err
	}
	var state, segment, stmt string
	var problemID int64
	var origins, n, budget, used int
	if err := st.DB.QueryRow(ctx, `SELECT o.state, o.segment, p.id, p.canonical_statement, p.independent_origin_count, p.observation_count,
		o.budget_units_total, o.budget_units_used
		FROM opportunity o JOIN problem p ON p.id = o.problem_id WHERE o.id = $1`, id).
		Scan(&state, &segment, &problemID, &stmt, &origins, &n, &budget, &used); err != nil {
		return fmt.Errorf("opportunity %d: %w", id, err)
	}
	fmt.Printf("Opportunity %d — %s\nProblem %d: %s\nSegment: %s\nEvidence: %d observations, %d independent origins\nResearch budget: %d/%d units\n",
		id, strings.ToUpper(state), problemID, stmt, segment, n, origins, used, budget)

	var notes string
	var reason *string
	_ = st.DB.QueryRow(ctx, `SELECT notes, reason_code FROM decision WHERE subject_type = 'opportunity' AND subject_id = $1
		ORDER BY id DESC LIMIT 1`, id).Scan(&notes, &reason)
	if notes != "" {
		fmt.Printf("\nTriage summary: %s\n", notes)
	}
	if reason != nil {
		fmt.Printf("Reason: %s\n", *reason)
	}

	fmt.Println("\nGates:")
	rows, err := st.DB.Query(ctx, `SELECT gate_code, status, rationale, evidence_ids FROM gate_status WHERE opportunity_id = $1`, id)
	if err != nil {
		return err
	}
	type gate struct {
		code, status, why string
		ids               []int64
	}
	gates, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (gate, error) {
		var g gate
		err := r.Scan(&g.code, &g.status, &g.why, &g.ids)
		return g, err
	})
	if err != nil {
		return err
	}
	sort.Slice(gates, func(i, j int) bool { return gates[i].code < gates[j].code })
	for _, g := range gates {
		cite := ""
		if len(g.ids) > 0 {
			cite = fmt.Sprintf("  [obs %v]", g.ids)
		}
		fmt.Printf("  %-3s %-7s %s%s\n", g.code, g.status, g.why, cite)
	}

	fmt.Println("\nQuestions (kill questions first, then by P(kill) ÷ cost):")
	qrows, err := st.DB.Query(ctx, `SELECT kind, status, linked_code, text, coalesce(p_kill, 0), coalesce(cost_estimate, 0)
		FROM question WHERE opportunity_id = $1
		ORDER BY status = 'open' DESC, kind = 'kill' DESC, coalesce(voe_estimate, 0) DESC, id`, id)
	if err != nil {
		return err
	}
	defer qrows.Close()
	for qrows.Next() {
		var kind, status, code, text string
		var pk, cost float64
		if err := qrows.Scan(&kind, &status, &code, &text, &pk, &cost); err != nil {
			return err
		}
		fmt.Printf("  [%s %s %s] %s  (P(kill) %.2f, ~%.0f min)\n", status, kind, code, text, pk, cost)
	}
	return qrows.Err()
}
