package report

import (
	"bytes"
	_ "embed"
	"fmt"
	"html"
	"html/template"
	"math"
	"regexp"
	"strings"

	"github.com/kwant-dbg/auto/internal/lifecycle"
)

//go:embed page.html.tmpl
var pageTemplate string

// The review page: the digest as an interactive page the founder answers in
// place. It is page content for an Artifact (no doctype, html, head or body;
// the publisher wraps it). Answers are saved by the page itself, to the
// artifact's own store, as documents in a "feedback" collection keyed
// rate-<opportunity>, decide-<opportunity>, pick-<pair>, grade-<finding> and
// review-<week>, which are read back and recorded with the feedback package.

type pageData struct {
	*Export
	WeekNumber string
	Period     string
	Health     []string
	Scale      float64 // shared MRR axis, dollars
}

type cardData struct {
	Card  *Card
	Blind bool
	Label string
	Scale float64
}

type gateView struct {
	Code, Status, Mark, Note, Title string
}

var page = template.Must(template.New("page").Funcs(template.FuncMap{
	"cardView": func(c *Card, p *pageData, blind bool, label string) cardData {
		return cardData{Card: c, Blind: blind, Label: label, Scale: p.Scale}
	},
	"tier": tierLabel,
	"tierClass": func(t int) string {
		switch {
		case t <= 0:
			return "t-unk"
		case t <= 2:
			return "t-strong"
		case t <= 4:
			return "t-mid"
		default:
			return "t-weak"
		}
	},
	"money":    Money,
	"prob":     func(p float64) string { return fmt.Sprintf("%.2f", p) },
	"pathName": func(s string) string { return strings.ReplaceAll(oneLine(s), "_", "-") },
	"gates":    gateViews,
	"spanStyle": func(lo, hi, scale float64) template.CSS {
		l, h := pos(lo, scale), pos(hi, scale)
		return template.CSS(fmt.Sprintf("left: %.2f%%; width: %.2f%%", l, math.Max(h-l, 0.4)))
	},
	"pointStyle":  func(v, scale float64) template.CSS { return template.CSS(fmt.Sprintf("left: %.2f%%", pos(v, scale))) },
	"killReasons": func() []string { return lifecycle.KillReasons },
	"md":          mdHTML,
}).Parse(pageTemplate))

// RenderPage renders a digest as the review page.
func RenderPage(x *Export) (string, error) {
	d := &pageData{Export: x, Scale: axisMax(x)}
	if _, w, ok := strings.Cut(x.Week, "-W"); ok {
		d.WeekNumber = strings.TrimLeft(w, "0")
	}
	d.Period = fmt.Sprintf("%s – %s", x.PeriodStart.UTC().Format("2 Jan"), x.PeriodEnd.UTC().Format("2 Jan 2006"))
	if first, _, _ := strings.Cut(x.Sections["0"], "\n"); first != "" {
		for _, part := range strings.Split(first, " · ") {
			d.Health = append(d.Health, strings.TrimSpace(part))
		}
	}
	var b bytes.Buffer
	if err := page.Execute(&b, d); err != nil {
		return "", err
	}
	return b.String(), nil
}

func pos(v, scale float64) float64 {
	if scale <= 0 {
		return 0
	}
	return math.Min(math.Max(100*v/scale, 0), 100)
}

// axisMax is a round dollar figure at or above every P90 on the page, so all
// revenue bars share one scale.
func axisMax(x *Export) float64 {
	top := 1000.0
	see := func(c *Card) {
		if c != nil && c.MRR != nil && c.MRR.P90 > top {
			top = c.MRR.P90
		}
	}
	for _, c := range x.Cards {
		see(c)
	}
	for _, p := range x.Pairs {
		see(p.A)
		see(p.B)
	}
	for _, step := range []float64{1000, 2000, 5000, 10000, 20000, 50000, 100000, 200000, 500000, 1e6} {
		if top <= step {
			return step
		}
	}
	return math.Ceil(top/1e6) * 1e6
}

func gateViews(gates map[string]string, prospects int) []gateView {
	names := map[string]string{
		"F1": "No employment conflict", "F2": "Not a regulated domain you avoid", "F3": "Fits your hours and budget", "F4": "Within your skills",
		"M1": "An identifiable buyer", "M2": "Buyers already spend on this", "M3": "50+ reachable organisations", "M4": "No strong incumbent at this price",
		"M5": "Serving a customer costs under 40% of price", "M6": "No missing or prohibited dependency",
	}
	var out []gateView
	for _, code := range []string{"F1", "F2", "F3", "F4", "M1", "M2", "M3", "M4", "M5", "M6"} {
		s := gates[code]
		if s == "SKIPPED" {
			continue
		}
		if s == "" {
			s = "UNKNOWN"
		}
		g := gateView{Code: code, Status: s, Mark: mark(s), Title: names[code] + ": " + strings.ToLower(s)}
		if code == "M3" && prospects > 0 {
			g.Note = fmt.Sprintf("(%d prospects)", prospects)
		}
		out = append(out, g)
	}
	return out
}

var (
	mdBold   = regexp.MustCompile(`\*\*(.+?)\*\*`)
	mdCode   = regexp.MustCompile("`([^`]+)`")
	mdItalic = regexp.MustCompile(`(^|[^*])\*([^*]+)\*`)
	mdOL     = regexp.MustCompile(`^\d+\.\s+`)
)

// mdHTML renders the digest's own section markdown (paragraphs, bullet and
// numbered lists, bold, italics and code spans) as HTML. Text is escaped
// first, so model or source text can't inject markup.
func mdHTML(md string) template.HTML {
	var b strings.Builder
	list := ""
	closeList := func() {
		if list != "" {
			b.WriteString("</" + list + ">")
			list = ""
		}
	}
	open := func(tag string) {
		if list != tag {
			closeList()
			b.WriteString("<" + tag + ">")
			list = tag
		}
	}
	for _, line := range strings.Split(md, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
			closeList()
		case strings.HasPrefix(line, "- "):
			open("ul")
			b.WriteString("<li>" + inline(strings.TrimPrefix(line, "- ")) + "</li>")
		case mdOL.MatchString(line):
			open("ol")
			b.WriteString("<li>" + inline(mdOL.ReplaceAllString(line, "")) + "</li>")
		default:
			closeList()
			b.WriteString("<p>" + inline(line) + "</p>")
		}
	}
	closeList()
	return template.HTML(b.String())
}

func inline(s string) string {
	s = html.EscapeString(s)
	s = mdCode.ReplaceAllString(s, "<code>$1</code>")
	s = mdBold.ReplaceAllString(s, "<b>$1</b>")
	return mdItalic.ReplaceAllString(s, "$1<i>$2</i>")
}
