// Package report renders what the founder reads: opportunity cards in the
// fixed template (docs/08 §2) and the weekly digest built around them.
// AOE's cards and the B0 baseline's cards share one struct and one template
// so that blind pairs differ in content, not presentation (docs/07 §4).
package report

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
)

// Evidence is one quote on a card.
type Evidence struct {
	Tier   int    `json:"tier"` // 0-7; 0 when unknown
	Quote  string `json:"quote"`
	Source string `json:"source"`
	Date   string `json:"date,omitempty"`
	URL    string `json:"url"`
	// Claim is set for research findings: the verified claim the quote backs.
	Claim string `json:"claim,omitempty"`
	// Snapshot locates an AOE quote in its stored snapshot. B0 quotes have
	// none: they come from a one-shot answer and are shown as given.
	Snapshot *Span `json:"snapshot,omitempty"`
}

// Span is a quote's location in a raw document.
type Span struct {
	DocumentID int64 `json:"document_id"`
	Start      int   `json:"start"`
	End        int   `json:"end"`
}

// TierCount summarises the independent origins not quoted on the card.
type TierCount struct {
	Tier    int `json:"tier"`
	Origins int `json:"origins"`
}

// Path is a route to first revenue.
type Path struct {
	Name    string  `json:"path"`
	Channel string  `json:"channel,omitempty"`
	Weeks   float64 `json:"weeks_to_first_dollar"`
}

// MRR is the 12-month MRR distribution.
type MRR struct {
	P10   float64 `json:"p10"`
	P50   float64 `json:"p50"`
	P90   float64 `json:"p90"`
	PGe1k float64 `json:"p_ge_1k"`
	PGe5k float64 `json:"p_ge_5k"`
}

// Card is an opportunity card.
type Card struct {
	OpportunityID int64  `json:"opportunity_id,omitempty"`
	Segment       string `json:"segment"`
	Job           string `json:"job"`
	State         string `json:"state,omitempty"`
	FirstSeen     string `json:"first_seen,omitempty"` // ISO week
	Problem       string `json:"problem"`

	Origins     int         `json:"independent_origins"`
	Evidence    []Evidence  `json:"evidence"`
	MoreOrigins []TierCount `json:"more_origins,omitempty"`
	Findings    []Evidence  `json:"findings,omitempty"`
	Counter     []Evidence  `json:"counter_evidence"`

	Gates     map[string]string `json:"gates"` // code → PASS | FAIL | UNKNOWN | SKIPPED
	Prospects int               `json:"prospects"`
	Paths     []Path            `json:"paths"`
	MRR       *MRR              `json:"mrr,omitempty"`
	Weakest   string            `json:"weakest_link"`
	Debate    string            `json:"debate,omitempty"`

	NextExperiment string  `json:"next_experiment"`
	FounderHours   float64 `json:"founder_hours"`
	FounderUSD     float64 `json:"founder_usd"`
}

// RenderOptions control how a card is shown.
type RenderOptions struct {
	// Blind hides everything that identifies where a card came from: the
	// card id, state, first-seen week, snapshot references and commands.
	Blind bool
	// Label replaces the header's metadata in blind mode, e.g. "Card A".
	Label string
}

// Display returns the card as a reader sees it: internal evidence references
// scrubbed from model text and, when blind, everything that identifies its
// source removed — ids, state, first-seen week, snapshot pointers, source
// names, dates and AOE's uncited-origin counts. Every renderer (markdown,
// the review page) shows only Display copies.
func (c *Card) Display(blind bool) *Card {
	cp := *c
	cp.Weakest, cp.Debate, cp.NextExperiment, cp.Problem = scrub(c.Weakest), scrub(c.Debate), scrub(c.NextExperiment), scrub(c.Problem)
	if blind {
		// The two systems cite and date sources differently, and only AOE
		// counts uncited origins; normalise all three so neither side is
		// recognisable by its format.
		cp.Evidence, cp.Findings, cp.Counter = blindEvidence(c.Evidence), blindEvidence(c.Findings), blindEvidence(c.Counter)
		cp.MoreOrigins = nil
		cp.OpportunityID, cp.State, cp.FirstSeen = 0, "", ""
	}
	return &cp
}

// Markdown renders the card in the fixed template.
func (c *Card) Markdown(o RenderOptions) string {
	c = c.Display(o.Blind)
	var b strings.Builder
	head := fmt.Sprintf("%s — %s", oneLine(c.Segment), oneLine(c.Job))
	switch {
	case o.Blind:
		if o.Label != "" {
			head = o.Label + ": " + head
		}
		fmt.Fprintf(&b, "### %s\n\n", head)
	default:
		fmt.Fprintf(&b, "### %s — %s\n", head, c.State)
		fmt.Fprintf(&b, "*card %d · first seen %s*\n\n", c.OpportunityID, c.FirstSeen)
	}
	fmt.Fprintf(&b, "**Problem (normalised):** %s\n\n", oneLine(c.Problem))

	if c.Origins > 0 {
		fmt.Fprintf(&b, "**Best evidence** (tier, %d independent origins):\n", c.Origins)
	} else {
		b.WriteString("**Best evidence** (tier):\n")
	}
	if len(c.Evidence) == 0 && len(c.Findings) == 0 {
		b.WriteString("- none\n")
	}
	for _, e := range c.Evidence {
		fmt.Fprintf(&b, "- %s — %s\n", tierLabel(e.Tier), quoteLine(e, o.Blind))
	}
	for _, m := range c.MoreOrigins {
		fmt.Fprintf(&b, "- %s ×%d more independent origins\n", tierLabel(m.Tier), m.Origins)
	}
	for _, f := range c.Findings {
		fmt.Fprintf(&b, "- Research: %s — %s\n", oneLine(f.Claim), quoteLine(f, o.Blind))
	}
	b.WriteString("\n**Counter-evidence:**")
	if len(c.Counter) == 0 {
		b.WriteString(" none found\n")
	} else {
		b.WriteString("\n")
		for _, e := range c.Counter {
			if e.Claim != "" {
				fmt.Fprintf(&b, "- %s — %s\n", oneLine(e.Claim), quoteLine(e, o.Blind))
			} else {
				fmt.Fprintf(&b, "- %s\n", quoteLine(e, o.Blind))
			}
		}
	}
	fmt.Fprintf(&b, "\n**Gates:** %s\n", GateLine(c.Gates, c.Prospects))
	if len(c.Paths) > 0 {
		var ps []string
		for _, p := range c.Paths {
			s := strings.ReplaceAll(oneLine(p.Name), "_", "-")
			if p.Channel != "" {
				s += " via " + oneLine(p.Channel)
			}
			if p.Weeks > 0 {
				s += fmt.Sprintf(" (first $ in ~%.0f wks)", p.Weeks)
			}
			ps = append(ps, s)
		}
		fmt.Fprintf(&b, "**Paths:** %s\n", strings.Join(ps, " · "))
	}
	if c.MRR != nil {
		fmt.Fprintf(&b, "**12-mo MRR:** P10 %s · P50 %s · P90 %s · P(≥$1k)=%.2f · P(≥$5k)=%.2f\n",
			Money(c.MRR.P10), Money(c.MRR.P50), Money(c.MRR.P90), c.MRR.PGe1k, c.MRR.PGe5k)
	} else {
		b.WriteString("**12-mo MRR:** not assessed yet\n")
	}
	if c.Weakest != "" {
		fmt.Fprintf(&b, "**Weakest link:** %s\n", oneLine(c.Weakest))
	}
	if c.Debate != "" {
		fmt.Fprintf(&b, "**Debate:** %s\n", oneLine(c.Debate))
	}
	next := oneLine(c.NextExperiment)
	if next == "" {
		b.WriteString("**Next experiment:** none proposed yet\n")
		if !o.Blind {
			fmt.Fprintf(&b, "  `aoe decide %d advance|park|kill --reason <code>` · `aoe rate %d useful|not|duplicate`\n", c.OpportunityID, c.OpportunityID)
		}
		return b.String()
	}
	fmt.Fprintf(&b, "**Next experiment:** %s\n", next)
	cost := fmt.Sprintf("~%.0f h", c.FounderHours)
	if c.FounderUSD > 0 {
		cost += fmt.Sprintf(" · $%.0f", c.FounderUSD)
	}
	if o.Blind {
		fmt.Fprintf(&b, "  Founder cost: %s\n", cost)
	} else {
		fmt.Fprintf(&b, "  Founder cost: %s · `aoe decide %d advance|park|kill --reason <code>` · `aoe rate %d useful|not|duplicate`\n",
			cost, c.OpportunityID, c.OpportunityID)
	}
	return b.String()
}

func blindEvidence(es []Evidence) []Evidence {
	out := make([]Evidence, len(es))
	for i, e := range es {
		if h := host(e.URL); h != "" {
			e.Source = h
		}
		e.Date, e.Snapshot = "", nil
		out[i] = e
	}
	return out
}

// GateLine renders gate verdicts compactly: skipped gates (e.g. a disabled
// F1) are left out, the founder gates collapse to one mark when they agree,
// and M3 shows the prospect count.
func GateLine(gates map[string]string, prospects int) string {
	var parts, fcodes []string
	for _, code := range []string{"F1", "F2", "F3", "F4"} {
		if gates[code] != "SKIPPED" {
			fcodes = append(fcodes, code)
		}
	}
	same := len(fcodes) > 1
	for _, code := range fcodes {
		if mark(gates[code]) != mark(gates[fcodes[0]]) {
			same = false
		}
	}
	if same {
		parts = append(parts, fcodes[0]+"–"+fcodes[len(fcodes)-1]+" "+mark(gates[fcodes[0]]))
	} else {
		for _, code := range fcodes {
			parts = append(parts, code+" "+mark(gates[code]))
		}
	}
	for i := 1; i <= 6; i++ {
		code := fmt.Sprintf("M%d", i)
		p := code + " " + mark(gates[code])
		if code == "M3" && prospects > 0 {
			p += fmt.Sprintf(" (%d prospects)", prospects)
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, " · ")
}

func mark(status string) string {
	switch status {
	case "PASS":
		return "✓"
	case "FAIL":
		return "✗"
	case "SKIPPED":
		return "–"
	default:
		return "?"
	}
}

func tierLabel(t int) string {
	if t <= 0 {
		return "T?"
	}
	return fmt.Sprintf("T%d", t)
}

// MaxQuote bounds a quote's length on a card. A shortened quote stays a
// verbatim prefix of the snapshot text, marked with an ellipsis.
const MaxQuote = 240

func quoteLine(e Evidence, blind bool) string {
	q := oneLine(e.Quote)
	if r := []rune(q); len(r) > MaxQuote {
		q = strings.TrimSpace(string(r[:MaxQuote])) + "…"
	}
	src := oneLine(e.Source)
	if e.Date != "" {
		src += ", " + e.Date
	}
	line := fmt.Sprintf("“%s” — %s", q, src)
	switch {
	case !blind && e.Snapshot != nil:
		line += fmt.Sprintf(" [snapshot %d](%s)", e.Snapshot.DocumentID, e.URL)
	case e.URL != "":
		line += fmt.Sprintf(" (%s)", e.URL)
	}
	return line
}

// Money formats dollars compactly: $0, $297, $1.4k, $12k.
func Money(v float64) string {
	switch a := math.Abs(v); {
	case a >= 10000:
		return fmt.Sprintf("$%.0fk", v/1000)
	case a >= 1000:
		return fmt.Sprintf("$%.1fk", v/1000)
	default:
		return fmt.Sprintf("$%.0f", v)
	}
}

// oneLine collapses whitespace so model text can't break the template.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// internalRef matches AOE's evidence references in model text (O123 for an
// observation, F45 for a finding, ranges like O695–O847, lists like O1, O2).
var (
	internalRef = regexp.MustCompile(`\b[OF]\d+(?:\s*[–-]\s*[OF]?\d+)?(?:\s*,\s*[OF]\d+(?:\s*[–-]\s*[OF]?\d+)?)*\b`)
	parenRef    = regexp.MustCompile(`\s*\(\s*` + internalRef.String() + `\s*\)`)
)

// scrub removes internal references, which mean nothing to a reader and
// would give a blind card away: parenthesised ones are dropped, others read
// as "the cited evidence".
func scrub(s string) string {
	s = parenRef.ReplaceAllString(s, "")
	return internalRef.ReplaceAllString(s, "the cited evidence")
}

// sortTierCounts orders the "more origins" lines strongest first.
func sortTierCounts(m map[int]int) []TierCount {
	var out []TierCount
	for t, n := range m {
		out = append(out, TierCount{Tier: t, Origins: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tier < out[j].Tier })
	return out
}
