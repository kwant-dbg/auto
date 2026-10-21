package marketplace

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/kwant-dbg/auto/internal/connectors"
	"github.com/kwant-dbg/auto/internal/store"
)

type sink struct {
	docs    []store.Document
	metrics map[string]float64
}

func (s *sink) env(c *http.Client) connectors.Env {
	s.metrics = map[string]float64{}
	return connectors.Env{HTTP: c, Metric: func(subject, name string, v float64) error {
		s.metrics[subject+"/"+name] = v
		return nil
	}}
}

func (s *sink) emit(d store.Document) error { s.docs = append(s.docs, d); return nil }

func newConn(t *testing.T, cfg map[string]any) *Connector {
	t.Helper()
	raw, _ := json.Marshal(cfg)
	c, err := connectors.New(Type, raw)
	if err != nil {
		t.Fatal(err)
	}
	return c.(*Connector)
}

func atlassianServer(t *testing.T, total int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/2/addons", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cost") != "paid" || r.URL.Query().Get("hosting") != "cloud" {
			t.Errorf("unexpected filters: %s", r.URL.RawQuery)
		}
		off, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		lim, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		var addons []map[string]any
		for i := off; i < min(off+lim, total); i++ {
			key := fmt.Sprintf("com.example.app%d", i)
			addons = append(addons, map[string]any{
				"id": strconv.Itoa(100 + i), "key": key, "name": fmt.Sprintf("App %d", i), "tagLine": "Do the thing", "summary": "Summary & more",
				"_links": map[string]any{
					"alternate": map[string]any{"href": fmt.Sprintf("/apps/%d/app-%d", 100+i, i), "type": "text/html"},
					"reviews":   []any{map[string]any{"href": "/rest/2/addons/" + key + "/reviews{?sort,offset,limit}", "templated": true}},
				},
				"_embedded": map[string]any{
					"categories":   []any{map[string]any{"name": "Reporting"}},
					"distribution": map[string]any{"downloads": 900, "totalInstalls": 100 * (i + 1)},
					"reviews":      map[string]any{"averageStars": 3.5, "count": 12},
					"vendor":       map[string]any{"name": "Vendor Inc"},
				},
			})
		}
		json.NewEncoder(w).Encode(map[string]any{"count": total, "_embedded": map[string]any{"addons": addons}}) //nolint:errcheck
	})
	mux.HandleFunc("/rest/2/addons/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/reviews"):
			json.NewEncoder(w).Encode(map[string]any{"_embedded": map[string]any{"reviews": []any{ //nolint:errcheck
				map[string]any{"id": "r1", "review": "Exports break every month-end; we re-key the data into Excel.", "stars": 2, "date": "2026-10-01T00:00:00Z",
					"_embedded": map[string]any{"author": map[string]any{"name": "Pat Example"}}},
				map[string]any{"id": "r2", "review": "Love it", "stars": 5, "date": "2026-10-02T00:00:00Z"},
				map[string]any{"id": "r3", "stars": 1, "date": "2026-10-03T00:00:00Z"}, // rating without text
			}}})
		case strings.HasSuffix(r.URL.Path, "/pricing/cloud/live"):
			json.NewEncoder(w).Encode(map[string]any{"items": []any{ //nolint:errcheck
				map[string]any{"editionDescription": "10 users", "amount": 0, "monthsValid": 1, "unitCount": 10, "licenseType": "COMMERCIAL"},
				map[string]any{"editionDescription": "50 users", "amount": 120, "monthsValid": 1, "unitCount": 50, "licenseType": "COMMERCIAL"},
				map[string]any{"editionDescription": "50 users annual", "amount": 1200, "monthsValid": 12, "unitCount": 50, "licenseType": "COMMERCIAL"},
			}})
		default:
			http.NotFound(w, r)
		}
	})
	return httptest.NewServer(mux)
}

func TestAtlassianSweep(t *testing.T) {
	srv := atlassianServer(t, 5)
	defer srv.Close()
	c := newConn(t, map[string]any{"platform": "atlassian", "base_url": srv.URL, "page_size": 2, "max_apps_per_run": 4,
		"reviews_per_app": 10, "include_pricing": true, "min_installs": 150})
	var s sink
	cursor, err := c.Fetch(context.Background(), s.env(srv.Client()), nil, s.emit)
	if err != nil {
		t.Fatal(err)
	}
	if string(cursor) != `{"offset":4}` {
		t.Fatalf("cursor = %s", cursor)
	}
	// Apps 0–3 fetched; app 0 has 100 installs (< min 150) and is skipped.
	// Each kept app: 1 listing + 1 low-star review with text.
	if len(s.docs) != 6 {
		t.Fatalf("docs = %d, want 6", len(s.docs))
	}
	listing := s.docs[0]
	if listing.CanonicalURL != srv.URL+"/apps/101/app-1" || listing.OrgHint != "Vendor Inc" {
		t.Fatalf("listing = %+v", listing)
	}
	if strings.Contains(string(listing.Content), "installs") {
		t.Fatal("changing counters must be metrics, not document content")
	}
	if !strings.Contains(string(listing.Content), `"Summary & more"`) {
		t.Fatalf("listing text escaped or missing: %s", listing.Content)
	}
	rev := s.docs[1]
	if !strings.Contains(string(rev.Content), "re-key the data") || rev.AuthorHash == "" || strings.Contains(string(rev.Content), "Pat Example") {
		t.Fatalf("review = %+v %s", rev, rev.Content)
	}
	if v := s.metrics["atlassian:com.example.app1/installs"]; v != 200 {
		t.Fatalf("installs metric = %v", v)
	}
	if v := s.metrics["atlassian:com.example.app1/price_usd_month_min_paid"]; v != 120 {
		t.Fatalf("min paid price = %v (annual items must be ignored)", v)
	}

	// The next run resumes at offset 4 and wraps to 0 at the end of the catalogue.
	s.docs = nil
	cursor, err = c.Fetch(context.Background(), s.env(srv.Client()), cursor, s.emit)
	if err != nil || string(cursor) != `{"offset":0}` || len(s.docs) != 2 {
		t.Fatalf("second run: cursor=%s docs=%d err=%v", cursor, len(s.docs), err)
	}
}

const wpFeed = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:dc="http://purl.org/dc/elements/1.1/"><channel>
<item><guid>https://wordpress.org/support/topic/broken-totals/</guid><title>Totals wrong on refunds (2 stars)</title>
<link>https://wordpress.org/support/topic/broken-totals/</link><pubDate>Mon, 19 Oct 2026 10:00:00 +0000</pubDate><dc:creator>shopowner</dc:creator>
<description><![CDATA[<p>Replies: 0</p><p>Rating: 2 stars</p><p class="x">Invoice totals ignore partial refunds &amp; we fix them by hand.</p>]]></description></item>
<item><guid>https://wordpress.org/support/topic/great/</guid><title>Great (5 stars)</title>
<link>https://wordpress.org/support/topic/great/</link><pubDate>Mon, 19 Oct 2026 11:00:00 +0000</pubDate><dc:creator>fan</dc:creator>
<description><![CDATA[<p>Rating: 5 stars</p><p>Great.</p>]]></description></item>
</channel></rss>`

func TestWordPress(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/plugins/info/1.2/", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("action") != "query_plugins" || q.Get("request[search]") != "invoice" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"info": map[string]any{"page": 1, "pages": 1},
			"plugins": []any{
				map[string]any{"name": "Invoices &amp; Slips", "slug": "invoices", "short_description": "Make invoices.",
					"tags": map[string]any{"invoice": "invoice", "pdf": "pdf"}, "active_installs": 5000, "rating": 84, "num_ratings": 40,
					"author": `<a href="https://example.com">Acme</a>`},
				map[string]any{"name": "Tiny", "slug": "tiny", "tags": []any{}, "active_installs": 10},
			},
		})
	})
	mux.HandleFunc("/support/plugin/invoices/reviews/feed/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(wpFeed)) //nolint:errcheck
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newConn(t, map[string]any{"platform": "wordpress", "base_url": srv.URL, "search": "invoice", "reviews_per_app": 10, "min_installs": 100})
	var s sink
	cursor, err := c.Fetch(context.Background(), s.env(srv.Client()), nil, s.emit)
	if err != nil {
		t.Fatal(err)
	}
	if string(cursor) != `{"offset":0}` {
		t.Fatalf("single-page catalogue should wrap: %s", cursor)
	}
	if len(s.docs) != 2 { // listing + the 2-star review; "tiny" is below min_installs
		t.Fatalf("docs = %d: %+v", len(s.docs), s.docs)
	}
	if !strings.Contains(string(s.docs[0].Content), `"Invoices & Slips"`) || s.docs[0].OrgHint != "Acme" {
		t.Fatalf("listing = %s org=%q", s.docs[0].Content, s.docs[0].OrgHint)
	}
	rev := string(s.docs[1].Content)
	if !strings.Contains(rev, "Invoice totals ignore partial refunds & we fix them by hand.") || strings.Contains(rev, "Rating:") ||
		!strings.Contains(rev, `"stars": 2`) || !strings.Contains(rev, `"title": "Totals wrong on refunds"`) {
		t.Fatalf("review = %s", rev)
	}
	if s.metrics["wordpress:invoices/avg_stars"] != 4.2 {
		t.Fatalf("avg stars = %v", s.metrics["wordpress:invoices/avg_stars"])
	}
}

func TestUnknownPlatform(t *testing.T) {
	if _, err := connectors.New(Type, json.RawMessage(`{"platform":"nope"}`)); err == nil {
		t.Fatal("unknown platform must be rejected")
	}
}
