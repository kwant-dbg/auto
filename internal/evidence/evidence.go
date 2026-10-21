// Package evidence holds the deterministic evidence rules: locating quotes in
// snapshots (the span check), assigning evidence tiers by rule (EXT-4), and
// deriving origin keys for independence counting (PRB-4).
package evidence

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Evidence tiers (PRD §5). T7 (model inference) is never assigned to an
// observation: observations are quotes, by construction.
const (
	T0Commitment        = 0
	T1ObservedSpend     = 1
	T2DirectToUs        = 2
	T3PublishedOffer    = 3
	T4SpecificComplaint = 4
	T5Engagement        = 5
	T6Commentary        = 6
)

// Document kinds, derived from source type and content.
const (
	KindProcurement = "procurement"
	KindListing     = "marketplace_listing"
	KindReview      = "marketplace_review"
	KindCommunity   = "community"
	KindWeb         = "web"
	KindFounder     = "founder"
)

// DocKind classifies a document for tier rules.
func DocKind(sourceType string, content []byte) string {
	switch sourceType {
	case "usaspending_awards":
		return KindProcurement
	case "app_marketplace":
		var head struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal(content, &head) == nil && head.Kind == "review" {
			return KindReview
		}
		return KindListing
	case "hn_search", "github_search":
		return KindCommunity
	case "founder_interview":
		return KindFounder
	default:
		return KindWeb
	}
}

// Tier assigns an evidence tier from the document kind and the extracted
// category. The model chooses the category; it never chooses the tier.
func Tier(kind, category string) int {
	switch kind {
	case KindFounder:
		return T2DirectToUs
	case KindProcurement:
		if category == "spend" {
			return T1ObservedSpend // a public record of an actual payment
		}
	case KindListing:
		switch category {
		case "spend":
			return T1ObservedSpend // installs of a paid app
		case "offer":
			return T3PublishedOffer
		}
	}
	switch category {
	case "offer":
		return T3PublishedOffer
	case "complaint", "job_task", "spend":
		// A third party's statement about spend or pain: specific, but not a
		// record of payment and not said to us.
		return T4SpecificComplaint
	default:
		return T6Commentary
	}
}

// OriginKey identifies the independent origin of a document. Two documents
// with the same key count once for independence: the same reviewer, the same
// buying organisation, or the same website.
func OriginKey(sourceType, authorHash, orgHint, canonicalURL string) string {
	switch {
	case authorHash != "":
		return sourceType + ":author:" + authorHash
	case orgHint != "":
		return sourceType + ":org:" + strings.ToLower(strings.Join(strings.Fields(orgHint), " "))
	}
	if u, err := url.Parse(canonicalURL); err == nil && u.Host != "" {
		return "host:" + strings.TrimPrefix(strings.ToLower(u.Host), "www.")
	}
	return sourceType + ":url:" + canonicalURL
}

// Locate finds quote in content and returns its byte span. It tries, in order:
// an exact match; a match that ignores differences in whitespace; and, for
// JSON documents, the quote as it appears JSON-escaped. ok is false when the
// quote isn't in the document, which means it must be rejected.
func Locate(content []byte, quote string) (start, end int, ok bool) {
	q := strings.TrimSpace(quote)
	if q == "" {
		return 0, 0, false
	}
	if i := bytes.Index(content, []byte(q)); i >= 0 {
		return i, i + len(q), true
	}
	if s, e, ok := locateFuzzySpace(content, q); ok {
		return s, e, true
	}
	if esc, err := json.Marshal(q); err == nil {
		inner := esc[1 : len(esc)-1]
		if i := bytes.Index(content, inner); i >= 0 {
			return i, i + len(inner), true
		}
		if s, e, ok := locateFuzzySpace(content, string(inner)); ok {
			return s, e, true
		}
	}
	return 0, 0, false
}

// locateFuzzySpace matches q against content treating any run of whitespace
// (including JSON's escaped \n) as equal to any other run.
func locateFuzzySpace(content []byte, q string) (int, int, bool) {
	tokens := strings.Fields(q)
	if len(tokens) == 0 {
		return 0, 0, false
	}
	first := []byte(tokens[0])
	for from := 0; from < len(content); {
		i := bytes.Index(content[from:], first)
		if i < 0 {
			return 0, 0, false
		}
		start := from + i
		pos := start + len(first)
		matched := true
		for _, tok := range tokens[1:] {
			next := skipSpace(content, pos)
			if next == pos || !bytes.HasPrefix(content[next:], []byte(tok)) {
				matched = false
				break
			}
			pos = next + len(tok)
		}
		if matched {
			return start, pos, true
		}
		from = start + 1
	}
	return 0, 0, false
}

// skipSpace advances past whitespace and JSON-escaped whitespace (\n, \t, \r).
func skipSpace(b []byte, i int) int {
	for i < len(b) {
		if b[i] == '\\' && i+1 < len(b) && (b[i+1] == 'n' || b[i+1] == 't' || b[i+1] == 'r') {
			i += 2
			continue
		}
		r, size := utf8.DecodeRune(b[i:])
		if !unicode.IsSpace(r) {
			return i
		}
		i += size
	}
	return i
}
