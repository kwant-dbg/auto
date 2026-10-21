// Package verify checks claims made by agents (RES-3, docs/04 §4): every
// quote must be found verbatim in a stored snapshot (deterministic span
// check), and a separate model call judges whether the located quotes support
// the claim, with an overclaiming rubric. Claims with no locatable quote are
// unsupported without asking a model.
package verify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kwant-dbg/auto/internal/claude"
	"github.com/kwant-dbg/auto/internal/evidence"
	"github.com/kwant-dbg/auto/internal/llm"
	"github.com/kwant-dbg/auto/internal/store"
	"github.com/kwant-dbg/auto/prompts"
)

// PromptVersion is the entailment prompt in use.
const PromptVersion = "v1"

// Verdicts.
const (
	Supported   = "supported"
	Partial     = "partial"
	Unsupported = "unsupported"
)

// Citation is a quote an agent attributes to a stored document.
type Citation struct {
	DocumentID int64  `json:"document_id"`
	Quote      string `json:"quote"`
}

// Span is a citation located in its snapshot. Quote is the snapshot slice.
type Span struct {
	DocumentID int64  `json:"raw_document_id"`
	Start      int    `json:"start"`
	End        int    `json:"end"`
	Quote      string `json:"quote"`
	URL        string `json:"url"`
}

// Claim is one claim to verify.
type Claim struct {
	Key       string
	Text      string
	Citations []Citation
}

// Result is the verdict for one claim.
type Result struct {
	Spans   []Span
	Missing int // citations that couldn't be located verbatim
	Verdict string
	Note    string
	Checked bool // entailment was checked by a model
}

// Verifier checks claims.
type Verifier struct {
	Store *store.Store
	LLM   *llm.Client // nil: span check only, located claims are "partial"
	Model string
}

type doc struct {
	content []byte
	url     string
}

// LocateAll runs the span check for every claim.
func (v *Verifier) LocateAll(ctx context.Context, claims []Claim) (map[string]Result, error) {
	cache := map[int64]*doc{}
	load := func(id int64) (*doc, error) {
		if d, ok := cache[id]; ok {
			return d, nil
		}
		var blob, url string
		err := v.Store.DB.QueryRow(ctx, `SELECT blob_path, canonical_url FROM raw_document WHERE id = $1`, id).Scan(&blob, &url)
		if errors.Is(err, pgx.ErrNoRows) {
			cache[id] = nil
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		content, err := v.Store.Blobs.Get(blob)
		if err != nil {
			return nil, err
		}
		d := &doc{content: content, url: url}
		cache[id] = d
		return d, nil
	}

	out := map[string]Result{}
	for _, c := range claims {
		var r Result
		for _, cit := range c.Citations {
			d, err := load(cit.DocumentID)
			if err != nil {
				return nil, err
			}
			if d == nil {
				r.Missing++
				continue
			}
			s, e, ok := evidence.Locate(d.content, cit.Quote)
			if !ok {
				r.Missing++
				continue
			}
			r.Spans = append(r.Spans, Span{DocumentID: cit.DocumentID, Start: s, End: e, Quote: string(d.content[s:e]), URL: d.url})
		}
		if len(r.Spans) == 0 {
			r.Verdict, r.Note = Unsupported, "no cited quote was found verbatim in a stored snapshot"
		}
		out[c.Key] = r
	}
	return out, nil
}

// Verify runs the span check, then the entailment check on claims with at
// least one located quote.
func (v *Verifier) Verify(ctx context.Context, taskID *int64, taskType string, claims []Claim) (map[string]Result, error) {
	results, err := v.LocateAll(ctx, claims)
	if err != nil {
		return nil, err
	}
	var todo []Claim
	for _, c := range claims {
		if r := results[c.Key]; len(r.Spans) > 0 {
			todo = append(todo, c)
		}
	}
	if len(todo) == 0 {
		return results, nil
	}
	if v.LLM == nil {
		for _, c := range todo {
			r := results[c.Key]
			r.Verdict, r.Note = Partial, "quotes located; entailment not checked"
			results[c.Key] = r
		}
		return results, nil
	}
	p, err := prompts.Load("verify", PromptVersion)
	if err != nil {
		return nil, err
	}
	const batch = 20
	for i := 0; i < len(todo); i += batch {
		part := todo[i:min(i+batch, len(todo))]
		res, err := v.LLM.Run(ctx, taskID, claude.Spec{
			TaskType: taskType + ".verify", PromptVersion: p.ID(), Model: v.Model, SystemPrompt: p.System,
			Prompt: buildPrompt(part, results), Schema: p.Schema, MaxTurns: 3, Timeout: 5 * time.Minute,
		})
		if err != nil {
			return nil, err
		}
		var out struct {
			Verdicts []struct {
				Key     string `json:"key"`
				Verdict string `json:"verdict"`
				Note    string `json:"note"`
			} `json:"verdicts"`
		}
		if err := json.Unmarshal(res.Structured, &out); err != nil {
			return nil, fmt.Errorf("%w: %v", claude.ErrInvalidOutput, err)
		}
		for _, vd := range out.Verdicts {
			r, ok := results[vd.Key]
			if !ok || len(r.Spans) == 0 {
				continue
			}
			switch vd.Verdict {
			case Supported, Partial, Unsupported:
				r.Verdict = vd.Verdict
			default:
				r.Verdict = Unsupported
			}
			r.Note, r.Checked = vd.Note, true
			if r.Missing > 0 && r.Verdict == Supported {
				r.Note = strings.TrimSpace(r.Note + fmt.Sprintf(" (%d other citation(s) not found verbatim)", r.Missing))
			}
			results[vd.Key] = r
		}
		// A claim the model skipped stays unchecked: treat it as partial.
		for _, c := range part {
			if r := results[c.Key]; r.Verdict == "" {
				r.Verdict, r.Note = Partial, "entailment verdict missing"
				results[c.Key] = r
			}
		}
	}
	return results, nil
}

func buildPrompt(claims []Claim, located map[string]Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Judge each of the %d claims against its quotes only.\n\n", len(claims))
	for _, c := range claims {
		fmt.Fprintf(&b, "<claim key=%q>\n%s\n", c.Key, c.Text)
		for _, s := range located[c.Key].Spans {
			q := s.Quote
			if len(q) > 1500 {
				q = q[:1500] + "…"
			}
			fmt.Fprintf(&b, "  <quote source=%q>%s</quote>\n", s.URL, q)
		}
		b.WriteString("</claim>\n\n")
	}
	return b.String()
}
