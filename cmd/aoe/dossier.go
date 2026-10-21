package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/kwant-dbg/auto/internal/app"
	"github.com/kwant-dbg/auto/internal/config"
	"github.com/kwant-dbg/auto/internal/founder"
	"github.com/kwant-dbg/auto/internal/research"
	"github.com/kwant-dbg/auto/internal/store"
)

func researchCmd(ctx context.Context, cfg *config.Config, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("research", flag.ContinueOnError)
	qid := fs.Int64("question", 0, "research this question")
	oid := fs.Int64("opportunity", 0, "research this opportunity's best open question(s)")
	limit := fs.Int("limit", 1, "questions to research")
	if err := fs.Parse(args); err != nil {
		return err
	}
	client := llmOrNil(ctx, cfg, st)
	if client == nil {
		return errors.New("research needs Claude")
	}
	r := app.Researcher(cfg, st, client)
	ids := []int64{*qid}
	if *qid == 0 {
		rows, err := st.DB.Query(ctx, `
			SELECT q.id FROM question q JOIN opportunity o ON o.id = q.opportunity_id
			WHERE q.status = 'open' AND ($1 = 0 OR o.id = $1) AND o.state IN ('dossier', 'qualified', 'candidate')
			ORDER BY array_position(ARRAY['dossier','qualified','candidate'], o.state), `+research.QuestionPriority+`, q.id LIMIT $2`, *oid, *limit)
		if err != nil {
			return err
		}
		if ids, err = pgx.CollectRows(rows, pgx.RowTo[int64]); err != nil {
			return err
		}
		if len(ids) == 0 {
			fmt.Println("no open questions")
			return nil
		}
	}
	for _, id := range ids {
		out, err := r.Run(ctx, nil, id)
		if err != nil {
			return fmt.Errorf("question %d: %w", id, err)
		}
		b, _ := json.Marshal(out)
		fmt.Println(string(b))
	}
	return nil
}

func oppArg(args []string, cmd string) (int64, error) {
	if len(args) != 1 {
		return 0, fmt.Errorf("usage: aoe %s <opportunity>", cmd)
	}
	return strconv.ParseInt(args[0], 10, 64)
}

func assessCmd(ctx context.Context, cfg *config.Config, st *store.Store, args []string) error {
	id, err := oppArg(args, "assess")
	if err != nil {
		return err
	}
	client := llmOrNil(ctx, cfg, st)
	if client == nil {
		return errors.New("assessment needs Claude")
	}
	prof, err := founder.Load(cfg.FounderProfile)
	if err != nil {
		return err
	}
	out, err := app.Assessor(cfg, st, client, prof).Run(ctx, nil, id)
	if err != nil {
		return err
	}
	fmt.Printf("assessment %d: MRR@12m P10 $%.0f · P50 $%.0f · P90 $%.0f · P(≥$1k) %.0f%% · P(≥$5k) %.0f%% · weakest: %s · %d factors clamped to priors\n",
		out.AssessmentID, out.P10, out.P50, out.P90, 100*out.PGe1k, 100*out.PGe5k, out.Weakest, out.Clamped)
	return nil
}

func debateCmd(ctx context.Context, cfg *config.Config, st *store.Store, args []string) error {
	id, err := oppArg(args, "debate")
	if err != nil {
		return err
	}
	client := llmOrNil(ctx, cfg, st)
	if client == nil {
		return errors.New("the debate needs Claude")
	}
	out, err := app.Debater(cfg, st, client).Run(ctx, nil, id)
	if err != nil {
		return err
	}
	fmt.Printf("debate %d: %d arguments (%d struck) · bull holds %d · bear holds %d · unresolved %d · %d objections · %d new kill questions\n%s\n",
		out.DebateID, out.Arguments, out.Struck, out.BullHolds, out.BearHolds, out.Unresolved, out.Objections, out.Questions, out.Summary)
	return nil
}

// dossierSections prints findings, the current assessment and the latest debate.
func dossierSections(ctx context.Context, st *store.Store, id int64) error {
	rows, err := st.DB.Query(ctx, `SELECT f.id, f.verdict, f.stance, f.bears_on, f.claim, f.verifier_note,
		coalesce((SELECT string_agg(s->>'url', ' ') FROM jsonb_array_elements(f.citation_spans) s), '')
		FROM finding f WHERE f.opportunity_id = $1 ORDER BY (f.verdict = 'supported') DESC, f.id`, id)
	if err != nil {
		return err
	}
	defer rows.Close()
	header := false
	for rows.Next() {
		var fid int64
		var verdict, stance, bears, claim, note, urls *string
		if err := rows.Scan(&fid, &verdict, &stance, &bears, &claim, &note, &urls); err != nil {
			return err
		}
		if !header {
			fmt.Println("\nResearch findings (verified against stored snapshots):")
			header = true
		}
		n := ""
		if *verdict != "supported" && *note != "" {
			n = "  — " + *note
		}
		fmt.Printf("  F%d [%s, %s, %s] %s%s\n      %s\n", fid, *verdict, *stance, *bears, *claim, n, truncate(*urls, 200))
	}
	if err := rows.Err(); err != nil {
		return err
	}

	var aid int64
	var p10, p50, p90, p1, p5 float64
	var weakest string
	var paths, first10, experiments []byte
	err = st.DB.QueryRow(ctx, `SELECT a.id, a.ev_p10, a.ev_p50, a.ev_p90, a.p_ge_1k, a.p_ge_5k, a.weakest_factor, a.paths, a.first10_plan, a.next_experiments
		FROM opportunity o JOIN assessment a ON a.id = o.current_assessment_id WHERE o.id = $1`, id).
		Scan(&aid, &p10, &p50, &p90, &p1, &p5, &weakest, &paths, &first10, &experiments)
	if err == nil {
		fmt.Printf("\nAssessment %d: MRR at 12 months P10 $%.0f · P50 $%.0f · P90 $%.0f · P(≥$1k) %.0f%% · P(≥$5k) %.0f%% · weakest link: %s\n",
			aid, p10, p50, p90, 100*p1, 100*p5, weakest)
		frows, err := st.DB.Query(ctx, `SELECT factor_code, low, base, high, unit, clamped_to_prior, cardinality(citation_ids)
			FROM factor_estimate WHERE assessment_id = $1 ORDER BY factor_code`, aid)
		if err != nil {
			return err
		}
		for frows.Next() {
			var code, unit string
			var lo, base, hi float64
			var clamped bool
			var cites int
			if err := frows.Scan(&code, &lo, &base, &hi, &unit, &clamped, &cites); err != nil {
				return err
			}
			tag := fmt.Sprintf("%d citations", cites)
			if clamped {
				tag = "no evidence → wide prior"
			}
			fmt.Printf("  %-22s %10.3g  %10.3g  %10.3g  %-20s %s\n", code, lo, base, hi, unit, tag)
		}
		frows.Close()
		var ps []struct {
			Path    string  `json:"path"`
			Channel string  `json:"channel"`
			Weeks   float64 `json:"weeks_to_first_dollar"`
			PBuild  float64 `json:"p_build"`
		}
		if json.Unmarshal(paths, &ps) == nil && len(ps) > 0 {
			fmt.Println("  Paths:")
			for _, p := range ps {
				fmt.Printf("    %-18s first $ in ~%.0f weeks, P(build) %.2f, via %s\n", p.Path, p.Weeks, p.PBuild, p.Channel)
			}
		}
		var exps []struct {
			Hypothesis string  `json:"hypothesis"`
			Method     string  `json:"method"`
			Hours      float64 `json:"founder_hours"`
			Success    string  `json:"success_threshold"`
			Kill       string  `json:"kill_threshold"`
		}
		if json.Unmarshal(experiments, &exps) == nil && len(exps) > 0 {
			fmt.Println("  Next experiments:")
			for _, e := range exps {
				fmt.Printf("    • %s — %s (~%.0f h). Success: %s. Kill: %s\n", e.Hypothesis, e.Method, e.Hours, e.Success, e.Kill)
			}
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}

	var did int64
	var summary string
	err = st.DB.QueryRow(ctx, `SELECT id, summary FROM debate WHERE opportunity_id = $1 ORDER BY id DESC LIMIT 1`, id).Scan(&did, &summary)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Printf("\nDebate %d: %s\n", did, summary)
	rrows, err := st.DB.Query(ctx, `SELECT verdict, claim, rationale FROM debate_ruling WHERE debate_id = $1 ORDER BY id`, did)
	if err != nil {
		return err
	}
	for rrows.Next() {
		var v, c, r string
		if err := rrows.Scan(&v, &c, &r); err != nil {
			return err
		}
		fmt.Printf("  [%s] %s — %s\n", v, c, r)
	}
	rrows.Close()
	orows, err := st.DB.Query(ctx, `SELECT severity, text FROM objection WHERE debate_id = $1 ORDER BY severity DESC, id`, did)
	if err != nil {
		return err
	}
	defer orows.Close()
	for orows.Next() {
		var sev int
		var text string
		if err := orows.Scan(&sev, &text); err != nil {
			return err
		}
		fmt.Printf("  objection (severity %d): %s\n", sev, text)
	}
	return orows.Err()
}
