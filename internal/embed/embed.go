// Package embed computes embeddings with a local open-source model served
// over HTTP (Ollama or Hugging Face text-embeddings-inference), and the
// sweeper task that fills observation vectors (PRB-1).
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kwant-dbg/auto/internal/queue"
	"github.com/kwant-dbg/auto/internal/store"
)

// TaskType is the queue task type for the embedding sweeper.
const TaskType = "embed"

// Config selects the embedding server.
type Config struct {
	Provider   string        `yaml:"provider"` // ollama | tei; empty disables embeddings
	URL        string        `yaml:"url"`
	Model      string        `yaml:"model"`
	Dimensions int           `yaml:"dimensions"`
	BatchSize  int           `yaml:"batch_size"`
	Timeout    time.Duration `yaml:"timeout"`
}

// Enabled reports whether embeddings are configured.
func (c Config) Enabled() bool { return c.Provider != "" }

// ModelID is stored as embedding_model on every vector.
func (c Config) ModelID() string { return c.Provider + ":" + c.Model }

// Client calls the embedding server.
type Client struct {
	cfg  Config
	http *http.Client
}

// New validates the config.
func New(cfg Config) (*Client, error) {
	switch cfg.Provider {
	case "ollama", "tei":
	default:
		return nil, fmt.Errorf("embed: unknown provider %q (ollama or tei)", cfg.Provider)
	}
	if cfg.URL == "" || cfg.Dimensions <= 0 {
		return nil, fmt.Errorf("embed: url and dimensions are required")
	}
	if cfg.Provider == "ollama" && cfg.Model == "" {
		return nil, fmt.Errorf("embed: model is required for ollama")
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 64
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 2 * time.Minute
	}
	return &Client{cfg: cfg, http: &http.Client{Timeout: cfg.Timeout}}, nil
}

// Embed returns one vector per input, checking the dimension.
func (c *Client) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	var url string
	var body any
	switch c.cfg.Provider {
	case "ollama":
		url, body = strings.TrimRight(c.cfg.URL, "/")+"/api/embed", map[string]any{"model": c.cfg.Model, "input": inputs}
	case "tei":
		url, body = strings.TrimRight(c.cfg.URL, "/")+"/embed", map[string]any{"inputs": inputs, "truncate": true}
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embed: HTTP %d: %.300s", resp.StatusCode, data)
	}
	var vecs [][]float32
	switch c.cfg.Provider {
	case "ollama":
		var r struct {
			Embeddings [][]float32 `json:"embeddings"`
		}
		err = json.Unmarshal(data, &r)
		vecs = r.Embeddings
	case "tei":
		err = json.Unmarshal(data, &vecs)
	}
	if err != nil {
		return nil, fmt.Errorf("embed: decode: %w", err)
	}
	if len(vecs) != len(inputs) {
		return nil, fmt.Errorf("embed: got %d vectors for %d inputs", len(vecs), len(inputs))
	}
	for i, v := range vecs {
		if len(v) != c.cfg.Dimensions {
			return nil, fmt.Errorf("embed: vector %d has %d dimensions, config says %d", i, len(v), c.cfg.Dimensions)
		}
	}
	return vecs, nil
}

// Statement renders the normalised job statement that gets embedded
// (docs/02 §5: normalise before embedding).
func Statement(actor, job, obstacle, consequence string) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(actor))
	b.WriteString(" needs to ")
	b.WriteString(strings.TrimSpace(job))
	if o := strings.TrimSpace(obstacle); o != "" {
		b.WriteString(" but ")
		b.WriteString(o)
	}
	if c := strings.TrimSpace(consequence); c != "" {
		b.WriteString(", causing ")
		b.WriteString(c)
	}
	return b.String()
}

// VectorLiteral formats a vector for pgvector ("[1,2,3]").
func VectorLiteral(v []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(x), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

// Sweeper fills missing observation embeddings, a batch per task run.
type Sweeper struct {
	Store  *store.Store
	Client *Client
}

// Handle embeds up to BatchSize observations that have no vector. Rows are
// locked with SKIP LOCKED, so concurrent sweepers never embed the same row.
func (s *Sweeper) Handle(ctx context.Context, _ *queue.Task) (any, error) {
	n := 0
	err := pgx.BeginFunc(ctx, s.Store.DB, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, actor_segment, job, obstacle, consequence FROM observation
			WHERE embedding IS NULL ORDER BY id LIMIT $1 FOR UPDATE SKIP LOCKED`, s.Client.cfg.BatchSize)
		if err != nil {
			return err
		}
		type item struct {
			id   int64
			text string
		}
		items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (item, error) {
			var it item
			var a, j, o, c string
			err := r.Scan(&it.id, &a, &j, &o, &c)
			it.text = Statement(a, j, o, c)
			return it, err
		})
		if err != nil || len(items) == 0 {
			return err
		}
		texts := make([]string, len(items))
		for i, it := range items {
			texts[i] = it.text
		}
		vecs, err := s.Client.Embed(ctx, texts)
		if err != nil {
			return err
		}
		for i, it := range items {
			if _, err := tx.Exec(ctx, `UPDATE observation SET embedding = $2::vector, embedding_model = $3 WHERE id = $1`,
				it.id, VectorLiteral(vecs[i]), s.Client.cfg.ModelID()); err != nil {
				return err
			}
		}
		n = len(items)
		return nil
	})
	return map[string]int{"embedded": n}, err
}

// Schedule enqueues one sweeper task when there are unembedded observations
// and no sweeper is already pending.
func Schedule(ctx context.Context, st *store.Store) (bool, error) {
	var pending bool
	if err := st.DB.QueryRow(ctx, `SELECT
		EXISTS (SELECT 1 FROM observation WHERE embedding IS NULL)
		AND NOT EXISTS (SELECT 1 FROM task WHERE type = $1 AND state IN ('queued', 'running'))`, TaskType).Scan(&pending); err != nil {
		return false, err
	}
	if !pending {
		return false, nil
	}
	_, created, err := queue.Enqueue(ctx, st.DB, queue.NewTask{Type: TaskType, Priority: 1})
	return created, err
}
