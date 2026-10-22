package report

import (
	"context"
	crand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kwant-dbg/auto/internal/store"
)

// Options are the digest's attention limits (docs/08 §3).
type Options struct {
	Cards      int    `yaml:"cards"`       // ≤ 3 cards
	Approvals  int    `yaml:"approvals"`   // ≤ 5 decisions needed
	Pairs      int    `yaml:"pairs"`       // blind pairs vs B0
	SpotChecks int    `yaml:"spot_checks"` // random findings to grade
	Changes    int    `yaml:"changes"`     // notable changes
	OutDir     string `yaml:"out_dir"`     // markdown copies; empty skips
	// Weekday and Hour (UTC) are when the scheduler builds the week's digest.
	Weekday string `yaml:"weekday"`
	Hour    int    `yaml:"hour"`
	// BaselineWait is how long the digest waits for the week's B0 run.
	BaselineWait time.Duration `yaml:"baseline_wait"`
}

// Defaults fills unset values.
func (o *Options) Defaults() {
	def := func(v *int, d int) {
		if *v <= 0 {
			*v = d
		}
	}
	def(&o.Cards, 3)
	def(&o.Approvals, 5)
	def(&o.Pairs, 2)
	def(&o.SpotChecks, 3)
	def(&o.Changes, 5)
	if o.Weekday == "" {
		o.Weekday = "monday"
	}
	if o.BaselineWait <= 0 {
		o.BaselineWait = 6 * time.Hour
	}
}

// Builder builds weekly digests.
type Builder struct {
	Store *store.Store
	Opt   Options
	// AOEFirst draws which side of a blind pair the AOE card takes;
	// defaults to a fair coin from crypto/rand.
	AOEFirst func() bool
}

// Digest is a built digest.
type Digest struct {
	ID       int64  `json:"id"`
	Week     string `json:"week"`
	Markdown string `json:"-"`
	Stats    Stats  `json:"stats"`
	Path     string `json:"path,omitempty"`
	Existing bool   `json:"existing,omitempty"`
}

// Stats are the digest's own measurements.
type Stats struct {
	Cards         int     `json:"cards"`
	Approvals     int     `json:"approvals"`
	Pairs         int     `json:"pairs"`
	SpotChecks    int     `json:"spot_checks"`
	Changes       int     `json:"changes"`
	Woken         int     `json:"woken"`
	Killed        int     `json:"killed"`
	QuotesChecked int     `json:"quotes_checked"`
	QuotesFailed  int     `json:"quotes_failed"` // dropped: never shown (A1)
	Turns         int     `json:"turns"`
	EstCostUSD    float64 `json:"est_cost_usd"`
}

// ErrExists means the week already has a digest.
var ErrExists = errors.New("report: digest already exists for this week")

// Build assembles the digest for now's ISO week. With save it is stored
// (with the cards, pairs and spot checks it showed) and written to OutDir;
// a week that already has one returns it unchanged. Without save it is a
// preview: nothing is written and pair ids are placeholders.
func (b *Builder) Build(ctx context.Context, now time.Time, generatedBy string, save bool) (*Digest, error) {
	b.Opt.Defaults()
	week := ISOWeek(now)
	if save {
		d, err := b.existing(ctx, week)
		if err == nil {
			return d, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	}
	start := now.Add(-7 * 24 * time.Hour)
	var prevEnd time.Time
	if err := b.Store.DB.QueryRow(ctx, `SELECT period_end FROM digest WHERE period_end < $1 ORDER BY period_end DESC LIMIT 1`, now).Scan(&prevEnd); err == nil {
		start = prevEnd
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	out := &Digest{Week: week}
	var md string
	run := func(tx pgx.Tx) error {
		g := &gen{b: b, tx: tx, chk: NewChecker(b.Store), start: start, end: now, save: save, d: out}
		if save {
			if err := tx.QueryRow(ctx, `INSERT INTO digest (week, period_start, period_end, markdown, generated_by) VALUES ($1, $2, $3, '', $4) RETURNING id`,
				week, start, now, generatedBy).Scan(&out.ID); err != nil {
				return err
			}
		}
		var err error
		md, err = g.render(ctx)
		if err != nil {
			return err
		}
		out.Stats.QuotesChecked, out.Stats.QuotesFailed = g.chk.Checked, g.chk.Failed
		out.Markdown = md
		if !save {
			return nil
		}
		stats, _ := json.Marshal(out.Stats)
		_, err = tx.Exec(ctx, `UPDATE digest SET markdown = $2, stats = $3 WHERE id = $1`, out.ID, md, stats)
		return err
	}
	var err error
	if save {
		err = pgx.BeginFunc(ctx, b.Store.DB, run)
	} else {
		err = pgx.BeginTxFunc(ctx, b.Store.DB, pgx.TxOptions{AccessMode: pgx.ReadOnly}, run)
	}
	if err != nil {
		return nil, err
	}
	if save && b.Opt.OutDir != "" {
		if err := os.MkdirAll(b.Opt.OutDir, 0o755); err != nil {
			return out, err
		}
		out.Path = filepath.Join(b.Opt.OutDir, "digest-"+week+".md")
		if err := os.WriteFile(out.Path, []byte(md), 0o644); err != nil {
			return out, err
		}
	}
	return out, nil
}

func (b *Builder) existing(ctx context.Context, week string) (*Digest, error) {
	d := &Digest{Week: week, Existing: true}
	var stats []byte
	err := b.Store.DB.QueryRow(ctx, `SELECT id, markdown, stats FROM digest WHERE week = $1`, week).Scan(&d.ID, &d.Markdown, &stats)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(stats, &d.Stats)
	if b.Opt.OutDir != "" {
		d.Path = filepath.Join(b.Opt.OutDir, "digest-"+week+".md")
	}
	return d, nil
}

// gen holds one build's state.
type gen struct {
	b          *Builder
	tx         pgx.Tx
	chk        *Checker
	start, end time.Time
	save       bool
	d          *Digest
	w          strings.Builder
	cardIDs    []int64
}

func (g *gen) printf(format string, a ...any) { fmt.Fprintf(&g.w, format, a...) }

func (g *gen) render(ctx context.Context) (string, error) {
	g.printf("# Weekly digest — %s\n\n", g.d.Week)
	g.printf("*%s → %s · aim to finish in 60 minutes*\n\n", g.start.UTC().Format("2006-01-02 15:04"), g.end.UTC().Format("2006-01-02 15:04 MST"))
	steps := []struct {
		title string
		fn    func(context.Context) error
	}{
		{"0. System health", g.health},
		{"1. Decisions needed", g.approvals},
		{"2. Top opportunity cards", g.cards},
		{"3. Notable changes", g.changes},
		{"4. Blind pairwise: which would you rather spend an hour on?", g.pairs},
		{"5. Spot-check: grade these findings", g.spotChecks},
		{"6. Parked → woken this week", g.woken},
		{"7. Killed this week", g.killed},
	}
	for _, s := range steps {
		g.printf("## %s\n\n", s.title)
		if err := s.fn(ctx); err != nil {
			return "", fmt.Errorf("%s: %w", s.title, err)
		}
		g.printf("\n")
	}
	g.printf("---\nFeedback: `aoe rate <card> useful|not|duplicate [--novelty 1-5] [--reason R]` · `aoe pick <pair> A|B [--reason R]` · " +
		"`aoe grade <finding> supported|overclaimed|wrong` · `aoe decide <card> advance|park|kill|revive --reason <code> [--wake ...]` · " +
		"`aoe review-time <minutes>`\n")
	return g.w.String(), nil
}

func (g *gen) health(ctx context.Context) error {
	var enabled, healthy int
	var sick []string
	rows, err := g.tx.Query(ctx, `SELECT name, consecutive_errors FROM source WHERE enabled ORDER BY name`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var name string
		var errs int
		if err := rows.Scan(&name, &errs); err != nil {
			rows.Close()
			return err
		}
		enabled++
		if errs == 0 {
			healthy++
		} else {
			sick = append(sick, fmt.Sprintf("%s (%d errors in a row)", name, errs))
		}
	}
	rows.Close()
	var turns, failedCalls, dead int
	var cost float64
	if err := g.tx.QueryRow(ctx, `SELECT coalesce(sum(num_turns), 0), coalesce(sum(est_cost_usd), 0), count(*) FILTER (WHERE status <> 'ok')
		FROM llm_call WHERE created_at >= $1 AND created_at < $2`, g.start, g.end).Scan(&turns, &cost, &failedCalls); err != nil {
		return err
	}
	// Failed collections don't retry in the queue: the source's back-off
	// reschedules them, so they're reported apart from real dead letters.
	var collectFailed int
	if err := g.tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE type <> 'collect'), count(*) FILTER (WHERE type = 'collect')
		FROM task WHERE state = 'dead' AND finished_at >= $1 AND finished_at < $2`, g.start, g.end).Scan(&dead, &collectFailed); err != nil {
		return err
	}
	var paused bool
	if err := g.tx.QueryRow(ctx, `SELECT coalesce((SELECT value = 'true'::jsonb FROM control WHERE key = 'paused'), false)`).Scan(&paused); err != nil {
		return err
	}
	g.d.Stats.Turns, g.d.Stats.EstCostUSD = turns, cost
	ok := "✓"
	if healthy < enabled || dead > 0 || paused {
		ok = "⚠"
	}
	state := "running"
	if paused {
		state = "PAUSED"
	}
	g.printf("%s %d/%d sources healthy · %d Claude turns (≈$%.0f at API prices; covered by the subscription) · %d failed calls · %d dead tasks · %d collection runs failed and were rescheduled · %s\n",
		ok, healthy, enabled, turns, cost, failedCalls, dead, collectFailed, state)
	for _, s := range sick {
		g.printf("- unhealthy: %s\n", s)
	}
	return nil
}

// approvals lists what only the founder can decide: dossiers ready for a
// go/no-go, qualified opportunities eligible for an early probe, and
// questions desk research can't answer.
func (g *gen) approvals(ctx context.Context) error {
	type item struct {
		oppID, qID int64
		text       string
	}
	var items []item
	rows, err := g.tx.Query(ctx, `
		SELECT o.id, o.state, p.canonical_statement, o.saturated, a.p_ge_1k,
		       (SELECT count(*) FROM prospect pr WHERE pr.opportunity_id = o.id),
		       (SELECT count(*) FROM question q WHERE q.opportunity_id = o.id AND q.status = 'open')
		FROM opportunity o JOIN problem p ON p.id = o.problem_id
		LEFT JOIN assessment a ON a.id = o.current_assessment_id
		WHERE o.state = 'dossier'
		   OR (o.state = 'qualified' AND (SELECT count(*) FROM prospect pr WHERE pr.opportunity_id = o.id) >= 20)
		ORDER BY o.state = 'dossier' DESC, o.saturated DESC, a.p_ge_1k DESC NULLS LAST, o.id`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		var state, stmt string
		var saturated bool
		var p1k *float64
		var prospects, open int
		if err := rows.Scan(&id, &state, &stmt, &saturated, &p1k, &prospects, &open); err != nil {
			rows.Close()
			return err
		}
		why := "eligible for an early probe (M1, M2 pass; ≥ 20 prospects)"
		if state == "dossier" {
			why = fmt.Sprintf("dossier, %d open questions", open)
			if saturated {
				why = "dossier, desk research saturated"
			}
		}
		if p1k != nil {
			why += fmt.Sprintf(", P(≥$1k) %.2f", *p1k)
		}
		items = append(items, item{oppID: id, text: fmt.Sprintf("**Opportunity %d** — %s. %s; %d prospects → `aoe decide %d advance|park|kill --reason <code>`",
			id, truncate(oneLine(stmt), 140), why, prospects, id)})
	}
	rows.Close()
	qrows, err := g.tx.Query(ctx, `
		SELECT q.id, q.opportunity_id, q.text FROM question q JOIN opportunity o ON o.id = q.opportunity_id
		WHERE q.status = 'needs_human' AND q.linked_code <> 'F1' AND o.state NOT IN ('killed', 'parked')
		ORDER BY q.kind = 'kill' DESC, coalesce(q.voe_estimate, 0) DESC, q.id`)
	if err != nil {
		return err
	}
	for qrows.Next() {
		var it item
		var text string
		if err := qrows.Scan(&it.qID, &it.oppID, &text); err != nil {
			qrows.Close()
			return err
		}
		it.text = fmt.Sprintf("**Question %d** (opportunity %d) needs you: %s → `aoe answer %d \"...\"`", it.qID, it.oppID, oneLine(text), it.qID)
		items = append(items, it)
	}
	qrows.Close()
	if len(items) == 0 {
		g.printf("Nothing needs a decision this week.\n")
		return nil
	}
	shown := min(len(items), g.b.Opt.Approvals)
	for i, it := range items[:shown] {
		g.printf("%d. %s\n", i+1, it.text)
		if g.save {
			var q *int64
			if it.qID != 0 {
				q = &it.qID
			}
			if _, err := g.tx.Exec(ctx, `INSERT INTO digest_item (digest_id, kind, position, opportunity_id, question_id) VALUES ($1, 'approval', $2, $3, $4)`,
				g.d.ID, i+1, it.oppID, q); err != nil {
				return err
			}
		}
	}
	if len(items) > shown {
		g.printf("\n%d more held back by the weekly limit of %d.\n", len(items)-shown, g.b.Opt.Approvals)
	}
	g.d.Stats.Approvals = shown
	return nil
}

// rankSQL orders opportunities for cards: the latest pairwise ranking
// (RPT-2) first, then expected value, stage and strength of evidence.
const rankSQL = `o.rank_score DESC NULLS LAST, a.p_ge_1k DESC NULLS LAST, array_position(ARRAY['validating','dossier','qualified','candidate'], o.state),
	p.t1_origin_count DESC, p.independent_origin_count DESC, o.id`

// cards shows up to Opt.Cards qualified-or-later opportunities. One already
// shown in an earlier digest returns only after a material change (a new
// assessment, state change or verified finding), so the founder doesn't
// review the same card twice (A4).
func (g *gen) cards(ctx context.Context) error {
	rows, err := g.tx.Query(ctx, `
		SELECT o.id FROM opportunity o JOIN problem p ON p.id = o.problem_id
		LEFT JOIN assessment a ON a.id = o.current_assessment_id
		LEFT JOIN LATERAL (SELECT max(d.created_at) AS at FROM digest_item di JOIN digest d ON d.id = di.digest_id
		                   WHERE di.kind = 'card' AND di.opportunity_id = o.id) shown ON true
		WHERE o.state IN ('qualified', 'dossier', 'validating')
		  AND (shown.at IS NULL
		       OR EXISTS (SELECT 1 FROM assessment a2 WHERE a2.opportunity_id = o.id AND a2.created_at > shown.at)
		       OR EXISTS (SELECT 1 FROM decision dc WHERE dc.subject_type = 'opportunity' AND dc.subject_id = o.id
		                  AND dc.created_at > shown.at AND dc.from_state IS DISTINCT FROM dc.to_state AND dc.to_state IS NOT NULL)
		       OR EXISTS (SELECT 1 FROM finding f WHERE f.opportunity_id = o.id AND f.created_at > shown.at AND f.verdict = 'supported'))
		ORDER BY `+rankSQL+` LIMIT $1`, g.b.Opt.Cards)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return g.noCards(ctx)
	}
	for i, id := range ids {
		c, err := LoadCard(ctx, g.chk, id)
		if err != nil {
			return err
		}
		g.printf("%s\n", c.Markdown(RenderOptions{}))
		if g.save {
			snap, _ := json.Marshal(c)
			if _, err := g.tx.Exec(ctx, `INSERT INTO digest_item (digest_id, kind, position, opportunity_id, snapshot) VALUES ($1, 'card', $2, $3, $4)`,
				g.d.ID, i+1, id, snap); err != nil {
				return err
			}
		}
	}
	g.cardIDs = ids
	g.d.Stats.Cards = len(ids)
	return nil
}

// noCards says so explicitly (RPT-1) and names the closest opportunity and
// what blocks it.
func (g *gen) noCards(ctx context.Context) error {
	var id int64
	var state, stmt string
	err := g.tx.QueryRow(ctx, `SELECT o.id, o.state, p.canonical_statement FROM opportunity o JOIN problem p ON p.id = o.problem_id
		LEFT JOIN assessment a ON a.id = o.current_assessment_id
		WHERE o.state IN ('candidate', 'qualified', 'dossier', 'validating') ORDER BY `+rankSQL+` LIMIT 1`).Scan(&id, &state, &stmt)
	if errors.Is(err, pgx.ErrNoRows) {
		g.printf("No opportunity met the bar this week, and none is in progress.\n")
		return nil
	}
	if err != nil {
		return err
	}
	g.printf("No opportunity met the bar this week. Closest: **opportunity %d** (%s) — %s, blocked on %s.\n",
		id, state, truncate(oneLine(stmt), 140), g.blocker(ctx, id))
	return nil
}

// blocker names the first gate that isn't PASS, or the next kill question.
func (g *gen) blocker(ctx context.Context, id int64) string {
	for _, code := range []string{"M1", "M2", "M3", "M4", "M5", "M6"} {
		var status string
		err := g.tx.QueryRow(ctx, `SELECT status FROM gate_status WHERE opportunity_id = $1 AND gate_code = $2`, id, code).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && status != "PASS") {
			if status == "" {
				status = "UNKNOWN"
			}
			return fmt.Sprintf("%s (%s)", code, status)
		}
	}
	var q string
	if err := g.tx.QueryRow(ctx, `SELECT text FROM question WHERE opportunity_id = $1 AND kind = 'kill' AND status <> 'answered' ORDER BY id LIMIT 1`, id).Scan(&q); err == nil {
		return "the kill question “" + truncate(oneLine(q), 120) + "”"
	}
	return "research budget"
}

func (g *gen) changes(ctx context.Context) error {
	rows, err := g.tx.Query(ctx, `
		SELECT d.subject_id, d.decision_type, d.actor, coalesce(d.from_state, '-'), d.to_state, coalesce(d.reason_code, ''), p.canonical_statement
		FROM decision d JOIN opportunity o ON o.id = d.subject_id JOIN problem p ON p.id = o.problem_id
		WHERE d.subject_type = 'opportunity' AND d.created_at >= $1 AND d.created_at < $2
		  AND d.to_state IS NOT NULL AND d.from_state IS NOT NULL AND d.from_state <> d.to_state
		  AND d.to_state NOT IN ('killed') AND NOT (d.from_state = 'parked' AND d.to_state = 'candidate')
		ORDER BY array_position(ARRAY['validating','dossier','qualified','parked','candidate'], d.to_state), d.created_at DESC`, g.start, g.end)
	if err != nil {
		return err
	}
	type ch struct {
		id                                  int64
		kind, actor, from, to, reason, stmt string
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ch, error) {
		var c ch
		err := r.Scan(&c.id, &c.kind, &c.actor, &c.from, &c.to, &c.reason, &c.stmt)
		return c, err
	})
	if err != nil {
		return err
	}
	if len(list) == 0 {
		g.printf("No tracked opportunity changed stage.\n")
		return nil
	}
	shown := min(len(list), g.b.Opt.Changes)
	for _, c := range list[:shown] {
		r := ""
		if c.reason != "" {
			r = " (" + c.reason + ")"
		}
		g.printf("- Opportunity %d: %s → **%s**%s by %s %s — %s\n", c.id, c.from, c.to, r, c.actor, c.kind, truncate(oneLine(c.stmt), 120))
	}
	if len(list) > shown {
		g.printf("- …and %d more (`aoe opportunities`)\n", len(list)-shown)
	}
	g.d.Stats.Changes = shown
	return nil
}

// pairs shows AOE cards against unused B0 cards with sides drawn at random
// and every source marker hidden.
func (g *gen) pairs(ctx context.Context) error {
	rows, err := g.tx.Query(ctx, `
		SELECT bc.id, bc.card FROM baseline_card bc JOIN baseline_run br ON br.id = bc.run_id
		WHERE NOT EXISTS (SELECT 1 FROM card_pair cp WHERE cp.baseline_card_id = bc.id)
		ORDER BY br.id DESC, bc.id LIMIT $1`, g.b.Opt.Pairs)
	if err != nil {
		return err
	}
	type bcard struct {
		id   int64
		card Card
	}
	base, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (bcard, error) {
		var b bcard
		var raw []byte
		if err := r.Scan(&b.id, &raw); err != nil {
			return b, err
		}
		return b, json.Unmarshal(raw, &b.card)
	})
	if err != nil {
		return err
	}
	if len(base) == 0 {
		g.printf("No unused B0 baseline cards this week (the baseline run is pending or failed), so no pairs.\n")
		return nil
	}
	// AOE's side: this digest's cards first, then the next best opportunities.
	aoe := append([]int64{}, g.cardIDs...)
	if len(aoe) < len(base) {
		more, err := g.tx.Query(ctx, `SELECT o.id FROM opportunity o JOIN problem p ON p.id = o.problem_id
			LEFT JOIN assessment a ON a.id = o.current_assessment_id
			WHERE o.state IN ('qualified', 'dossier', 'validating', 'candidate') AND NOT (o.id = ANY($1))
			ORDER BY `+rankSQL+` LIMIT $2`, aoe, len(base)-len(aoe))
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(more, pgx.RowTo[int64])
		if err != nil {
			return err
		}
		aoe = append(aoe, ids...)
	}
	n := min(len(aoe), len(base))
	if n == 0 {
		g.printf("AOE has no opportunity in progress to compare, so no pairs.\n")
		return nil
	}
	coin := g.b.AOEFirst
	if coin == nil {
		coin = fairCoin
	}
	for i := 0; i < n; i++ {
		ac, err := LoadCard(ctx, g.chk, aoe[i])
		if err != nil {
			return err
		}
		bc := base[i].card
		side := "B"
		left, right := &bc, ac
		if coin() {
			side, left, right = "A", ac, &bc
		}
		pairID := int64(i + 1)
		if g.save {
			if err := g.tx.QueryRow(ctx, `INSERT INTO card_pair (digest_id, position, opportunity_id, baseline_card_id, aoe_side) VALUES ($1, $2, $3, $4, $5) RETURNING id`,
				g.d.ID, i+1, aoe[i], base[i].id, side).Scan(&pairID); err != nil {
				return err
			}
		}
		g.printf("**Pair %d** → `aoe pick %d A|B [--reason R]`\n\n", pairID, pairID)
		g.printf("%s\n%s\n", left.Markdown(RenderOptions{Blind: true, Label: "Card A"}), right.Markdown(RenderOptions{Blind: true, Label: "Card B"}))
	}
	g.d.Stats.Pairs = n
	return nil
}

func fairCoin() bool {
	var b [1]byte
	_, _ = crand.Read(b[:])
	return b[0]&1 == 1
}

// spotChecks shows random verified findings, newest first, that haven't
// been graded yet (A2).
func (g *gen) spotChecks(ctx context.Context) error {
	rows, err := g.tx.Query(ctx, `
		SELECT f.id, f.opportunity_id, f.claim, f.citation_spans FROM finding f
		WHERE f.verdict = 'supported' AND NOT f.retracted AND f.created_at < $2
		  AND NOT EXISTS (SELECT 1 FROM feedback fb WHERE fb.subject_type = 'finding' AND fb.subject_id = f.id AND fb.kind = 'grade')
		ORDER BY (f.created_at >= $1) DESC, random() LIMIT $3`, g.start, g.end, g.b.Opt.SpotChecks)
	if err != nil {
		return err
	}
	type fr struct {
		id    int64
		opp   *int64
		claim string
		spans []byte
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (fr, error) {
		var f fr
		err := r.Scan(&f.id, &f.opp, &f.claim, &f.spans)
		return f, err
	})
	if err != nil {
		return err
	}
	if len(list) == 0 {
		g.printf("No verified findings to grade yet.\n")
		return nil
	}
	pos := 0
	for _, f := range list {
		var spans []findingSpan
		_ = json.Unmarshal(f.spans, &spans)
		var quotes []string
		for _, s := range spans {
			if g.chk.Check(ctx, s.DocumentID, s.Start, s.End, s.Quote) {
				quotes = append(quotes, fmt.Sprintf("  > “%s” — %s", truncate(oneLine(displayQuote(s.Quote)), MaxQuote), s.URL))
			}
		}
		if len(quotes) == 0 {
			continue
		}
		pos++
		opp := ""
		if f.opp != nil {
			opp = fmt.Sprintf(" (opportunity %d)", *f.opp)
		}
		g.printf("- **Finding %d**%s: %s → `aoe grade %d supported|overclaimed|wrong`\n%s\n", f.id, opp, oneLine(f.claim), f.id, strings.Join(quotes, "\n"))
		if g.save {
			if _, err := g.tx.Exec(ctx, `INSERT INTO digest_item (digest_id, kind, position, opportunity_id, finding_id) VALUES ($1, 'spot_check', $2, $3, $4)`,
				g.d.ID, pos, f.opp, f.id); err != nil {
				return err
			}
		}
	}
	g.d.Stats.SpotChecks = pos
	return nil
}

func (g *gen) woken(ctx context.Context) error {
	rows, err := g.tx.Query(ctx, `
		SELECT d.subject_id, d.actor, d.notes, p.canonical_statement FROM decision d
		JOIN opportunity o ON o.id = d.subject_id JOIN problem p ON p.id = o.problem_id
		WHERE d.subject_type = 'opportunity' AND d.from_state IN ('parked', 'killed') AND d.to_state = 'candidate'
		  AND d.created_at >= $1 AND d.created_at < $2 ORDER BY d.created_at`, g.start, g.end)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var actor, notes, stmt string
		if err := rows.Scan(&id, &actor, &notes, &stmt); err != nil {
			return err
		}
		g.d.Stats.Woken++
		g.printf("- Opportunity %d (%s): %s — %s\n", id, actor, truncate(oneLine(notes), 120), truncate(oneLine(stmt), 120))
	}
	if g.d.Stats.Woken == 0 {
		g.printf("None.\n")
	}
	return rows.Err()
}

// killed lists this week's kills with reason codes. Duplicates merged by
// consolidation are counted, not listed.
func (g *gen) killed(ctx context.Context) error {
	const maxListed = 15
	rows, err := g.tx.Query(ctx, `
		SELECT id, reason, actor, stmt FROM (
			SELECT DISTINCT ON (d.subject_id) d.subject_id AS id, coalesce(d.reason_code, '-') AS reason, d.actor, p.canonical_statement AS stmt, d.created_at
			FROM decision d JOIN opportunity o ON o.id = d.subject_id JOIN problem p ON p.id = o.problem_id
			WHERE d.subject_type = 'opportunity' AND d.to_state = 'killed' AND o.state = 'killed' AND d.created_at >= $1 AND d.created_at < $2
			ORDER BY d.subject_id, d.reason_code IS NULL, d.created_at DESC) k
		ORDER BY reason = 'duplicate', reason, created_at`, g.start, g.end)
	if err != nil {
		return err
	}
	defer rows.Close()
	listed, dups, more := 0, 0, 0
	byReason := map[string]int{}
	for rows.Next() {
		var id int64
		var reason, actor, stmt string
		if err := rows.Scan(&id, &reason, &actor, &stmt); err != nil {
			return err
		}
		g.d.Stats.Killed++
		if reason == "duplicate" {
			dups++
			continue
		}
		byReason[reason]++
		if listed >= maxListed {
			more++
			continue
		}
		listed++
		by := ""
		if actor == "human" {
			by = ", by you"
		}
		g.printf("- Opportunity %d — `%s`%s — %s\n", id, reason, by, truncate(oneLine(stmt), 120))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if more > 0 {
		g.printf("- …and %d more (`aoe opportunities --state killed`)\n", more)
	}
	if dups > 0 {
		g.printf("- %d duplicates merged into existing opportunities\n", dups)
	}
	if g.d.Stats.Killed == 0 {
		g.printf("None.\n")
	}
	return nil
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
