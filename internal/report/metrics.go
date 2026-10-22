package report

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kwant-dbg/auto/internal/store"
)

// Metric is one acceptance criterion (docs/06 §3) with its current value.
type Metric struct {
	Code   string `json:"code"`
	Name   string `json:"name"`
	Value  string `json:"value"`
	Target string `json:"target"`
	// Met is nil when there isn't enough data to judge.
	Met *bool  `json:"met"`
	N   int    `json:"n"`
	How string `json:"how,omitempty"`
}

// Metrics computes A1–A10 over the trailing window and the engine stop-rule
// inputs (G2: baseline win rate; A7: reaching Validating).
func Metrics(ctx context.Context, st *store.Store, now time.Time, window time.Duration) ([]Metric, error) {
	since := now.Add(-window)
	db := st.DB
	var out []Metric
	yes, no := true, false
	judge := func(ok bool) *bool {
		if ok {
			return &yes
		}
		return &no
	}
	rate := func(num, den int) string {
		if den == 0 {
			return "no data"
		}
		return fmt.Sprintf("%.0f%% (%d/%d)", 100*float64(num)/float64(den), num, den)
	}

	// A1: every quote is span-checked at render time; failures are dropped.
	var checked, failed int
	if err := db.QueryRow(ctx, `SELECT coalesce(sum((stats->>'quotes_checked')::int), 0), coalesce(sum((stats->>'quotes_failed')::int), 0)
		FROM digest WHERE period_end >= $1`, since).Scan(&checked, &failed); err != nil {
		return nil, err
	}
	a1 := Metric{Code: "A1", Name: "Citation validity of displayed quotes", Target: "100%", N: checked,
		How: fmt.Sprintf("%d quotes re-checked against snapshots at render time; %d failed and were not shown", checked, failed)}
	if checked > 0 {
		a1.Value, a1.Met = "100% of displayed quotes", &yes
	} else {
		a1.Value = "no digests yet"
	}
	out = append(out, a1)

	// A2: founder grades on spot-checked findings.
	var supported, graded int
	if err := db.QueryRow(ctx, `SELECT count(*) FILTER (WHERE value = 'supported'), count(*) FROM (
		SELECT DISTINCT ON (subject_id) value FROM feedback WHERE subject_type = 'finding' AND kind = 'grade' AND created_at >= $1
		ORDER BY subject_id, created_at DESC) g`, since).Scan(&supported, &graded); err != nil {
		return nil, err
	}
	a2 := Metric{Code: "A2", Name: "Claim support (spot-checked findings graded supported)", Value: rate(supported, graded), Target: "≥ 90%", N: graded}
	if graded > 0 {
		a2.Met = judge(float64(supported) >= 0.9*float64(graded))
	}
	out = append(out, a2)

	out = append(out, Metric{Code: "A3", Name: "Dedupe quality on a labelled set", Value: "not measured", Target: "P ≥ 0.9, R ≥ 0.7",
		How: "needs a labelled pair set (eval/); consolidation decisions are logged in problem_merge for labelling"})

	// A4 and A5: card ratings (latest per card).
	var useful, dup, rated int
	if err := db.QueryRow(ctx, `SELECT count(*) FILTER (WHERE value = 'useful'), count(*) FILTER (WHERE value = 'duplicate'), count(*) FROM (
		SELECT DISTINCT ON (subject_id) value FROM feedback WHERE subject_type = 'opportunity' AND kind = 'rating' AND created_at >= $1
		ORDER BY subject_id, created_at DESC) r`, since).Scan(&useful, &dup, &rated); err != nil {
		return nil, err
	}
	a4 := Metric{Code: "A4", Name: "Duplicate cards (rated same as previous)", Value: rate(dup, rated), Target: "≤ 10%", N: rated}
	a5 := Metric{Code: "A5", Name: "Usefulness (cards worth 30 more minutes)", Value: rate(useful, rated), Target: "≥ 40%", N: rated}
	if rated > 0 {
		a4.Met = judge(float64(dup) <= 0.1*float64(rated))
		a5.Met = judge(float64(useful) >= 0.4*float64(rated))
	}
	out = append(out, a4, a5)

	// A6: blind pairwise win rate against B0 (latest pick per pair).
	wins, picks, err := WinRate(ctx, st, since)
	if err != nil {
		return nil, err
	}
	a6 := Metric{Code: "A6", Name: "Beats baseline B0 in blind pairs", Value: rate(wins, picks), Target: "≥ 65%", N: picks}
	if picks > 0 {
		a6.Met = judge(float64(wins) >= 0.65*float64(picks))
	}
	out = append(out, a6)

	// A7: opportunities reaching Validating with ≥ 50 prospects.
	var validating int
	if err := db.QueryRow(ctx, `SELECT count(DISTINCT d.subject_id) FROM decision d
		WHERE d.subject_type = 'opportunity' AND d.to_state = 'validating' AND d.created_at >= $1
		  AND (SELECT count(*) FROM prospect p WHERE p.opportunity_id = d.subject_id) >= 50`, since).Scan(&validating); err != nil {
		return nil, err
	}
	out = append(out, Metric{Code: "A7", Name: "Opportunities reaching Validating with a ≥ 50-org prospect list", Value: fmt.Sprint(validating),
		Target: "≥ 1 in 4 weeks", N: validating, Met: judge(validating >= 1)})

	// A8: founder review time.
	var minutes, timed int
	if err := db.QueryRow(ctx, `SELECT coalesce(avg(review_minutes), 0)::int, count(review_minutes) FROM digest WHERE period_end >= $1`, since).Scan(&minutes, &timed); err != nil {
		return nil, err
	}
	a8 := Metric{Code: "A8", Name: "Founder time per weekly digest", Target: "≤ 60 min", N: timed, Value: "not recorded (`aoe review-time <minutes>`)"}
	if timed > 0 {
		a8.Value, a8.Met = fmt.Sprintf("%d min average over %d digests", minutes, timed), judge(minutes <= 60)
	}
	out = append(out, a8)

	// A9: consecutive weekly cycles that ran unattended, most recent first.
	streak, err := unattendedStreak(ctx, st)
	if err != nil {
		return nil, err
	}
	out = append(out, Metric{Code: "A9", Name: "Consecutive unattended weekly cycles", Value: fmt.Sprint(streak), Target: "4", N: streak, Met: judge(streak >= 4),
		How: "a week counts when the scheduler built its digest itself and no task died in its period (failed collections reschedule themselves and don't count)"})

	// A10: cost per card shown and per opportunity advanced.
	var turns int
	var cost float64
	if err := db.QueryRow(ctx, `SELECT coalesce(sum(num_turns), 0), coalesce(sum(est_cost_usd), 0) FROM llm_call WHERE created_at >= $1`, since).Scan(&turns, &cost); err != nil {
		return nil, err
	}
	var cards, advanced int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM digest_item di JOIN digest d ON d.id = di.digest_id WHERE di.kind = 'card' AND d.period_end >= $1`, since).Scan(&cards); err != nil {
		return nil, err
	}
	if err := db.QueryRow(ctx, `SELECT count(DISTINCT subject_id) FROM decision WHERE subject_type = 'opportunity' AND created_at >= $1
		AND to_state IN ('qualified', 'dossier', 'validating', 'building', 'live') AND from_state IS DISTINCT FROM to_state`, since).Scan(&advanced); err != nil {
		return nil, err
	}
	per := func(n int) string {
		if n == 0 {
			return "–"
		}
		return fmt.Sprintf("%d turns, ≈$%.2f", turns/n, cost/float64(n))
	}
	out = append(out, Metric{Code: "A10", Name: "Cost visibility", Target: "reported", Met: &yes, N: turns,
		Value: fmt.Sprintf("%d turns, ≈$%.0f at API prices · per card shown: %s (%d cards) · per opportunity advanced: %s (%d)",
			turns, cost, per(cards), cards, per(advanced), advanced)})
	return out, nil
}

// WinRate counts AOE wins in blind pairs picked since t (latest pick per pair).
func WinRate(ctx context.Context, st *store.Store, since time.Time) (wins, picks int, err error) {
	err = st.DB.QueryRow(ctx, `SELECT count(*) FILTER (WHERE f.value = cp.aoe_side), count(*) FROM (
		SELECT DISTINCT ON (subject_id) subject_id, value FROM feedback WHERE subject_type = 'card_pair' AND kind = 'pairwise' AND created_at >= $1
		ORDER BY subject_id, created_at DESC) f JOIN card_pair cp ON cp.id = f.subject_id`, since).Scan(&wins, &picks)
	return wins, picks, err
}

func unattendedStreak(ctx context.Context, st *store.Store) (int, error) {
	rows, err := st.DB.Query(ctx, `SELECT d.generated_by = 'system'
		AND NOT EXISTS (SELECT 1 FROM task t WHERE t.state = 'dead' AND t.type <> 'collect'
		                AND t.finished_at >= d.period_start AND t.finished_at < d.period_end)
		FROM digest d ORDER BY d.period_end DESC`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var ok bool
		if err := rows.Scan(&ok); err != nil {
			return 0, err
		}
		if !ok {
			break
		}
		n++
	}
	return n, rows.Err()
}

// StopRule evaluates the pre-committed engine stop rule (docs/06 §5): at
// week 8 of operation, stop engine work if the baseline win rate is below
// 55% or no opportunity has reached Validating.
func StopRule(ctx context.Context, st *store.Store) (string, error) {
	var first *time.Time
	var weeks int
	if err := st.DB.QueryRow(ctx, `SELECT min(period_start), count(*) FROM digest`).Scan(&first, &weeks); err != nil {
		return "", err
	}
	if first == nil {
		return "Stop rule: no digests yet; the 8 weeks start with the first one.", nil
	}
	wins, picks, err := WinRate(ctx, st, *first)
	if err != nil {
		return "", err
	}
	var validating int
	if err := st.DB.QueryRow(ctx, `SELECT count(DISTINCT subject_id) FROM decision WHERE subject_type = 'opportunity' AND to_state = 'validating'`).Scan(&validating); err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Stop rule (week %d of 8): baseline win rate %s, %d opportunities reached Validating.", weeks, rateOrNone(wins, picks), validating)
	switch {
	case weeks < 8:
		b.WriteString(" Too early to apply.")
	case picks == 0 || float64(wins) < 0.55*float64(picks) || validating == 0:
		b.WriteString(" TRIGGERED: stop engine infrastructure work and switch to founder-led vertical exploration.")
	default:
		b.WriteString(" Passed: keep building.")
	}
	return b.String(), nil
}

func rateOrNone(n, d int) string {
	if d == 0 {
		return "n/a (no picks)"
	}
	return fmt.Sprintf("%.0f%% (%d/%d)", 100*float64(n)/float64(d), n, d)
}
