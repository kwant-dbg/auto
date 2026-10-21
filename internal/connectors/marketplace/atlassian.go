package marketplace

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/kwant-dbg/auto/internal/connectors"
	"github.com/kwant-dbg/auto/internal/store"
)

// atlassian reads the public Atlassian Marketplace REST API (v2).
type atlassian struct{ cfg *Config }

func newAtlassian(cfg *Config) *atlassian {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://marketplace.atlassian.com"
	}
	if cfg.PageSize <= 0 || cfg.PageSize > 50 {
		cfg.PageSize = 50
	}
	if cfg.Hosting == "" {
		cfg.Hosting = "cloud"
	}
	if cfg.Cost == "" {
		cfg.Cost = "paid" // paid apps: the spend signal
	}
	return &atlassian{cfg: cfg}
}

type atlAddon struct {
	ID       string         `json:"id"`
	Key      string         `json:"key"`
	Name     string         `json:"name"`
	TagLine  string         `json:"tagLine"`
	Summary  string         `json:"summary"`
	Links    map[string]any `json:"_links"`
	Embedded struct {
		Categories []struct {
			Name string `json:"name"`
		} `json:"categories"`
		Distribution struct {
			Downloads     float64 `json:"downloads"`
			TotalInstalls float64 `json:"totalInstalls"`
		} `json:"distribution"`
		Reviews struct {
			AverageStars float64 `json:"averageStars"`
			Count        float64 `json:"count"`
		} `json:"reviews"`
		Vendor struct {
			Name string `json:"name"`
		} `json:"vendor"`
	} `json:"_embedded"`
}

type atlPage struct {
	Count    int `json:"count"`
	Embedded struct {
		Addons []atlAddon `json:"addons"`
	} `json:"_embedded"`
}

// link returns the first link of the given rel and type ("" matches any type).
func link(links map[string]any, rel, typ string) string {
	pick := func(m map[string]any) string {
		if h, _ := m["href"].(string); h != "" && (typ == "" || m["type"] == typ) {
			return h
		}
		return ""
	}
	switch v := links[rel].(type) {
	case map[string]any:
		return pick(v)
	case []any:
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				if h := pick(m); h != "" {
					return h
				}
			}
		}
	}
	return ""
}

func (a *atlassian) fetchPage(ctx context.Context, env connectors.Env, offset, limit int) ([]app, int, error) {
	q := url.Values{}
	q.Set("offset", strconv.Itoa(offset))
	q.Set("limit", strconv.Itoa(limit))
	q.Set("hosting", a.cfg.Hosting)
	q.Set("cost", a.cfg.Cost)
	if a.cfg.Application != "" {
		q.Set("application", a.cfg.Application)
	}
	var page atlPage
	if err := connectors.GetJSON(ctx, env, a.cfg.BaseURL+"/rest/2/addons?"+q.Encode(), &page); err != nil {
		return nil, offset, err
	}
	apps := make([]app, 0, len(page.Embedded.Addons))
	for _, ad := range page.Embedded.Addons {
		ap := app{
			Key: ad.Key, Name: ad.Name, Tagline: ad.TagLine, Summary: ad.Summary,
			Vendor:   ad.Embedded.Vendor.Name,
			installs: ad.Embedded.Distribution.TotalInstalls,
			metrics: map[string]float64{
				"installs":     ad.Embedded.Distribution.TotalInstalls,
				"downloads":    ad.Embedded.Distribution.Downloads,
				"avg_stars":    ad.Embedded.Reviews.AverageStars,
				"review_count": ad.Embedded.Reviews.Count,
			},
			reviewsHref: link(ad.Links, "reviews", ""),
		}
		for _, c := range ad.Embedded.Categories {
			ap.Categories = append(ap.Categories, c.Name)
		}
		if alt := link(ad.Links, "alternate", "text/html"); alt != "" {
			ap.URL = a.cfg.BaseURL + alt
		} else {
			ap.URL = a.cfg.BaseURL + "/apps/" + url.PathEscape(ad.ID)
		}
		if a.cfg.IncludePricing {
			if err := a.pricing(ctx, env, &ap); err != nil {
				return nil, offset, err
			}
		}
		apps = append(apps, ap)
	}
	next := offset + len(page.Embedded.Addons)
	if len(page.Embedded.Addons) == 0 || next >= page.Count {
		next = 0
	}
	return apps, next, nil
}

type atlPricing struct {
	Items []struct {
		EditionDescription string  `json:"editionDescription"`
		Amount             float64 `json:"amount"`
		MonthsValid        int     `json:"monthsValid"`
		UnitCount          int     `json:"unitCount"`
		LicenseType        string  `json:"licenseType"`
	} `json:"items"`
}

// pricing adds monthly commercial price points and records the cheapest paid
// tier plus common user tiers as metrics.
func (a *atlassian) pricing(ctx context.Context, env connectors.Env, ap *app) error {
	hosting := a.cfg.Hosting
	var p atlPricing
	err := connectors.GetJSON(ctx, env, fmt.Sprintf("%s/rest/2/addons/%s/pricing/%s/live", a.cfg.BaseURL, url.PathEscape(ap.Key), hosting), &p)
	if err != nil {
		if strings.Contains(err.Error(), "HTTP 404") { // no published pricing
			return nil
		}
		return err
	}
	minPaid := 0.0
	for _, it := range p.Items {
		if it.LicenseType != "" && it.LicenseType != "COMMERCIAL" {
			continue
		}
		if it.MonthsValid != 1 {
			continue
		}
		ap.Pricing = append(ap.Pricing, pricePoint{Edition: it.EditionDescription, AmountUSD: it.Amount, MonthsValid: it.MonthsValid, Units: it.UnitCount})
		if it.Amount > 0 && (minPaid == 0 || it.Amount < minPaid) {
			minPaid = it.Amount
		}
		switch it.UnitCount {
		case 10, 50, 100, 500:
			ap.metrics[fmt.Sprintf("price_usd_month_%d_users", it.UnitCount)] = it.Amount
		}
	}
	if minPaid > 0 {
		ap.metrics["price_usd_month_min_paid"] = minPaid
	}
	return nil
}

type atlReviews struct {
	Embedded struct {
		Reviews []struct {
			ID       string  `json:"id"`
			Review   string  `json:"review"`
			Stars    float64 `json:"stars"`
			Date     string  `json:"date"`
			Embedded struct {
				Author struct {
					Name string `json:"name"`
				} `json:"author"`
			} `json:"_embedded"`
		} `json:"reviews"`
	} `json:"_embedded"`
}

func (a *atlassian) reviews(ctx context.Context, env connectors.Env, ap app, limit int) ([]review, error) {
	href := ap.reviewsHref
	if i := strings.IndexByte(href, '{'); i >= 0 { // strip URI-template suffix
		href = href[:i]
	}
	if href == "" {
		href = "/rest/2/addons/" + url.PathEscape(ap.Key) + "/reviews"
	}
	var r atlReviews
	if err := connectors.GetJSON(ctx, env, fmt.Sprintf("%s%s?sort=recent&limit=%d", a.cfg.BaseURL, href, min(limit, 100)), &r); err != nil {
		return nil, err
	}
	out := make([]review, 0, len(r.Embedded.Reviews))
	for _, rv := range r.Embedded.Reviews {
		out = append(out, review{
			ID: rv.ID, URL: ap.URL + "#review-" + url.QueryEscape(rv.ID), Stars: int(rv.Stars + 0.5), Date: rv.Date,
			Text: strings.TrimSpace(rv.Review), AuthorHash: hashAuthor(rv.Embedded.Author.Name),
		})
	}
	return out, nil
}

// hashAuthor keeps reviewer identity only as a hash, for independence counting.
func hashAuthor(name string) string {
	name = strings.TrimSpace(strings.ToLower(name))
	if name == "" {
		return ""
	}
	return store.Hash([]byte("author:" + name))[:16]
}
