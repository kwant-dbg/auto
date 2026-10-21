// Package textutil converts fetched markup to the plain text AOE stores and
// quotes from. Spans are checked against this text, so conversion must be
// deterministic.
package textutil

import (
	"bytes"
	"io"
	"regexp"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

var (
	skip = map[atom.Atom]bool{
		atom.Script: true, atom.Style: true, atom.Noscript: true, atom.Svg: true, atom.Template: true,
		atom.Iframe: true, atom.Object: true, atom.Head: true,
	}
	block = map[atom.Atom]bool{
		atom.P: true, atom.Div: true, atom.Br: true, atom.Li: true, atom.Ul: true, atom.Ol: true,
		atom.H1: true, atom.H2: true, atom.H3: true, atom.H4: true, atom.H5: true, atom.H6: true,
		atom.Tr: true, atom.Table: true, atom.Section: true, atom.Article: true, atom.Header: true,
		atom.Footer: true, atom.Blockquote: true, atom.Pre: true, atom.Hr: true, atom.Dd: true, atom.Dt: true,
		atom.Main: true, atom.Aside: true, atom.Nav: true, atom.Form: true, atom.Td: true, atom.Th: true,
	}
	spaces    = regexp.MustCompile(`[ \t\f\v\r\x{00a0}]+`)
	blankRuns = regexp.MustCompile(`\n{3,}`)
)

// HTMLToText returns the page title and its visible text, one block per line.
func HTMLToText(r io.Reader) (title, text string, err error) {
	z := html.NewTokenizer(r)
	var b strings.Builder
	depth := 0 // inside skipped elements
	inTitle := false
	for {
		switch z.Next() {
		case html.ErrorToken:
			if z.Err() == io.EOF {
				return strings.TrimSpace(title), Normalize(b.String()), nil
			}
			return "", "", z.Err()
		case html.StartTagToken, html.SelfClosingTagToken:
			name, _ := z.TagName()
			a := atom.Lookup(name)
			if a == atom.Title {
				inTitle = true
			}
			if skip[a] && a != atom.Head {
				depth++
			}
			if block[a] {
				b.WriteByte('\n')
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			a := atom.Lookup(name)
			if a == atom.Title {
				inTitle = false
			}
			if skip[a] && a != atom.Head && depth > 0 {
				depth--
			}
			if block[a] {
				b.WriteByte('\n')
			}
		case html.TextToken:
			t := string(z.Text())
			if inTitle {
				title += t
				continue
			}
			if depth == 0 {
				b.WriteString(t)
			}
		}
	}
}

// Normalize collapses horizontal whitespace, trims lines and squeezes blank lines.
func Normalize(s string) string {
	lines := strings.Split(spaces.ReplaceAllString(s, " "), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	out := blankRuns.ReplaceAllString(strings.Join(lines, "\n"), "\n\n")
	return strings.TrimSpace(out)
}

// HTMLFragmentToText converts a snippet (e.g. an RSS description) to text.
func HTMLFragmentToText(s string) string {
	_, text, err := HTMLToText(bytes.NewBufferString("<body>" + s + "</body>"))
	if err != nil {
		return Normalize(s)
	}
	return text
}
