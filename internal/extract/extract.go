// Package extract turns raw documents into verified observations
// (docs/02 §5). The model returns quotes and a normalised job statement; code
// locates each quote in the stored snapshot, assigns the evidence tier by
// rule, and rejects anything it can't find verbatim.
package extract

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/kwant-dbg/auto/internal/claude"
	"github.com/kwant-dbg/auto/internal/evidence"
	"github.com/kwant-dbg/auto/internal/llm"
	"github.com/kwant-dbg/auto/internal/queue"
	"github.com/kwant-dbg/auto/internal/store"
	"github.com/kwant-dbg/auto/internal/worker"
	"github.com/kwant-dbg/auto/prompts"
)

// TaskType is the queue task type.
const TaskType = "extract"

// PromptVersion is the extractor prompt in use.
const PromptVersion = "v1"

// MaxDocChars caps how much of one document goes into a prompt. Longer
// documents are truncated; quotes can only come from the part shown.
const MaxDocChars = 40_000

// Inputs are the task inputs.
type Inputs struct {
	DocumentIDs   []int64 `json:"document_ids"`
	PromptVersion string  `json:"prompt_version"`
}

// Outputs are the task outputs.
type Outputs struct {
	Documents    int `json:"documents"`
	Observations int `json:"observations"`
	Rejected     int `json:"rejected"`
	Duplicates   int `json:"duplicates"`
}

// Doc is a document prepared for extraction.
type Doc struct {
	ID               int64
	SourceType       string
	CanonicalURL     string
	AuthorHash       string
	OrgHint          string
	InjectionSuspect bool
	Content          []byte
}

// LoadDocs reads documents and their snapshots.
func LoadDocs(ctx context.Context, st *store.Store, ids []int64) ([]Doc, error) {
	rows, err := st.DB.Query(ctx, `
		SELECT r.id, s.type, r.canonical_url, coalesce(r.author_hash, ''), coalesce(r.org_hint, ''),
		       r.injection_suspect, r.blob_path
		FROM raw_document r JOIN source s ON s.id = r.source_id
		WHERE r.id = ANY($1) ORDER BY r.id`, ids)
	if err != nil {
		return nil, err
	}
	type row struct {
		Doc
		blob string
	}
	rs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.ID, &x.SourceType, &x.CanonicalURL, &x.AuthorHash, &x.OrgHint, &x.InjectionSuspect, &x.blob)
		return x, err
	})
	if err != nil {
		return nil, err
	}
	docs := make([]Doc, 0, len(rs))
	for _, r := range rs {
		content, err := st.Blobs.Get(r.blob)
		if err != nil {
			return nil, fmt.Errorf("document %d: %w", r.ID, err)
		}
		r.Doc.Content = content
		docs = append(docs, r.Doc)
	}
	return docs, nil
}

// visible returns the part of a document shown to the model.
func visible(content []byte) []byte {
	if len(content) <= MaxDocChars {
		return content
	}
	cut := MaxDocChars
	for cut > 0 && !utf8.RuneStart(content[cut]) {
		cut--
	}
	return content[:cut]
}

// BuildPrompt renders the user message for a batch.
func BuildPrompt(docs []Doc) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Extract observations from the %d document(s) below. Quote exactly. Return an empty list if nothing qualifies.\n\n", len(docs))
	for _, d := range docs {
		v := visible(d.Content)
		fmt.Fprintf(&b, "<document id=\"%d\" kind=\"%s\" url=\"%s\"", d.ID, evidence.DocKind(d.SourceType, d.Content), d.CanonicalURL)
		if len(v) < len(d.Content) {
			b.WriteString(` truncated="true"`)
		}
		b.WriteString(">\n")
		b.Write(v)
		b.WriteString("\n</document>\n\n")
	}
	return b.String()
}

// modelObservation is one item of the model's structured output.
type modelObservation struct {
	DocumentID   int64           `json:"document_id"`
	Quote        string          `json:"quote"`
	Category     string          `json:"category"`
	ActorSegment string          `json:"actor_segment"`
	Job          string          `json:"job"`
	Obstacle     string          `json:"obstacle"`
	Consequence  string          `json:"consequence"`
	Specificity  int             `json:"specificity"`
	SpendHints   json.RawMessage `json:"spend_hints"`
}

var validCategory = map[string]bool{"spend": true, "offer": true, "complaint": true, "job_task": true, "change": true, "other": true}

// Extractor runs extraction tasks.
type Extractor struct {
	Store *store.Store
	LLM   *llm.Client
	Model string
}

// Handle is the worker handler for extract tasks.
func (e *Extractor) Handle(ctx context.Context, t *queue.Task) (any, error) {
	var in Inputs
	if err := json.Unmarshal(t.Inputs, &in); err != nil {
		return nil, queue.Permanent(fmt.Errorf("inputs: %w", err))
	}
	if in.PromptVersion == "" {
		in.PromptVersion = PromptVersion
	}
	out, err := e.Run(ctx, &t.ID, in)
	var deferred *worker.DeferError
	if err != nil && !errors.As(err, &deferred) && ctx.Err() == nil {
		markFailed(context.WithoutCancel(ctx), e.Store, in, t, err)
	}
	return out, err
}

// markFailed records a failed attempt on the extraction rows when the task has
// no retries left, so the scheduler can decide whether to try again.
func markFailed(ctx context.Context, st *store.Store, in Inputs, t *queue.Task, cause error) {
	if t.Attempts < t.MaxAttempts && !queue.IsPermanent(cause) {
		return
	}
	//nolint:errcheck // best effort: the scheduler also reaps rows whose task died
	st.DB.Exec(ctx, `UPDATE extraction SET status = 'failed', updated_at = now()
		WHERE raw_document_id = ANY($1) AND prompt_version = $2 AND status = 'queued'`, in.DocumentIDs, in.PromptVersion)
}

// Run extracts one batch and stores the verified observations.
func (e *Extractor) Run(ctx context.Context, taskID *int64, in Inputs) (Outputs, error) {
	if in.PromptVersion == "" {
		in.PromptVersion = PromptVersion
	}
	p, err := prompts.Load("extract", in.PromptVersion)
	if err != nil {
		return Outputs{}, queue.Permanent(err)
	}
	docs, err := LoadDocs(ctx, e.Store, in.DocumentIDs)
	if err != nil {
		return Outputs{}, err
	}
	out := Outputs{Documents: len(docs)}
	if len(docs) == 0 {
		return out, nil
	}

	userPrompt := BuildPrompt(docs)
	res, err := e.LLM.Run(ctx, taskID, claude.Spec{
		TaskType: TaskType, PromptVersion: p.ID(), Model: e.Model,
		SystemPrompt: p.System, Prompt: userPrompt, Schema: p.Schema,
		MaxTurns: 3, Timeout: 8 * time.Minute,
	})
	if err != nil {
		return out, err
	}
	var parsed struct {
		Observations []modelObservation `json:"observations"`
	}
	if err := json.Unmarshal(res.Structured, &parsed); err != nil {
		return out, fmt.Errorf("%w: %v", claude.ErrInvalidOutput, err)
	}

	byID := make(map[int64]*Doc, len(docs))
	for i := range docs {
		byID[docs[i].ID] = &docs[i]
	}
	inputHash := store.Hash([]byte(p.ID() + "\x00" + p.System + "\x00" + userPrompt))[:32]
	model := e.Model
	perDoc := map[int64][2]int{} // observations, rejected

	err = pgx.BeginFunc(ctx, e.Store.DB, func(tx pgx.Tx) error {
		seen := map[string]bool{}
		for _, o := range parsed.Observations {
			d := byID[o.DocumentID]
			reject := func(reason string) error {
				out.Rejected++
				if d == nil {
					return nil // unknown document id: nothing to attach the rejection to
				}
				c := perDoc[d.ID]
				c[1]++
				perDoc[d.ID] = c
				_, err := tx.Exec(ctx, `INSERT INTO rejected_extraction (raw_document_id, quote, reason, prompt_version)
					VALUES ($1, $2, $3, $4)`, d.ID, o.Quote, reason, p.ID())
				return err
			}
			if d == nil {
				if err := reject("unknown document id " + strconv.FormatInt(o.DocumentID, 10)); err != nil {
					return err
				}
				continue
			}
			if !validCategory[o.Category] {
				if err := reject("invalid category " + o.Category); err != nil {
					return err
				}
				continue
			}
			start, end, ok := evidence.Locate(visible(d.Content), o.Quote)
			if !ok {
				if err := reject("quote not found verbatim in snapshot"); err != nil {
					return err
				}
				continue
			}
			key := fmt.Sprintf("%d:%d:%d", d.ID, start, end)
			if seen[key] {
				out.Duplicates++
				continue
			}
			seen[key] = true

			hints := o.SpendHints
			if len(hints) == 0 || string(hints) == "null" {
				hints = json.RawMessage(`{}`)
			}
			kind := evidence.DocKind(d.SourceType, d.Content)
			_, err := tx.Exec(ctx, `
				INSERT INTO observation (raw_document_id, span_start, span_end, quote, category, evidence_tier,
					actor_segment, job, obstacle, consequence, specificity, spend_hints, origin_key, verified,
					injection_suspect, model, prompt_version, input_hash)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, true, $14, $15, $16, $17)`,
				d.ID, start, end, string(d.Content[start:end]), o.Category, evidence.Tier(kind, o.Category),
				strings.TrimSpace(o.ActorSegment), strings.TrimSpace(o.Job), strings.TrimSpace(o.Obstacle),
				strings.TrimSpace(o.Consequence), clamp(o.Specificity, 0, 5), hints,
				evidence.OriginKey(d.SourceType, d.AuthorHash, d.OrgHint, d.CanonicalURL),
				d.InjectionSuspect, model, p.ID(), inputHash)
			if err != nil {
				return err
			}
			out.Observations++
			c := perDoc[d.ID]
			c[0]++
			perDoc[d.ID] = c
		}
		for _, d := range docs {
			c := perDoc[d.ID]
			if _, err := tx.Exec(ctx, `
				INSERT INTO extraction (raw_document_id, prompt_version, status, task_id, observation_count, rejected_count)
				VALUES ($1, $2, 'done', $3, $4, $5)
				ON CONFLICT (raw_document_id, prompt_version) DO UPDATE SET status = 'done',
					observation_count = EXCLUDED.observation_count, rejected_count = EXCLUDED.rejected_count,
					updated_at = now()`, d.ID, in.PromptVersion, taskID, c[0], c[1]); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

func clamp(v, lo, hi int) int { return max(lo, min(hi, v)) }
