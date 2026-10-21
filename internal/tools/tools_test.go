package tools

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kwant-dbg/auto/internal/store"
	"github.com/kwant-dbg/auto/internal/testdb"
)

func setupServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st := testdb.New(t)
	ctx := context.Background()
	var tos []store.TOSEntry
	var specs []store.SourceSpec
	for name, typ := range map[string]string{SourceWeb: "web_fetch", SourceHN: "hn_search", SourceGitHub: "github_search"} {
		tos = append(tos, store.TOSEntry{SourceType: typ, AccessMethod: "a", TermsURL: "u", ReviewedAt: "2026-10-21", AllowedUse: "x", RateLimitPerMin: 6000})
		specs = append(specs, store.SourceSpec{Name: name, Type: typ})
	}
	if err := st.SyncTOS(ctx, tos); err != nil {
		t.Fatal(err)
	}
	if err := st.SyncSources(ctx, specs); err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: st, AllowInternal: true, HTTP: http.DefaultClient}
	s.defaults()
	s.throttle.gap = 0
	return s, st
}

func site(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("User-agent: *\nDisallow: /private\nAllow: /private/ok\n")) //nolint:errcheck
	})
	mux.HandleFunc("/pricing", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<html><head><title>Pricing</title></head><body><p>Starter is $49/month.</p><p>` + //nolint:errcheck
			strings.Repeat("Lots of words. ", 100) + `</p></body></html>`))
	})
	mux.HandleFunc("/private/x", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("secret")) }) //nolint:errcheck
	mux.HandleFunc("/file.pdf", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Write([]byte("%PDF")) //nolint:errcheck
	})
	return httptest.NewServer(mux)
}

func TestFetchSnapshotsExactlyWhatTheAgentSees(t *testing.T) {
	s, st := setupServer(t)
	srv := site(t)
	defer srv.Close()
	s.MaxTextChars = 200
	ctx := context.Background()

	out, err := s.Fetch(ctx, FetchInput{URL: srv.URL + "/pricing#plans"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Title != "Pricing" || out.URL != srv.URL+"/pricing" || out.NextOffset <= 0 || !strings.Contains(out.Text, "Starter is $49/month.") {
		t.Fatalf("out = %+v", out)
	}
	var blob string
	st.DB.QueryRow(ctx, `SELECT blob_path FROM raw_document WHERE id = $1`, out.DocumentID).Scan(&blob) //nolint:errcheck
	content, _ := st.Blobs.Get(blob)
	if !strings.HasPrefix(string(content), out.Text) {
		t.Fatal("the returned text must be a prefix of the stored snapshot")
	}
	// Paging continues where the last page stopped and reaches the end.
	rest, err := s.Fetch(ctx, FetchInput{URL: srv.URL + "/pricing", Offset: out.NextOffset})
	if err != nil || rest.DocumentID != out.DocumentID {
		t.Fatalf("second page: %+v %v (same content must reuse the snapshot)", rest, err)
	}

	if _, err := s.Fetch(ctx, FetchInput{URL: srv.URL + "/private/x"}); err == nil || !strings.Contains(err.Error(), "robots.txt") {
		t.Fatalf("robots.txt not honoured: %v", err)
	}
	if _, err := s.Fetch(ctx, FetchInput{URL: srv.URL + "/file.pdf"}); err == nil || !strings.Contains(err.Error(), "unsupported content type") {
		t.Fatalf("pdf: %v", err)
	}
	if _, err := s.Fetch(ctx, FetchInput{URL: "file:///etc/passwd"}); err == nil {
		t.Fatal("non-http scheme must be refused")
	}
}

func TestInternalAddressesAreBlocked(t *testing.T) {
	for _, u := range []string{"http://localhost:8080/", "http://127.0.0.1/", "http://169.254.169.254/latest/meta-data", "http://10.0.0.5/", "http://[::1]/", "http://metadata.internal/"} {
		if _, err := checkURL(u, false); !errors.Is(err, ErrBlockedAddress) {
			t.Errorf("checkURL(%s) = %v, want blocked", u, err)
		}
	}
	if _, err := checkURL("https://example.com/a#frag", false); err != nil {
		t.Errorf("public URL refused: %v", err)
	}
	// The dialer refuses internal IPs even when a hostname hides them.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("http_proxy", "")
	t.Setenv("https_proxy", "")
	c := SafeClient(5e9, false)
	if _, err := c.Get("http://127.0.0.1:" + port + "/"); err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Errorf("dialer allowed loopback: %v", err)
	}
}

func TestParseRobots(t *testing.T) {
	txt := "User-agent: aoe-research\nDisallow: /\n\nUser-agent: *\nDisallow: /tmp\n"
	if r := parseRobots(strings.NewReader(txt), "aoe-research/0.1 (x)"); len(r) != 1 || r[0].path != "/" {
		t.Errorf("specific group not chosen: %+v", r)
	}
	if r := parseRobots(strings.NewReader(txt), "other/1"); len(r) != 1 || r[0].path != "/tmp" {
		t.Errorf("wildcard group not chosen: %+v", r)
	}
}

func TestSearchesSnapshotResults(t *testing.T) {
	s, st := setupServer(t)
	ctx := context.Background()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/search":
			if r.URL.Query().Get("tags") != "comment" {
				t.Errorf("tags = %s", r.URL.Query().Get("tags"))
			}
			json.NewEncoder(w).Encode(map[string]any{"hits": []any{map[string]any{ //nolint:errcheck
				"objectID": "42", "story_title": "Ask HN: invoicing", "comment_text": "<p>We reconcile &amp; re-key every month.</p>",
				"author": "pg", "created_at": "2026-10-01T00:00:00Z"}}})
		case "/search/issues":
			if !strings.Contains(r.URL.Query().Get("q"), "is:issue") || r.Header.Get("Authorization") != "Bearer tok" {
				t.Errorf("github request: %s auth=%q", r.URL.RawQuery, r.Header.Get("Authorization"))
			}
			json.NewEncoder(w).Encode(map[string]any{"items": []any{map[string]any{ //nolint:errcheck
				"html_url": "https://github.com/o/r/issues/1", "title": "CSV export drops rows", "body": "Every export loses the last row.",
				"state": "open", "comments": 3, "repository_url": api(r) + "/repos/o/r", "user": map[string]any{"login": "u"},
				"reactions": map[string]any{"total_count": 17}}}})
		}
	}))
	defer api.Close()
	s.HNBaseURL, s.GitHubBaseURL, s.GitHubToken = api.URL, api.URL, "tok"

	hn, err := s.SearchHN(ctx, HNInput{Query: "invoicing", Kind: "comment"})
	if err != nil || len(hn.Hits) != 1 || hn.Hits[0].Text != "We reconcile & re-key every month." || hn.Hits[0].URL != "https://news.ycombinator.com/item?id=42" {
		t.Fatalf("hn = %+v %v", hn, err)
	}
	gh, err := s.SearchGitHub(ctx, GitHubInput{Query: "csv export"})
	if err != nil || len(gh.Hits) != 1 || gh.Hits[0].Score != 17 {
		t.Fatalf("github = %+v %v", gh, err)
	}
	var docs int
	var org string
	st.DB.QueryRow(ctx, `SELECT count(*), max(org_hint) FROM raw_document`).Scan(&docs, &org) //nolint:errcheck
	if docs != 2 || org != "o/r" {
		t.Fatalf("snapshots = %d org=%q", docs, org)
	}
}

func api(r *http.Request) string { return "http://" + r.Host }

func TestMCPRoundTripAndKBSearch(t *testing.T) {
	s, st := setupServer(t)
	ctx := context.Background()
	web, _ := st.GetSource(ctx, SourceWeb)
	doc, _ := st.SaveDocument(ctx, web.ID, store.Document{CanonicalURL: "https://e.com", MIME: "text/plain", Content: []byte("Month-end reconciliation takes two days.")})
	if _, err := st.DB.Exec(ctx, `INSERT INTO observation (raw_document_id, span_start, span_end, quote, category, evidence_tier,
		actor_segment, job, obstacle, origin_key, model, prompt_version, input_hash)
		VALUES ($1, 0, 39, 'Month-end reconciliation takes two days', 'complaint', 4, 'bookkeepers at agencies',
		'reconcile client accounts', 'manual matching', 'host:e.com', 'm', 'p', 'h')`, doc.ID); err != nil {
		t.Fatal(err)
	}

	serverT, clientT := mcp.NewInMemoryTransports()
	if _, err := s.MCP().Connect(ctx, serverT, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tl := range tools.Tools {
		names[tl.Name] = true
	}
	for _, want := range []string{"fetch_and_snapshot", "kb_search", "search_hn", "search_github_issues"} {
		if !names[want] {
			t.Errorf("tool %s not exposed (have %v)", want, names)
		}
	}

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "kb_search", Arguments: map[string]any{"query": "reconciliation"}})
	if err != nil || res.IsError {
		t.Fatalf("kb_search: %+v %v", res, err)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	var out KBOutput
	json.Unmarshal(raw, &out) //nolint:errcheck
	if len(out.Hits) != 1 || out.Hits[0].DocumentID != doc.ID || !strings.Contains(out.Hits[0].Statement, "bookkeepers at agencies need to reconcile") {
		t.Fatalf("kb hits = %+v", out)
	}

	// A tool error comes back as an error result, not a protocol failure.
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "fetch_and_snapshot", Arguments: map[string]any{"url": "ftp://x"}})
	if err != nil || !res.IsError {
		t.Fatalf("bad fetch: %+v %v", res, err)
	}
}

func TestToolsRefuseSourcesWithoutTOS(t *testing.T) {
	st := testdb.New(t)
	s := NewServer(st, "ua")
	if _, err := s.Fetch(context.Background(), FetchInput{URL: "https://example.com"}); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("err = %v", err)
	}
}

func TestWindow(t *testing.T) {
	b := []byte("héllo wörld")
	page, next := window(b, 0, 2) // would split é
	if page != "h" || next != 1 {
		t.Fatalf("page=%q next=%d", page, next)
	}
	page, next = window(b, next, 100)
	if page != "éllo wörld" || next != -1 {
		t.Fatalf("page=%q next=%d", page, next)
	}
}
