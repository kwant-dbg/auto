package report

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kwant-dbg/auto/internal/store"
)

// Export is a stored digest in structured form, for renderers other than
// markdown (the review page). Cards come from the snapshots taken when the
// digest was built; pair cards are blind Display copies placed on the side
// the digest drew, so nothing in an Export says which side is AOE.
type Export struct {
	ID          int64     `json:"id"`
	Week        string    `json:"week"`
	PeriodStart time.Time `json:"period_start"`
	PeriodEnd   time.Time `json:"period_end"`
	Stats       Stats     `json:"stats"`
	// Sections holds the markdown body of the digest's text sections,
	// keyed by section number ("0" health, "1" decisions, "3" changes,
	// "6" woken, "7" killed).
	Sections   map[string]string `json:"sections"`
	Cards      []*Card           `json:"cards"`
	Pairs      []ExportPair      `json:"pairs"`
	SpotChecks []ExportFinding   `json:"spot_checks"`
}

// ExportPair is a blind pair.
type ExportPair struct {
	ID int64 `json:"id"`
	A  *Card `json:"a"`
	B  *Card `json:"b"`
}

// ExportFinding is a finding shown for spot-checking.
type ExportFinding struct {
	ID            int64      `json:"id"`
	OpportunityID *int64     `json:"opportunity_id,omitempty"`
	Claim         string     `json:"claim"`
	Quotes        []Evidence `json:"quotes"`
}

// ExportDigest loads a stored digest ("latest" or an ISO week).
func ExportDigest(ctx context.Context, st *store.Store, week string) (*Export, error) {
	x := &Export{Sections: map[string]string{}}
	var md string
	var stats []byte
	err := st.DB.QueryRow(ctx, `SELECT id, week, period_start, period_end, markdown, stats FROM digest
		WHERE $1 IN ('', 'latest') OR week = $1 ORDER BY period_end DESC LIMIT 1`, week).
		Scan(&x.ID, &x.Week, &x.PeriodStart, &x.PeriodEnd, &md, &stats)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("no digest for %q", week)
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(stats, &x.Stats)
	x.Sections = splitSections(md)

	rows, err := st.DB.Query(ctx, `SELECT opportunity_id, snapshot FROM digest_item WHERE digest_id = $1 AND kind = 'card' ORDER BY position`, x.ID)
	if err != nil {
		return nil, err
	}
	shown := map[int64]*Card{}
	for rows.Next() {
		var id int64
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return nil, err
		}
		var c Card
		if err := json.Unmarshal(raw, &c); err != nil {
			rows.Close()
			return nil, fmt.Errorf("card %d snapshot: %w", id, err)
		}
		shown[id] = &c
		x.Cards = append(x.Cards, c.Display(false))
	}
	rows.Close()

	chk := NewChecker(st)
	prows, err := st.DB.Query(ctx, `SELECT cp.id, cp.opportunity_id, cp.aoe_side, bc.card FROM card_pair cp
		JOIN baseline_card bc ON bc.id = cp.baseline_card_id WHERE cp.digest_id = $1 ORDER BY cp.position`, x.ID)
	if err != nil {
		return nil, err
	}
	type pr struct {
		id, opp int64
		side    string
		base    []byte
	}
	prs, err := pgx.CollectRows(prows, func(r pgx.CollectableRow) (pr, error) {
		var p pr
		err := r.Scan(&p.id, &p.opp, &p.side, &p.base)
		return p, err
	})
	if err != nil {
		return nil, err
	}
	for _, p := range prs {
		aoe := shown[p.opp]
		if aoe == nil {
			// Paired without being a card: rebuild it (current state).
			if aoe, err = LoadCard(ctx, chk, p.opp); err != nil {
				return nil, err
			}
		}
		var base Card
		if err := json.Unmarshal(p.base, &base); err != nil {
			return nil, err
		}
		a, b := aoe.Display(true), base.Display(true)
		if p.side == "B" {
			a, b = b, a
		}
		x.Pairs = append(x.Pairs, ExportPair{ID: p.id, A: a, B: b})
	}

	frows, err := st.DB.Query(ctx, `SELECT f.id, f.opportunity_id, f.claim, f.citation_spans FROM digest_item di
		JOIN finding f ON f.id = di.finding_id WHERE di.digest_id = $1 AND di.kind = 'spot_check' ORDER BY di.position`, x.ID)
	if err != nil {
		return nil, err
	}
	type fr struct {
		f     ExportFinding
		spans []byte
	}
	frs, err := pgx.CollectRows(frows, func(r pgx.CollectableRow) (fr, error) {
		var f fr
		err := r.Scan(&f.f.ID, &f.f.OpportunityID, &f.f.Claim, &f.spans)
		return f, err
	})
	if err != nil {
		return nil, err
	}
	for _, f := range frs {
		var spans []findingSpan
		_ = json.Unmarshal(f.spans, &spans)
		for _, s := range spans {
			if chk.Check(ctx, s.DocumentID, s.Start, s.End, s.Quote) {
				f.f.Quotes = append(f.f.Quotes, Evidence{Quote: displayQuote(s.Quote), Source: host(s.URL), URL: s.URL})
			}
		}
		f.f.Claim = scrub(f.f.Claim)
		x.SpotChecks = append(x.SpotChecks, f.f)
	}
	return x, nil
}

// splitSections maps "## N. Title" headings to the markdown beneath them.
func splitSections(md string) map[string]string {
	out := map[string]string{}
	var key string
	var b strings.Builder
	flush := func() {
		if key != "" {
			out[key] = strings.TrimSpace(b.String())
		}
		b.Reset()
	}
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(line, "## ") {
			flush()
			key, _, _ = strings.Cut(strings.TrimPrefix(line, "## "), ".")
			continue
		}
		if strings.HasPrefix(line, "---") {
			flush()
			key = ""
			continue
		}
		if key != "" {
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	flush()
	return out
}
