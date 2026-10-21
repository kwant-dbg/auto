package usaspending

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kwant-dbg/auto/internal/connectors"
	"github.com/kwant-dbg/auto/internal/store"
)

type fakeAPI struct {
	mu       sync.Mutex
	requests []map[string]any
	pages    int // pages per window
	failOn   string
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/api/v2/search/spending_by_award/" {
		http.NotFound(w, r)
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	f.mu.Lock()
	f.requests = append(f.requests, body)
	f.mu.Unlock()

	filters := body["filters"].(map[string]any)
	tp := filters["time_period"].([]any)[0].(map[string]any)
	startDate := tp["start_date"].(string)
	if startDate == f.failOn {
		http.Error(w, "upstream exploded", http.StatusBadRequest)
		return
	}
	page := int(body["page"].(float64))
	results := []map[string]any{{
		"Award ID":              "AWD-" + startDate + "-" + string(rune('0'+page)),
		"generated_internal_id": "CONT_AWD_" + startDate + "_" + string(rune('0'+page)),
		"Description":           "RECORDS MANAGEMENT SERVICES",
		"Award Amount":          42000.0,
		"Start Date":            startDate,
		"Awarding Agency":       "Department of Examples",
	}}
	json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
		"results":       results,
		"page_metadata": map[string]any{"page": page, "hasNext": page < f.pages},
	})
}

func newConnector(t *testing.T, base string, extra map[string]any) *Connector {
	t.Helper()
	cfg := map[string]any{"base_url": base, "window_days": 1, "max_windows_per_run": 10}
	for k, v := range extra {
		cfg[k] = v
	}
	raw, _ := json.Marshal(cfg)
	c, err := connectors.New(Type, raw)
	if err != nil {
		t.Fatal(err)
	}
	return c.(*Connector)
}

func fixedNow() time.Time { return time.Date(2026, 10, 5, 15, 0, 0, 0, time.UTC) }

func TestFetchWindowsAndPaging(t *testing.T) {
	api := &fakeAPI{pages: 2}
	srv := httptest.NewServer(api)
	defer srv.Close()
	c := newConnector(t, srv.URL, map[string]any{"start_date": "2026-10-01", "keywords": []string{"records"},
		"filters": map[string]any{"naics_codes": []string{"5614"}}})

	var docs []store.Document
	env := connectors.Env{HTTP: srv.Client(), Limiter: connectors.NewLimiter(6000), Now: fixedNow}
	cursor, err := c.Fetch(context.Background(), env, nil, func(d store.Document) error {
		docs = append(docs, d)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Oct 1–4 are complete days before "today" (Oct 5): 4 windows × 2 pages.
	if len(docs) != 8 {
		t.Fatalf("got %d documents, want 8", len(docs))
	}
	if string(cursor) != `{"next_start":"2026-10-05"}` {
		t.Fatalf("cursor = %s", cursor)
	}
	d := docs[0]
	if d.CanonicalURL != "https://www.usaspending.gov/award/CONT_AWD_2026-10-01_1" || d.MIME != "application/json" ||
		d.PublishedAt == nil || d.OrgHint != "Department of Examples" {
		t.Fatalf("document = %+v", d)
	}
	req := api.requests[0]
	f := req["filters"].(map[string]any)
	if f["keywords"] == nil || f["naics_codes"] == nil || f["award_amounts"] == nil {
		t.Fatalf("filters not passed through: %v", f)
	}

	// Next run resumes from the cursor and has nothing complete to fetch yet.
	api.requests = nil
	cursor2, err := c.Fetch(context.Background(), env, cursor, func(store.Document) error { return nil })
	if err != nil || string(cursor2) != string(cursor) || len(api.requests) != 0 {
		t.Fatalf("resume: cursor=%s err=%v requests=%d", cursor2, err, len(api.requests))
	}
}

func TestFetchKeepsProgressOnError(t *testing.T) {
	api := &fakeAPI{pages: 1, failOn: "2026-10-03"}
	srv := httptest.NewServer(api)
	defer srv.Close()
	c := newConnector(t, srv.URL, map[string]any{"start_date": "2026-10-01"})
	env := connectors.Env{HTTP: srv.Client(), Now: fixedNow}
	n := 0
	cursor, err := c.Fetch(context.Background(), env, nil, func(store.Document) error { n++; return nil })
	if err == nil {
		t.Fatal("want an error from the failing window")
	}
	if string(cursor) != `{"next_start":"2026-10-03"}` || n != 2 {
		t.Fatalf("cursor = %s, docs = %d; want progress kept up to the failed window", cursor, n)
	}
}

func TestWindowTooLarge(t *testing.T) {
	api := &fakeAPI{pages: 5}
	srv := httptest.NewServer(api)
	defer srv.Close()
	c := newConnector(t, srv.URL, map[string]any{"start_date": "2026-10-04", "max_pages_per_window": 2})
	_, err := c.Fetch(context.Background(), connectors.Env{HTTP: srv.Client(), Now: fixedNow}, nil,
		func(store.Document) error { return nil })
	if !errors.Is(err, ErrWindowTooLarge) {
		t.Fatalf("err = %v, want ErrWindowTooLarge rather than silent truncation", err)
	}
}

func TestDocumentsAreDeterministic(t *testing.T) {
	row := map[string]any{"b": 1, "a": "x", "Award ID": "A1"}
	d1, _ := toDocument(row)
	d2, _ := toDocument(map[string]any{"Award ID": "A1", "a": "x", "b": 1})
	if string(d1.Content) != string(d2.Content) {
		t.Fatal("same row must serialise identically so unchanged awards aren't re-versioned")
	}
	if _, err := toDocument(map[string]any{"Description": "no ids"}); err == nil {
		t.Fatal("rows without identifiers must be rejected")
	}
}

func TestDocumentsKeepTextVerbatim(t *testing.T) {
	d, err := toDocument(map[string]any{"Award ID": "A1", "Recipient Name": "J. & L. <Co>"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(d.Content), `"J. & L. <Co>"`) {
		t.Fatalf("text was escaped: %s", d.Content)
	}
}
