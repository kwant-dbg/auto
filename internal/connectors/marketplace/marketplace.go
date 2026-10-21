// Package marketplace collects app-marketplace listings and low-star reviews
// (discovery lens 2, docs/02 §3.2). One connector type serves every platform
// through small adapters, so adding a marketplace is configuration plus an
// adapter, never a new pipeline.
//
// Marketplaces are an evidence source, not a limit on what AOE recommends:
// listings show paid demand and its size, low-star reviews name what's missing.
//
// Documents hold text only (name, description, pricing, review text). Counters
// that change daily (installs, ratings) are recorded as metrics, so they don't
// create a new document version, and a re-extraction, every day.
package marketplace

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kwant-dbg/auto/internal/connectors"
	"github.com/kwant-dbg/auto/internal/store"
)

// Type is the source type name.
const Type = "app_marketplace"

// Config is the per-source configuration shared by all platforms.
type Config struct {
	Platform       string `json:"platform"` // atlassian | wordpress
	BaseURL        string `json:"base_url"`
	MaxAppsPerRun  int    `json:"max_apps_per_run"`
	PageSize       int    `json:"page_size"`
	ReviewsPerApp  int    `json:"reviews_per_app"`  // 0 disables reviews
	MaxReviewStars int    `json:"max_review_stars"` // keep reviews at or below this rating
	MinInstalls    int    `json:"min_installs"`     // skip apps below this install count

	// Atlassian
	Hosting        string `json:"hosting"`     // cloud | server | datacenter
	Cost           string `json:"cost"`        // free | paid | marketplace | orderable
	Application    string `json:"application"` // e.g. jira, confluence
	IncludePricing bool   `json:"include_pricing"`

	// WordPress
	Browse string `json:"browse"` // popular | new | updated | top-rated
	Search string `json:"search"`
	Tag    string `json:"tag"`
}

func (c *Config) defaults() {
	if c.MaxAppsPerRun <= 0 {
		c.MaxAppsPerRun = 100
	}
	if c.ReviewsPerApp < 0 {
		c.ReviewsPerApp = 0
	}
	if c.MaxReviewStars <= 0 {
		c.MaxReviewStars = 3
	}
}

// adapter is one platform.
type adapter interface {
	// fetchPage returns apps starting at offset, and the offset to resume from
	// (0 when the catalogue wrapped around).
	fetchPage(ctx context.Context, env connectors.Env, offset, limit int) (apps []app, next int, err error)
	// reviews returns an app's recent reviews.
	reviews(ctx context.Context, env connectors.Env, a app, limit int) ([]review, error)
}

// app is a normalised listing.
type app struct {
	Key         string         `json:"key"`
	Name        string         `json:"name"`
	URL         string         `json:"url"`
	Tagline     string         `json:"tagline,omitempty"`
	Summary     string         `json:"summary,omitempty"`
	Vendor      string         `json:"vendor,omitempty"`
	Categories  []string       `json:"categories,omitempty"`
	Pricing     []pricePoint   `json:"pricing,omitempty"`
	Extra       map[string]any `json:"extra,omitempty"`
	metrics     map[string]float64
	installs    float64
	reviewsHref string
}

type pricePoint struct {
	Edition     string  `json:"edition"`
	AmountUSD   float64 `json:"amount_usd"`
	MonthsValid int     `json:"months_valid"`
	Units       int     `json:"units,omitempty"`
}

// review is a normalised review.
type review struct {
	ID         string `json:"id"`
	URL        string `json:"-"`
	Stars      int    `json:"stars"`
	Date       string `json:"date"`
	Text       string `json:"review"`
	Title      string `json:"title,omitempty"`
	AuthorHash string `json:"-"`
}

// Cursor is the catalogue offset to resume from.
type Cursor struct {
	Offset int `json:"offset"`
}

// Connector implements connectors.Connector.
type Connector struct {
	cfg     Config
	adapter adapter
}

func init() {
	connectors.Register(Type, func(raw json.RawMessage) (connectors.Connector, error) {
		var cfg Config
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return nil, fmt.Errorf("%s config: %w", Type, err)
			}
		}
		cfg.defaults()
		var a adapter
		switch cfg.Platform {
		case "atlassian":
			a = newAtlassian(&cfg)
		case "wordpress":
			a = newWordPress(&cfg)
		default:
			return nil, fmt.Errorf("%s: unknown platform %q (have: atlassian, wordpress)", Type, cfg.Platform)
		}
		return &Connector{cfg: cfg, adapter: a}, nil
	})
}

// Fetch sweeps up to MaxAppsPerRun apps from the cursor, emitting a listing
// document and low-star review documents for each, and recording metrics.
// The sweep wraps around, so the whole catalogue is revisited over time.
func (c *Connector) Fetch(ctx context.Context, env connectors.Env, raw json.RawMessage, emit connectors.Emit) (json.RawMessage, error) {
	var cur Cursor
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &cur); err != nil {
			return nil, fmt.Errorf("cursor: %w", err)
		}
	}
	mk := func(o int) json.RawMessage { b, _ := json.Marshal(Cursor{Offset: o}); return b }

	processed := 0
	offset := cur.Offset
	for processed < c.cfg.MaxAppsPerRun {
		limit := min(c.cfg.PageSize, c.cfg.MaxAppsPerRun-processed)
		apps, next, err := c.adapter.fetchPage(ctx, env, offset, limit)
		if err != nil {
			return mk(offset), err
		}
		for _, a := range apps {
			if a.installs < float64(c.cfg.MinInstalls) {
				continue
			}
			if err := c.emitApp(ctx, env, a, emit); err != nil {
				return mk(offset), fmt.Errorf("app %s: %w", a.Key, err)
			}
		}
		processed += max(len(apps), 1)
		offset = next
		if next == 0 { // wrapped: stop for this run
			break
		}
	}
	return mk(offset), nil
}

func (c *Connector) emitApp(ctx context.Context, env connectors.Env, a app, emit connectors.Emit) error {
	subject := c.cfg.Platform + ":" + a.Key
	for name, v := range a.metrics {
		if err := env.RecordMetric(subject, name, v); err != nil {
			return err
		}
	}
	content, err := connectors.MarshalVerbatim(map[string]any{"platform": c.cfg.Platform, "kind": "listing", "app": a})
	if err != nil {
		return err
	}
	if err := emit(store.Document{CanonicalURL: a.URL, ExternalID: a.Key, MIME: "application/json", Content: content, OrgHint: a.Vendor}); err != nil {
		return err
	}
	if c.cfg.ReviewsPerApp == 0 {
		return nil
	}
	revs, err := c.adapter.reviews(ctx, env, a, c.cfg.ReviewsPerApp)
	if err != nil {
		return err
	}
	for _, r := range revs {
		if r.Stars > c.cfg.MaxReviewStars || r.Text == "" {
			continue
		}
		content, err := connectors.MarshalVerbatim(map[string]any{
			"platform": c.cfg.Platform, "kind": "review", "app_key": a.Key, "app_name": a.Name, "review": r,
		})
		if err != nil {
			return err
		}
		if err := emit(store.Document{CanonicalURL: r.URL, ExternalID: r.ID, MIME: "application/json",
			Content: content, AuthorHash: r.AuthorHash}); err != nil {
			return err
		}
	}
	return nil
}
