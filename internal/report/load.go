package report

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kwant-dbg/auto/internal/store"
)

// Checker re-runs the span check on every quote before it is shown (A1:
// unverifiable quotes are never displayed). It reads each snapshot once.
type Checker struct {
	Store   *store.Store
	cache   map[int64][]byte
	Checked int // quotes checked
	Failed  int // quotes dropped because the snapshot no longer matched
}

// NewChecker returns a span checker over the store's snapshots.
func NewChecker(st *store.Store) *Checker { return &Checker{Store: st, cache: map[int64][]byte{}} }

// Check reports whether quote is exactly the bytes [start, end) of the
// document's snapshot.
func (c *Checker) Check(ctx context.Context, docID int64, start, end int, quote string) bool {
	c.Checked++
	content, ok := c.cache[docID]
	if !ok {
		var rel string
		if err := c.Store.DB.QueryRow(ctx, `SELECT blob_path FROM raw_document WHERE id = $1`, docID).Scan(&rel); err == nil {
			content, _ = c.Store.Blobs.Get(rel) // a missing or corrupt blob fails the check
		}
		c.cache[docID] = content
	}
	if start < 0 || end <= start || end > len(content) || string(content[start:end]) != quote {
		c.Failed++
		return false
	}
	return true
}

// Card limits.
const (
	maxQuotedOrigins = 3
	maxFindings      = 2
	maxCounter       = 2
)

// LoadCard builds an AOE opportunity's card from the store. Every quote on
// it has passed the checker.
func LoadCard(ctx context.Context, chk *Checker, oppID int64) (*Card, error) {
	db := chk.Store.DB
	c := &Card{OpportunityID: oppID, Gates: map[string]string{}}
	var problemID int64
	var created time.Time
	var assessmentID *int64
	err := db.QueryRow(ctx, `SELECT o.segment, o.state, o.created_at, o.current_assessment_id, p.id, p.canonical_statement, p.independent_origin_count
		FROM opportunity o JOIN problem p ON p.id = o.problem_id WHERE o.id = $1`, oppID).
		Scan(&c.Segment, &c.State, &created, &assessmentID, &problemID, &c.Problem, &c.Origins)
	if err != nil {
		return nil, fmt.Errorf("opportunity %d: %w", oppID, err)
	}
	c.FirstSeen = ISOWeek(created)
	c.Job = JobOf(c.Problem)

	if err := c.loadEvidence(ctx, chk, problemID); err != nil {
		return nil, err
	}
	if err := c.loadFindings(ctx, chk); err != nil {
		return nil, err
	}

	rows, err := db.Query(ctx, `SELECT gate_code, status FROM gate_status WHERE opportunity_id = $1`, oppID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var code, status string
		if err := rows.Scan(&code, &status); err != nil {
			rows.Close()
			return nil, err
		}
		c.Gates[code] = status
	}
	rows.Close()
	if err := db.QueryRow(ctx, `SELECT count(*) FROM prospect WHERE opportunity_id = $1`, oppID).Scan(&c.Prospects); err != nil {
		return nil, err
	}
	if assessmentID != nil {
		if err := c.loadAssessment(ctx, db, *assessmentID); err != nil {
			return nil, err
		}
	}
	return c, c.loadDebate(ctx, db)
}

// loadEvidence quotes the strongest observation from each of the best
// independent origins and counts the rest by tier.
func (c *Card) loadEvidence(ctx context.Context, chk *Checker, problemID int64) error {
	rows, err := chk.Store.DB.Query(ctx, `
		SELECT DISTINCT ON (o.origin_key) o.origin_key, o.job, o.evidence_tier, o.quote, o.raw_document_id, o.span_start, o.span_end,
		       s.name, r.canonical_url, coalesce(r.published_at, r.fetched_at)
		FROM problem_observation po
		JOIN observation o ON o.id = po.observation_id
		JOIN raw_document r ON r.id = o.raw_document_id
		JOIN source s ON s.id = r.source_id
		WHERE po.problem_id = $1 AND NOT o.superseded AND NOT o.injection_suspect
		ORDER BY o.origin_key, o.evidence_tier, o.specificity DESC, o.id`, problemID)
	if err != nil {
		return err
	}
	type ob struct {
		job  string
		e    Evidence
		span Span
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ob, error) {
		var o ob
		var origin string
		var at time.Time
		var source string
		err := r.Scan(&origin, &o.job, &o.e.Tier, &o.e.Quote, &o.span.DocumentID, &o.span.Start, &o.span.End, &source, &o.e.URL, &at)
		// Cite the site, not AOE's source name: it reads better and keeps
		// blind cards blind.
		o.e.Source = host(o.e.URL)
		o.e.Date = at.Format("2006-01-02")
		return o, err
	})
	if err != nil {
		return err
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].e.Tier < all[j].e.Tier })
	more := map[int]int{}
	for _, o := range all {
		if c.Job == "" {
			c.Job = o.job // fallback: the job as stated by the strongest evidence
		}
		if len(c.Evidence) < maxQuotedOrigins && chk.Check(ctx, o.span.DocumentID, o.span.Start, o.span.End, o.e.Quote) {
			span := o.span
			o.e.Snapshot = &span
			o.e.Quote = displayQuote(o.e.Quote)
			c.Evidence = append(c.Evidence, o.e)
			continue
		}
		more[o.e.Tier]++
	}
	c.MoreOrigins = sortTierCounts(more)
	return nil
}

type findingSpan struct {
	DocumentID int64  `json:"raw_document_id"`
	Start      int    `json:"start"`
	End        int    `json:"end"`
	Quote      string `json:"quote"`
	URL        string `json:"url"`
}

// loadFindings adds verified research findings: supporting ones as
// evidence, contradicting ones as counter-evidence.
func (c *Card) loadFindings(ctx context.Context, chk *Checker) error {
	rows, err := chk.Store.DB.Query(ctx, `
		SELECT f.claim, f.stance, f.citation_spans, f.created_at FROM finding f
		WHERE f.opportunity_id = $1 AND NOT f.retracted AND f.stance <> 'neutral'
		  AND (f.verdict = 'supported' OR (f.stance = 'contradicts' AND f.verdict = 'partial'))
		ORDER BY f.verdict = 'supported' DESC, f.id DESC`, c.OpportunityID)
	if err != nil {
		return err
	}
	type fr struct {
		claim, stance string
		spans         []byte
		at            time.Time
	}
	fs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (fr, error) {
		var f fr
		err := r.Scan(&f.claim, &f.stance, &f.spans, &f.at)
		return f, err
	})
	if err != nil {
		return err
	}
	for _, f := range fs {
		into := &c.Findings
		limit := maxFindings
		if f.stance == "contradicts" {
			into, limit = &c.Counter, maxCounter
		}
		if len(*into) >= limit {
			continue
		}
		var spans []findingSpan
		if json.Unmarshal(f.spans, &spans) != nil {
			continue
		}
		for _, s := range spans {
			if !chk.Check(ctx, s.DocumentID, s.Start, s.End, s.Quote) {
				continue
			}
			*into = append(*into, Evidence{Claim: f.claim, Quote: displayQuote(s.Quote), Source: host(s.URL), URL: s.URL,
				Date: f.at.Format("2006-01-02"), Snapshot: &Span{DocumentID: s.DocumentID, Start: s.Start, End: s.End}})
			break
		}
	}
	return nil
}

func (c *Card) loadAssessment(ctx context.Context, db queryer, id int64) error {
	var m MRR
	var weakest string
	var paths, exps []byte
	if err := db.QueryRow(ctx, `SELECT ev_p10, ev_p50, ev_p90, p_ge_1k, p_ge_5k, coalesce(weakest_factor, ''), paths, next_experiments
		FROM assessment WHERE id = $1`, id).Scan(&m.P10, &m.P50, &m.P90, &m.PGe1k, &m.PGe5k, &weakest, &paths, &exps); err != nil {
		return err
	}
	c.MRR = &m
	var ps []Path
	if json.Unmarshal(paths, &ps) == nil {
		c.Paths = ps
	}
	if weakest != "" {
		c.Weakest = weakest
		var lo, base, hi float64
		var unit string
		var clamped bool
		var tier *int
		err := db.QueryRow(ctx, `SELECT low, base, high, unit, clamped_to_prior, evidence_tier_max FROM factor_estimate
			WHERE assessment_id = $1 AND factor_code = $2`, id, weakest).Scan(&lo, &base, &hi, &unit, &clamped, &tier)
		switch {
		case err == nil:
			ev := "no evidence → wide prior"
			if !clamped && tier != nil {
				ev = fmt.Sprintf("best evidence T%d", *tier)
			} else if !clamped {
				ev = "cited evidence"
			}
			c.Weakest = fmt.Sprintf("%s %s–%s %s, base %s (%s)", weakest, num(lo), num(hi), unit, num(base), ev)
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
	}
	var es []struct {
		Hypothesis string  `json:"hypothesis"`
		Method     string  `json:"method"`
		Hours      float64 `json:"founder_hours"`
		Kill       string  `json:"kill_threshold"`
	}
	if json.Unmarshal(exps, &es) == nil && len(es) > 0 {
		e := es[0]
		c.NextExperiment = strings.TrimRight(strings.TrimSpace(e.Method), ".")
		if e.Kill != "" {
			c.NextExperiment += "; kill if " + strings.TrimSpace(e.Kill)
		}
		c.FounderHours = e.Hours
	}
	return nil
}

// loadDebate summarises the latest judged debate by its strongest
// surviving Bear objection and how the Judge ruled.
func (c *Card) loadDebate(ctx context.Context, db queryer) error {
	var debateID int64
	var bull, bear, open int
	err := db.QueryRow(ctx, `SELECT d.id,
		       count(*) FILTER (WHERE r.verdict = 'bull_holds'), count(*) FILTER (WHERE r.verdict = 'bear_holds'),
		       count(*) FILTER (WHERE r.verdict = 'unresolved')
		FROM debate d LEFT JOIN debate_ruling r ON r.debate_id = d.id
		WHERE d.opportunity_id = $1 AND d.status = 'judged' GROUP BY d.id ORDER BY d.id DESC LIMIT 1`, c.OpportunityID).
		Scan(&debateID, &bull, &bear, &open)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var objection string
	err = db.QueryRow(ctx, `SELECT text FROM objection WHERE debate_id = $1 ORDER BY severity DESC, id LIMIT 1`, debateID).Scan(&objection)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	tally := fmt.Sprintf("Judge: Bear holds %d, Bull holds %d, unresolved %d", bear, bull, open)
	if objection == "" {
		c.Debate = "no Bear objection survived verification (" + tally + ")"
	} else {
		c.Debate = fmt.Sprintf("strongest surviving Bear objection: “%s” (%s)", oneLine(objection), tally)
	}
	return nil
}

type queryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

var jobRe = regexp.MustCompile(`(?i)\bneeds? to (.+?)(?:,? but |, causing |\.$|$)`)

// JobOf takes the job from a normalised problem statement ("<actor> need to
// <job> but <obstacle>, causing <consequence>"), so a card's header names
// the whole problem rather than one observation's specifics.
func JobOf(statement string) string {
	m := jobRe.FindStringSubmatch(oneLine(statement))
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// num formats a factor value to 3 significant figures without exponents:
// 1200, 250, 0.05.
func num(v float64) string {
	return strconv.FormatFloat(roundSig(v, 3), 'f', -1, 64)
}

func roundSig(v float64, digits int) float64 {
	if v == 0 {
		return 0
	}
	p := math.Pow(10, float64(digits)-math.Ceil(math.Log10(math.Abs(v))))
	return math.Round(v*p) / p
}

// ISOWeek formats t as an ISO week, e.g. 2026-W43.
func ISOWeek(t time.Time) string {
	y, w := t.UTC().ISOWeek()
	return fmt.Sprintf("%d-W%02d", y, w)
}

// displayQuote undoes JSON escaping in quotes taken from JSON snapshots
// (marketplace listings and reviews), so \n and \" read naturally.
func displayQuote(q string) string {
	if !strings.Contains(q, `\`) {
		return q
	}
	var s string
	if json.Unmarshal([]byte(`"`+q+`"`), &s) == nil {
		return s
	}
	return q
}

func host(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return strings.TrimPrefix(u.Host, "www.")
	}
	return raw
}
