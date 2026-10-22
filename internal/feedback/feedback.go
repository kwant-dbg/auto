// Package feedback records the founder's responses to the digest: card
// ratings, blind pair picks, finding grades, answers to questions only they
// can answer, and stage decisions (RPT-4). Everything is append-only.
package feedback

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/kwant-dbg/auto/internal/lifecycle"
	"github.com/kwant-dbg/auto/internal/store"
)

// Ratings a card can get. "duplicate" means "same as a card I've seen" (A4).
var Ratings = []string{"useful", "not_useful", "duplicate"}

// Grades a spot-checked finding can get (A2).
var Grades = []string{"supported", "overclaimed", "wrong"}

// execer is a pool or a transaction.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func insert(ctx context.Context, q execer, subjectType string, id int64, kind, value, reason string) error {
	_, err := q.Exec(ctx, `INSERT INTO feedback (subject_type, subject_id, kind, value, reason_code) VALUES ($1, $2, $3, $4, NULLIF($5, ''))`,
		subjectType, id, kind, value, reason)
	return err
}

func exists(ctx context.Context, st *store.Store, table string, id int64) error {
	var ok bool
	if err := st.DB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+table+` WHERE id = $1)`, id).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no %s %d", strings.ReplaceAll(table, "_", " "), id)
	}
	return nil
}

// Rate records whether a card was worth 30 more minutes (A5), or a
// duplicate (A4), and optionally its novelty: "would I have thought of this
// myself?", 1 (obviously) to 5 (never).
func Rate(ctx context.Context, st *store.Store, oppID int64, value, reason string, novelty int) error {
	if value == "not" {
		value = "not_useful"
	}
	if !slices.Contains(Ratings, value) {
		return fmt.Errorf("rating must be useful, not or duplicate, got %q", value)
	}
	if novelty < 0 || novelty > 5 {
		return errors.New("novelty must be 1-5")
	}
	if err := exists(ctx, st, "opportunity", oppID); err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, st.DB, func(tx pgx.Tx) error {
		if err := insert(ctx, tx, "opportunity", oppID, "rating", value, reason); err != nil {
			return err
		}
		if novelty > 0 {
			return insert(ctx, tx, "opportunity", oppID, "novelty", strconv.Itoa(novelty), "")
		}
		return nil
	})
}

// PickResult says how a pick resolved once recorded.
type PickResult struct {
	AOEWon bool
}

// Pick records which card of a blind pair the founder would rather spend an
// hour on. The latest pick for a pair counts.
func Pick(ctx context.Context, st *store.Store, pairID int64, side, reason string) (PickResult, error) {
	side = strings.ToUpper(strings.TrimSpace(side))
	if side != "A" && side != "B" {
		return PickResult{}, fmt.Errorf("pick A or B, got %q", side)
	}
	var aoeSide string
	if err := st.DB.QueryRow(ctx, `SELECT aoe_side FROM card_pair WHERE id = $1`, pairID).Scan(&aoeSide); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PickResult{}, fmt.Errorf("no pair %d", pairID)
		}
		return PickResult{}, err
	}
	if err := insert(ctx, st.DB, "card_pair", pairID, "pairwise", side, reason); err != nil {
		return PickResult{}, err
	}
	return PickResult{AOEWon: side == aoeSide}, nil
}

// Grade records a spot-check verdict on a finding.
func Grade(ctx context.Context, st *store.Store, findingID int64, value, reason string) error {
	if !slices.Contains(Grades, value) {
		return fmt.Errorf("grade must be one of %s, got %q", strings.Join(Grades, ", "), value)
	}
	if err := exists(ctx, st, "finding", findingID); err != nil {
		return err
	}
	return insert(ctx, st.DB, "finding", findingID, "grade", value, reason)
}

// Answer records the founder's answer to a question and re-runs the
// lifecycle, since an answered kill question can unblock a stage.
func Answer(ctx context.Context, st *store.Store, questionID int64, text string) (newState string, err error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", errors.New("answer is empty")
	}
	err = pgx.BeginFunc(ctx, st.DB, func(tx pgx.Tx) error {
		var oppID int64
		if err := tx.QueryRow(ctx, `UPDATE question SET status = 'answered', answer = $2, answered_at = now() WHERE id = $1 RETURNING opportunity_id`,
			questionID, "Founder: "+text).Scan(&oppID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("no question %d", questionID)
			}
			return err
		}
		newState, err = lifecycle.Advance(ctx, tx, oppID, "human", "answer", fmt.Sprintf("question %d answered by the founder", questionID))
		return err
	})
	return newState, err
}

// ReviewMinutes records founder time spent on a week's digest (A8). An
// empty week means the latest digest.
func ReviewMinutes(ctx context.Context, st *store.Store, week string, minutes int) (string, error) {
	if minutes < 0 {
		return "", errors.New("minutes must be ≥ 0")
	}
	err := st.DB.QueryRow(ctx, `UPDATE digest SET review_minutes = $2
		WHERE id = (SELECT id FROM digest WHERE $1 = '' OR week = $1 ORDER BY period_end DESC LIMIT 1) RETURNING week`, week, minutes).Scan(&week)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errors.New("no digest yet")
	}
	return week, err
}

// Decision is a founder stage decision.
type Decision struct {
	OpportunityID int64
	Action        string // advance | park | kill | revive
	Reason        string
	Notes         string
	Wake          []string // park: date:YYYY-MM-DD, evidence:+N, evidence:N, competitor:..., capability:..., regulation:...
	Override      bool     // advance even though a transition check fails
}

// advanceTo is the next stage a founder can advance an opportunity to.
var advanceTo = map[string]string{
	"candidate": "qualified", "qualified": "validating", "dossier": "validating",
	"validating": "building", "building": "live",
}

// MinPGe1k is the P(MRR ≥ $1k at 12 months) a dossier needs to move to
// Validating (docs/05 §4; tune with outcomes).
const MinPGe1k = 0.15

// Decide applies a founder decision. Advancing checks the transition rules
// of docs/05 §4 and refuses, listing what's missing, unless Override is set;
// overrides are recorded in the decision notes.
func Decide(ctx context.Context, st *store.Store, d Decision, now time.Time) (from, to string, err error) {
	err = pgx.BeginFunc(ctx, st.DB, func(tx pgx.Tx) error {
		var problemID int64
		var origins int
		if err := tx.QueryRow(ctx, `SELECT o.state, o.problem_id, p.independent_origin_count FROM opportunity o JOIN problem p ON p.id = o.problem_id
			WHERE o.id = $1 FOR UPDATE OF o`, d.OpportunityID).Scan(&from, &problemID, &origins); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("no opportunity %d", d.OpportunityID)
			}
			return err
		}
		t := lifecycle.Transition{From: from, Actor: "human", Type: "human", Reason: d.Reason, Notes: d.Notes}
		switch d.Action {
		case "advance":
			next, ok := advanceTo[from]
			if !ok {
				return fmt.Errorf("opportunity %d is %s; it can't be advanced (revive it first if parked or killed)", d.OpportunityID, from)
			}
			missing, err := checks(ctx, tx, d.OpportunityID, from, next)
			if err != nil {
				return err
			}
			if len(missing) > 0 && !d.Override {
				return fmt.Errorf("%s → %s needs: %s (use --override to advance anyway)", from, next, strings.Join(missing, "; "))
			}
			if len(missing) > 0 {
				t.Notes = strings.TrimSpace(t.Notes + " [override: " + strings.Join(missing, "; ") + "]")
			}
			t.To = next
		case "park":
			if from == "parked" || from == "killed" {
				return fmt.Errorf("opportunity %d is already %s", d.OpportunityID, from)
			}
			if d.Reason == "" {
				return errors.New("parking needs --reason (a kill reason code or not_now)")
			}
			if d.Reason != "not_now" && !slices.Contains(lifecycle.KillReasons, d.Reason) {
				return fmt.Errorf("unknown reason %q; use not_now or one of %s", d.Reason, strings.Join(lifecycle.KillReasons, ", "))
			}
			wake, err := ParseWake(d.Wake, problemID, origins)
			if err != nil {
				return err
			}
			if !slices.ContainsFunc(wake, lifecycle.Wake.Checkable) {
				return errors.New("parking needs at least one machine-checkable wake condition: --wake date:YYYY-MM-DD or --wake evidence:+N")
			}
			t.To, t.Wake = "parked", wake
		case "kill":
			if from == "killed" {
				return fmt.Errorf("opportunity %d is already killed", d.OpportunityID)
			}
			if !slices.Contains(lifecycle.KillReasons, d.Reason) {
				return fmt.Errorf("kill needs --reason, one of %s", strings.Join(lifecycle.KillReasons, ", "))
			}
			t.To = "killed"
		case "revive":
			if from != "parked" && from != "killed" {
				return fmt.Errorf("opportunity %d is %s, not parked or killed", d.OpportunityID, from)
			}
			if t.Reason == "" {
				t.Reason = "human_override"
			}
			t.To = "candidate"
		default:
			return fmt.Errorf("unknown action %q (advance, park, kill, revive)", d.Action)
		}
		to = t.To
		return lifecycle.Move(ctx, tx, d.OpportunityID, t)
	})
	return from, to, err
}

// checks lists the unmet conditions for a founder-approved transition.
func checks(ctx context.Context, tx pgx.Tx, oppID int64, from, to string) ([]string, error) {
	gates, err := lifecycle.Gates(ctx, tx, oppID)
	if err != nil {
		return nil, err
	}
	var prospects int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM prospect WHERE opportunity_id = $1`, oppID).Scan(&prospects); err != nil {
		return nil, err
	}
	var missing []string
	need := func(codes ...string) {
		for _, c := range codes {
			if gates[c] != "PASS" {
				missing = append(missing, fmt.Sprintf("%s PASS (is %s)", c, orUnknown(gates[c])))
			}
		}
	}
	switch {
	case from == "qualified" && to == "validating": // early probe
		need("M1", "M2")
		if prospects < 20 {
			missing = append(missing, fmt.Sprintf("≥ 20 prospects (has %d)", prospects))
		}
	case from == "dossier" && to == "validating":
		for code, s := range gates {
			if s == "FAIL" {
				missing = append(missing, code+" not FAIL")
			}
		}
		need("M1", "M2", "M3")
		if prospects < 50 {
			missing = append(missing, fmt.Sprintf("≥ 50 prospects (has %d)", prospects))
		}
		var p1k *float64
		if err := tx.QueryRow(ctx, `SELECT a.p_ge_1k FROM opportunity o LEFT JOIN assessment a ON a.id = o.current_assessment_id WHERE o.id = $1`, oppID).Scan(&p1k); err != nil {
			return nil, err
		}
		if p1k == nil || *p1k < MinPGe1k {
			missing = append(missing, fmt.Sprintf("P(≥$1k) ≥ %.2f (is %s)", MinPGe1k, fmtProb(p1k)))
		}
	case from == "validating" && to == "building":
		missing = append(missing, "validation success thresholds met (experiments are not tracked yet: confirm by hand)")
	}
	slices.Sort(missing)
	return missing, nil
}

var wakeRe = regexp.MustCompile(`^(date|evidence|competitor|capability|regulation):(.+)$`)

// ParseWake parses --wake values into conditions. evidence:+N means N more
// independent origins than the problem has now; evidence:N is absolute.
func ParseWake(values []string, problemID int64, origins int) ([]lifecycle.Wake, error) {
	var out []lifecycle.Wake
	for _, v := range values {
		m := wakeRe.FindStringSubmatch(strings.TrimSpace(v))
		if m == nil {
			return nil, fmt.Errorf("bad wake condition %q (date:YYYY-MM-DD, evidence:+N, competitor:..., capability:..., regulation:...)", v)
		}
		arg := strings.TrimSpace(m[2])
		switch m[1] {
		case "date":
			if _, err := time.Parse("2006-01-02", arg); err != nil {
				return nil, fmt.Errorf("wake date %q: want YYYY-MM-DD", arg)
			}
			out = append(out, lifecycle.Wake{Type: "date", Date: arg})
		case "evidence":
			n, err := strconv.Atoi(strings.TrimPrefix(arg, "+"))
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("wake evidence %q: want +N or N", arg)
			}
			if strings.HasPrefix(arg, "+") {
				n += origins
			}
			out = append(out, lifecycle.Wake{Type: "evidence_count", ProblemID: problemID, MinIndependentOrigins: n})
		case "competitor":
			out = append(out, lifecycle.Wake{Type: "competitor_event", Text: arg})
		default:
			out = append(out, lifecycle.Wake{Type: m[1], Text: arg})
		}
	}
	return out, nil
}

func orUnknown(s string) string {
	if s == "" {
		return "UNKNOWN"
	}
	return s
}

func fmtProb(p *float64) string {
	if p == nil {
		return "not assessed"
	}
	return fmt.Sprintf("%.2f", *p)
}
