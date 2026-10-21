package embed_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kwant-dbg/auto/internal/embed"
	"github.com/kwant-dbg/auto/internal/store"
	"github.com/kwant-dbg/auto/internal/testdb"
)

func fakeServer(t *testing.T, dims int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		var inputs []any
		switch r.URL.Path {
		case "/api/embed": // ollama
			inputs = body["input"].([]any)
		case "/embed": // tei
			inputs = body["inputs"].([]any)
		default:
			http.NotFound(w, r)
			return
		}
		vecs := make([][]float32, len(inputs))
		for i := range vecs {
			vecs[i] = make([]float32, dims)
			vecs[i][0] = float32(i + 1)
		}
		if r.URL.Path == "/api/embed" {
			json.NewEncoder(w).Encode(map[string]any{"embeddings": vecs}) //nolint:errcheck
		} else {
			json.NewEncoder(w).Encode(vecs) //nolint:errcheck
		}
	}))
}

func TestClients(t *testing.T) {
	srv := fakeServer(t, 4)
	defer srv.Close()
	for _, provider := range []string{"ollama", "tei"} {
		c, err := embed.New(embed.Config{Provider: provider, URL: srv.URL, Model: "m", Dimensions: 4})
		if err != nil {
			t.Fatal(err)
		}
		v, err := c.Embed(context.Background(), []string{"a", "b"})
		if err != nil || len(v) != 2 || v[1][0] != 2 {
			t.Fatalf("%s: %v %v", provider, v, err)
		}
	}
	c, _ := embed.New(embed.Config{Provider: "ollama", URL: srv.URL, Model: "m", Dimensions: 8})
	if _, err := c.Embed(context.Background(), []string{"a"}); err == nil {
		t.Fatal("dimension mismatch must fail")
	}
	if _, err := embed.New(embed.Config{Provider: "nope", URL: "x", Dimensions: 1}); err == nil {
		t.Fatal("unknown provider must fail")
	}
}

func TestStatementAndLiteral(t *testing.T) {
	if s := embed.Statement(" Jira admins ", "export reports", "exports break", ""); s != "Jira admins need to export reports but exports break" {
		t.Errorf("Statement = %q", s)
	}
	if l := embed.VectorLiteral([]float32{1, 0.5, -2}); l != "[1,0.5,-2]" {
		t.Errorf("VectorLiteral = %q", l)
	}
}

func TestSweeperAndSchedule(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	srv := fakeServer(t, 3)
	defer srv.Close()

	st.SyncTOS(ctx, []store.TOSEntry{{SourceType: "t", AccessMethod: "a", TermsURL: "u", ReviewedAt: "2026-10-21", AllowedUse: "x", RateLimitPerMin: 1}}) //nolint:errcheck
	st.SyncSources(ctx, []store.SourceSpec{{Name: "s", Type: "t"}})                                                                                       //nolint:errcheck
	src, _ := st.GetSource(ctx, "s")
	doc, _ := st.SaveDocument(ctx, src.ID, store.Document{CanonicalURL: "u", MIME: "text/plain", Content: []byte("hello world quote")})
	for i := 0; i < 3; i++ {
		if _, err := st.DB.Exec(ctx, `INSERT INTO observation (raw_document_id, span_start, span_end, quote, category, evidence_tier,
			actor_segment, job, origin_key, model, prompt_version, input_hash) VALUES ($1, 0, 5, 'hello', 'complaint', 4, 'a', 'b', 'o', 'm', 'p', 'h')`, doc.ID); err != nil {
			t.Fatal(err)
		}
	}

	created, err := embed.Schedule(ctx, st)
	if err != nil || !created {
		t.Fatalf("schedule: %v %v", created, err)
	}
	if again, _ := embed.Schedule(ctx, st); again {
		t.Fatal("a second sweeper must not be queued while one is pending")
	}

	c, _ := embed.New(embed.Config{Provider: "ollama", URL: srv.URL, Model: "nomic", Dimensions: 3, BatchSize: 2})
	sw := &embed.Sweeper{Store: st, Client: c}
	out, err := sw.Handle(ctx, nil)
	if err != nil || out.(map[string]int)["embedded"] != 2 {
		t.Fatalf("first sweep: %v %v", out, err)
	}
	sw.Handle(ctx, nil) //nolint:errcheck
	var missing int
	var model string
	st.DB.QueryRow(ctx, `SELECT count(*) FILTER (WHERE embedding IS NULL), max(embedding_model) FROM observation`).Scan(&missing, &model) //nolint:errcheck
	if missing != 0 || model != "ollama:nomic" {
		t.Fatalf("missing=%d model=%s", missing, model)
	}
}
