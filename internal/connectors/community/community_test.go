package community_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kwant-dbg/auto/internal/connectors"
	_ "github.com/kwant-dbg/auto/internal/connectors/community"
	"github.com/kwant-dbg/auto/internal/store"
)

var long = strings.Repeat("We run 40 Kubernetes clusters and every upgrade takes a week of manual checks. ", 4)

func collect(t *testing.T, typ string, cfg any, cursor json.RawMessage) ([]store.Document, map[string]float64, json.RawMessage) {
	t.Helper()
	raw, _ := json.Marshal(cfg)
	c, err := connectors.New(typ, raw)
	if err != nil {
		t.Fatal(err)
	}
	var docs []store.Document
	metrics := map[string]float64{}
	env := connectors.Env{HTTP: http.DefaultClient, UserAgent: "aoe-test", Metric: func(subject, name string, v float64) error {
		metrics[subject+" "+name] = v
		return nil
	}}
	next, err := c.Fetch(context.Background(), env, cursor, func(d store.Document) error { docs = append(docs, d); return nil })
	if err != nil {
		t.Fatal(err)
	}
	return docs, metrics, next
}

func TestHNCollectsNewDiscussionOnce(t *testing.T) {
	var filters []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/api/v1/search_by_date" || q.Get("query") != `"kubernetes upgrade"` || q.Get("typoTolerance") != "false" || q.Get("advancedSyntax") != "true" {
			http.Error(w, "unexpected "+r.URL.String(), 400)
			return
		}
		filters = append(filters, r.URL.Query().Get("numericFilters"))
		long, _ := json.Marshal("<p>" + long + "</p>")
		fmt.Fprintf(w, `{"hits":[
			{"objectID":"101","comment_text":%s,"story_title":"Ask HN: upgrades","author":"Alice","created_at":"2026-10-20T10:00:00Z","created_at_i":1792500000},
			{"objectID":"102","comment_text":"+1","author":"bob","created_at_i":1792500100},
			{"objectID":"103","title":"Show HN: a link","url":"https://x.example","author":"carol","created_at_i":1792400000},
			{"objectID":"104","comment_text":%s,"story_title":"Ask HN: Who is hiring? (October 2026)","author":"erin","created_at_i":1792400001}]}`, long, long)
	}))
	defer srv.Close()
	cfg := map[string]any{"base_url": srv.URL, "queries": []string{"kubernetes upgrade"}}
	docs, _, cursor := collect(t, "hn_search", cfg, nil)
	if len(docs) != 1 {
		t.Fatalf("want only the substantive comment, got %d docs", len(docs))
	}
	d := docs[0]
	if d.CanonicalURL != "https://news.ycombinator.com/item?id=101" || d.AuthorHash != store.AuthorHash("alice") || d.PublishedAt == nil ||
		!strings.Contains(string(d.Content), "every upgrade takes a week") || strings.Contains(string(d.Content), "<p>") {
		t.Fatalf("doc = %+v %s", d, d.Content)
	}
	// The next run only asks for items newer than the newest seen.
	collect(t, "hn_search", cfg, cursor)
	if len(filters) != 2 || filters[1] != "created_at_i>1792500100" {
		t.Fatalf("filters = %v", filters)
	}
}

func TestGitHubCollectsUpvotedFeatureRequests(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "tok")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/search/issues" || r.Header.Get("Authorization") != "Bearer tok" || q.Get("sort") != "reactions-+1" ||
			!strings.Contains(q.Get("q"), "is:issue") || !strings.Contains(q.Get("q"), "label:enhancement helm") {
			http.Error(w, "unexpected "+r.URL.String(), 400)
			return
		}
		body, _ := json.Marshal(long)
		fmt.Fprintf(w, `{"items":[
			{"html_url":"https://github.com/acme/helm/issues/1","title":"Support upgrade dry runs","body":%s,"state":"open","comments":12,
			 "created_at":"2025-01-02T03:04:05Z","repository_url":"%s/repos/acme/helm","user":{"login":"dana"},"reactions":{"total_count":340}},
			{"html_url":"https://github.com/acme/helm/pull/2","title":"PR","body":%s,"repository_url":"%s/repos/acme/helm","pull_request":{"url":"x"}},
			{"html_url":"https://github.com/acme/helm/issues/3","title":"short","body":"please add","repository_url":"%s/repos/acme/helm"}]}`,
			body, srvURL(r), body, srvURL(r), srvURL(r))
	}))
	defer srv.Close()
	docs, metrics, _ := collect(t, "github_search", map[string]any{"base_url": srv.URL, "queries": []string{"is:open label:enhancement helm"}}, nil)
	if len(docs) != 1 || docs[0].OrgHint != "acme/helm" || docs[0].AuthorHash != store.AuthorHash("dana") ||
		!strings.Contains(string(docs[0].Content), `"reactions": 340`) {
		t.Fatalf("docs = %+v", docs)
	}
	if metrics["https://github.com/acme/helm/issues/1 reactions"] != 340 || metrics["https://github.com/acme/helm/issues/1 comments"] != 12 {
		t.Fatalf("metrics = %v", metrics)
	}
}

func srvURL(r *http.Request) string { return "http://" + r.Host }

func TestConfigNeedsQueries(t *testing.T) {
	for _, typ := range []string{"hn_search", "github_search"} {
		if _, err := connectors.New(typ, json.RawMessage(`{}`)); err == nil {
			t.Errorf("%s without queries must fail", typ)
		}
	}
}
