// Package community collects discussion where practitioners describe their
// work: Hacker News stories and comments, and GitHub issues. Queries come
// from the source config, aimed at the founder's domains. Documents use the
// same shape the research agent's search tools save, so extraction and tier
// rules treat both alike (community kind: complaints are T4, engagement T5).
package community

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kwant-dbg/auto/internal/connectors"
	"github.com/kwant-dbg/auto/internal/store"
	"github.com/kwant-dbg/auto/internal/textutil"
)

// HNType is the source type for Hacker News collection.
const HNType = "hn_search"

// HNConfig configures Hacker News collection (Algolia's HN Search API).
type HNConfig struct {
	BaseURL      string   `json:"base_url"` // default https://hn.algolia.com
	Queries      []string `json:"queries"`
	Tags         string   `json:"tags"`           // default "(story,comment)"
	HitsPerQuery int      `json:"hits_per_query"` // per run, default 40, max 1000 (the API's page limit)
	LookbackDays int      `json:"lookback_days"`  // first run, default 14
	MinTextChars int      `json:"min_text_chars"` // skip short comments, default 160
	// SkipStories drops items in threads whose title starts with one of these
	// (the monthly hiring threads mention every tool by name). Defaults to
	// DefaultSkipStories.
	SkipStories []string `json:"skip_stories"`
}

// DefaultSkipStories are threads that mention tools without discussing them.
var DefaultSkipStories = []string{"Ask HN: Who is hiring?", "Ask HN: Who wants to be hired?", "Ask HN: Freelancer? Seeking freelancer?"}

// hnCursor is the newest item time seen per query.
type hnCursor struct {
	Since map[string]int64 `json:"since"`
}

type hn struct{ cfg HNConfig }

func init() {
	connectors.Register(HNType, func(raw json.RawMessage) (connectors.Connector, error) {
		var c HNConfig
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &c); err != nil {
				return nil, fmt.Errorf("hn_search config: %w", err)
			}
		}
		if len(c.Queries) == 0 {
			return nil, fmt.Errorf("hn_search config: no queries")
		}
		if c.BaseURL == "" {
			c.BaseURL = "https://hn.algolia.com"
		}
		if c.Tags == "" {
			c.Tags = "(story,comment)"
		}
		if c.HitsPerQuery <= 0 {
			c.HitsPerQuery = 40
		}
		c.HitsPerQuery = min(c.HitsPerQuery, 1000)
		if c.LookbackDays <= 0 {
			c.LookbackDays = 14
		}
		if c.MinTextChars <= 0 {
			c.MinTextChars = 160
		}
		if c.SkipStories == nil {
			c.SkipStories = DefaultSkipStories
		}
		return &hn{cfg: c}, nil
	})
}

type hnHit struct {
	ObjectID    string `json:"objectID"`
	Title       string `json:"title"`
	StoryTitle  string `json:"story_title"`
	StoryText   string `json:"story_text"`
	CommentText string `json:"comment_text"`
	Author      string `json:"author"`
	CreatedAt   string `json:"created_at"`
	CreatedAtI  int64  `json:"created_at_i"`
	Points      int    `json:"points"`
	NumComments int    `json:"num_comments"`
	URL         string `json:"url"`
}

// Fetch runs each query for items newer than the last run. Link-only
// stories and short comments carry no evidence and are skipped. Points and
// comment counts are kept as they were at first sight (the item isn't
// re-fetched), so documents don't churn.
func (h *hn) Fetch(ctx context.Context, env connectors.Env, cursor json.RawMessage, emit connectors.Emit) (json.RawMessage, error) {
	cur := hnCursor{Since: map[string]int64{}}
	if len(cursor) > 0 {
		_ = json.Unmarshal(cursor, &cur)
		if cur.Since == nil {
			cur.Since = map[string]int64{}
		}
	}
	floor := time.Now().AddDate(0, 0, -h.cfg.LookbackDays).Unix()
	save := func() json.RawMessage { b, _ := json.Marshal(cur); return b }
	for _, q := range h.cfg.Queries {
		since := max(cur.Since[q], floor)
		v := url.Values{}
		// Exact matching: typo tolerance turns "dagster" into "disaster", and
		// unquoted words match any long comment containing both.
		v.Set("query", phrase(q))
		v.Set("advancedSyntax", "true")
		v.Set("typoTolerance", "false")
		v.Set("tags", h.cfg.Tags)
		v.Set("hitsPerPage", strconv.Itoa(h.cfg.HitsPerQuery))
		v.Set("numericFilters", fmt.Sprintf("created_at_i>%d", since))
		var resp struct {
			Hits []hnHit `json:"hits"`
		}
		if err := connectors.GetJSON(ctx, env, h.cfg.BaseURL+"/api/v1/search_by_date?"+v.Encode(), &resp); err != nil {
			return save(), fmt.Errorf("query %q: %w", q, err)
		}
		newest := since
		for _, it := range resp.Hits {
			newest = max(newest, it.CreatedAtI)
			d, ok := h.document(it)
			if !ok {
				continue
			}
			if err := emit(d); err != nil {
				return save(), err
			}
		}
		cur.Since[q] = newest
	}
	return save(), nil
}

// phrase quotes a multi-word query so it matches as a phrase; queries that
// already use quotes or a minus sign are left as written.
func phrase(q string) string {
	q = strings.TrimSpace(q)
	if strings.ContainsAny(q, `"-`) || !strings.Contains(q, " ") {
		return q
	}
	return `"` + q + `"`
}

func (h *hn) document(it hnHit) (store.Document, bool) {
	title := it.Title
	if title == "" {
		title = it.StoryTitle
	}
	for _, skip := range h.cfg.SkipStories {
		if strings.HasPrefix(it.StoryTitle, skip) || strings.HasPrefix(it.Title, skip) {
			return store.Document{}, false
		}
	}
	text := strings.TrimSpace(textutil.HTMLFragmentToText(it.CommentText + it.StoryText))
	kind := "story"
	if it.CommentText != "" {
		kind = "comment"
	}
	if len(text) < h.cfg.MinTextChars {
		return store.Document{}, false
	}
	item := map[string]any{"kind": kind, "title": title, "text": text, "created_at": it.CreatedAt, "points": it.Points, "num_comments": it.NumComments}
	if it.URL != "" {
		item["link"] = it.URL
	}
	content, err := connectors.MarshalVerbatim(item)
	if err != nil {
		return store.Document{}, false
	}
	d := store.Document{
		CanonicalURL: "https://news.ycombinator.com/item?id=" + url.QueryEscape(it.ObjectID),
		ExternalID:   it.ObjectID, MIME: "application/json", Content: content, AuthorHash: store.AuthorHash(it.Author),
	}
	if it.CreatedAtI > 0 {
		t := time.Unix(it.CreatedAtI, 0).UTC()
		d.PublishedAt = &t
	}
	return d, true
}
