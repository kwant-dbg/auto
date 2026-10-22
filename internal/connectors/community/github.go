package community

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kwant-dbg/auto/internal/connectors"
	"github.com/kwant-dbg/auto/internal/store"
)

// GitHubType is the source type for GitHub issue collection.
const GitHubType = "github_search"

// GitHubConfig configures GitHub issue collection (the issue search API).
// Queries are GitHub search qualifiers, e.g.
// "is:issue is:open label:enhancement reactions:>25 kubernetes".
type GitHubConfig struct {
	BaseURL      string   `json:"base_url"` // default https://api.github.com
	Queries      []string `json:"queries"`
	PerQuery     int      `json:"per_query"`      // default 30, max 100
	Sort         string   `json:"sort"`           // default "reactions-+1" (most upvoted first)
	MinBodyChars int      `json:"min_body_chars"` // skip near-empty issues, default 200
}

type gh struct{ cfg GitHubConfig }

func init() {
	connectors.Register(GitHubType, func(raw json.RawMessage) (connectors.Connector, error) {
		var c GitHubConfig
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &c); err != nil {
				return nil, fmt.Errorf("github_search config: %w", err)
			}
		}
		if len(c.Queries) == 0 {
			return nil, fmt.Errorf("github_search config: no queries")
		}
		if c.BaseURL == "" {
			c.BaseURL = "https://api.github.com"
		}
		if c.PerQuery <= 0 {
			c.PerQuery = 30
		}
		c.PerQuery = min(c.PerQuery, 100)
		if c.Sort == "" {
			c.Sort = "reactions-+1"
		}
		if c.MinBodyChars <= 0 {
			c.MinBodyChars = 200
		}
		return &gh{cfg: c}, nil
	})
}

type ghIssue struct {
	HTMLURL       string `json:"html_url"`
	Title         string `json:"title"`
	Body          string `json:"body"`
	State         string `json:"state"`
	Comments      int    `json:"comments"`
	CreatedAt     string `json:"created_at"`
	RepositoryURL string `json:"repository_url"`
	PullRequest   *struct {
		URL string `json:"url"`
	} `json:"pull_request"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	Reactions struct {
		TotalCount int `json:"total_count"`
	} `json:"reactions"`
}

// Fetch runs each query. Reaction and comment counts are recorded as daily
// metrics as well as in the document, so demand can be tracked over time;
// a changed count makes a new document version, which is why this source
// runs weekly rather than daily. Set GITHUB_TOKEN for the higher search
// rate limit.
func (g *gh) Fetch(ctx context.Context, env connectors.Env, cursor json.RawMessage, emit connectors.Emit) (json.RawMessage, error) {
	token := os.Getenv("GITHUB_TOKEN")
	for _, q := range g.cfg.Queries {
		if !strings.Contains(q, "is:issue") && !strings.Contains(q, "type:issue") {
			q += " is:issue"
		}
		issues, err := g.search(ctx, env, token, q)
		if err != nil {
			return cursor, fmt.Errorf("query %q: %w", q, err)
		}
		for _, it := range issues {
			if it.PullRequest != nil || len(strings.TrimSpace(it.Body)) < g.cfg.MinBodyChars {
				continue
			}
			repo := strings.TrimPrefix(it.RepositoryURL, g.cfg.BaseURL+"/repos/")
			content, err := connectors.MarshalVerbatim(map[string]any{"kind": "issue", "repository": repo, "title": it.Title,
				"body": truncate(it.Body, 20_000), "state": it.State, "comments": it.Comments, "reactions": it.Reactions.TotalCount, "created_at": it.CreatedAt})
			if err != nil {
				return cursor, err
			}
			d := store.Document{CanonicalURL: it.HTMLURL, MIME: "application/json", Content: content,
				AuthorHash: store.AuthorHash(it.User.Login), OrgHint: repo}
			if t, err := time.Parse(time.RFC3339, it.CreatedAt); err == nil {
				d.PublishedAt = &t
			}
			if err := emit(d); err != nil {
				return cursor, err
			}
			if err := env.RecordMetric(it.HTMLURL, "reactions", float64(it.Reactions.TotalCount)); err != nil {
				return cursor, err
			}
			if err := env.RecordMetric(it.HTMLURL, "comments", float64(it.Comments)); err != nil {
				return cursor, err
			}
		}
	}
	return cursor, nil
}

func (g *gh) search(ctx context.Context, env connectors.Env, token, q string) ([]ghIssue, error) {
	if env.Limiter != nil {
		if err := env.Limiter.Wait(ctx); err != nil {
			return nil, err
		}
	}
	v := url.Values{}
	v.Set("q", q)
	v.Set("sort", g.cfg.Sort)
	v.Set("order", "desc")
	v.Set("per_page", strconv.Itoa(g.cfg.PerQuery))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.cfg.BaseURL+"/search/issues?"+v.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if env.UserAgent != "" {
		req.Header.Set("User-Agent", env.UserAgent)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := env.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, connectors.MaxResponseBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub search: HTTP %d: %.300s", resp.StatusCode, body)
	}
	var r struct {
		Items []ghIssue `json:"items"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	return r.Items, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
