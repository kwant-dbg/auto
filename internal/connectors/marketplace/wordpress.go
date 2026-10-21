package marketplace

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/kwant-dbg/auto/internal/connectors"
	"github.com/kwant-dbg/auto/internal/textutil"
)

// wordpress reads the WordPress.org plugin directory API and each plugin's
// public reviews feed.
type wordpress struct{ cfg *Config }

func newWordPress(cfg *Config) *wordpress {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.wordpress.org"
	}
	if cfg.PageSize <= 0 || cfg.PageSize > 250 {
		cfg.PageSize = 100
	}
	if cfg.Browse == "" && cfg.Search == "" && cfg.Tag == "" {
		cfg.Browse = "popular"
	}
	return &wordpress{cfg: cfg}
}

type wpPlugin struct {
	Name             string         `json:"name"`
	Slug             string         `json:"slug"`
	ShortDescription string         `json:"short_description"`
	Homepage         string         `json:"homepage"`
	Tags             map[string]any `json:"tags"` // object keyed by slug, or [] when empty
	ActiveInstalls   float64        `json:"active_installs"`
	Rating           float64        `json:"rating"` // 0–100
	NumRatings       float64        `json:"num_ratings"`
	SupportThreads   float64        `json:"support_threads"`
	SupportResolved  float64        `json:"support_threads_resolved"`
	Author           string         `json:"author"`
}

type wpPage struct {
	Info struct {
		Page  int `json:"page"`
		Pages int `json:"pages"`
	} `json:"info"`
	Plugins []wpPlugin `json:"plugins"`
}

// UnmarshalJSON tolerates "tags": [] for plugins without tags.
func (p *wpPlugin) UnmarshalJSON(b []byte) error {
	type plain wpPlugin
	var aux struct {
		plain
		Tags any `json:"tags"`
	}
	if err := jsonUnmarshal(b, &aux); err != nil {
		return err
	}
	*p = wpPlugin(aux.plain)
	if m, ok := aux.Tags.(map[string]any); ok {
		p.Tags = m
	}
	return nil
}

var authorTagRe = regexp.MustCompile(`<[^>]*>`)

func (w *wordpress) fetchPage(ctx context.Context, env connectors.Env, offset, limit int) ([]app, int, error) {
	// The API pages by page number; offsets stay aligned to PageSize.
	size := w.cfg.PageSize
	page := offset/size + 1
	q := url.Values{}
	q.Set("action", "query_plugins")
	q.Set("request[page]", strconv.Itoa(page))
	q.Set("request[per_page]", strconv.Itoa(size))
	switch {
	case w.cfg.Search != "":
		q.Set("request[search]", w.cfg.Search)
	case w.cfg.Tag != "":
		q.Set("request[tag]", w.cfg.Tag)
	default:
		q.Set("request[browse]", w.cfg.Browse)
	}
	for _, f := range []string{"description", "sections", "versions", "screenshots", "banners", "icons", "contributors", "donate_link"} {
		q.Set("request[fields]["+f+"]", "0")
	}
	var p wpPage
	if err := connectors.GetJSON(ctx, env, w.cfg.BaseURL+"/plugins/info/1.2/?"+q.Encode(), &p); err != nil {
		return nil, offset, err
	}
	apps := make([]app, 0, len(p.Plugins))
	for _, pl := range p.Plugins {
		ap := app{
			Key: pl.Slug, Name: textutil.HTMLFragmentToText(pl.Name),
			URL:      "https://wordpress.org/plugins/" + url.PathEscape(pl.Slug) + "/",
			Summary:  textutil.HTMLFragmentToText(pl.ShortDescription),
			Vendor:   strings.TrimSpace(authorTagRe.ReplaceAllString(pl.Author, "")),
			installs: pl.ActiveInstalls,
			metrics: map[string]float64{
				"installs":                 pl.ActiveInstalls,
				"avg_stars":                pl.Rating / 20,
				"review_count":             pl.NumRatings,
				"support_threads":          pl.SupportThreads,
				"support_threads_resolved": pl.SupportResolved,
			},
		}
		if pl.Homepage != "" {
			ap.Extra = map[string]any{"homepage": pl.Homepage}
		}
		for _, t := range pl.Tags {
			if s, ok := t.(string); ok {
				ap.Categories = append(ap.Categories, s)
			}
		}
		sortStrings(ap.Categories)
		apps = append(apps, ap)
	}
	next := offset + size
	if len(p.Plugins) == 0 || page >= p.Info.Pages {
		next = 0
	}
	return apps, next, nil
}

type rss struct {
	Channel struct {
		Items []struct {
			GUID        string `xml:"guid"`
			Title       string `xml:"title"`
			Link        string `xml:"link"`
			PubDate     string `xml:"pubDate"`
			Creator     string `xml:"http://purl.org/dc/elements/1.1/ creator"`
			Description string `xml:"description"`
		} `xml:"item"`
	} `xml:"channel"`
}

var (
	starsTitleRe = regexp.MustCompile(`\((\d) stars?\)\s*$`)
	starsBodyRe  = regexp.MustCompile(`(?m)^Rating: (\d) stars?$`)
	repliesRe    = regexp.MustCompile(`(?m)^Replies: \d+$`)
)

func (w *wordpress) reviews(ctx context.Context, env connectors.Env, ap app, limit int) ([]review, error) {
	feed := w.cfg.BaseURL
	if strings.Contains(feed, "api.wordpress.org") {
		feed = "https://wordpress.org"
	}
	body, err := connectors.Get(ctx, env, fmt.Sprintf("%s/support/plugin/%s/reviews/feed/", feed, url.PathEscape(ap.Key)))
	if err != nil {
		if strings.Contains(err.Error(), "HTTP 404") {
			return nil, nil
		}
		return nil, err
	}
	var doc rss
	if err := xml.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("reviews feed %s: %w", ap.Key, err)
	}
	var out []review
	for _, it := range doc.Channel.Items {
		if len(out) >= limit {
			break
		}
		text := textutil.HTMLFragmentToText(it.Description)
		stars := 0
		if m := starsBodyRe.FindStringSubmatch(text); m != nil {
			stars, _ = strconv.Atoi(m[1])
		} else if m := starsTitleRe.FindStringSubmatch(it.Title); m != nil {
			stars, _ = strconv.Atoi(m[1])
		}
		text = strings.TrimSpace(repliesRe.ReplaceAllString(starsBodyRe.ReplaceAllString(text, ""), ""))
		title := strings.TrimSpace(starsTitleRe.ReplaceAllString(it.Title, ""))
		id := it.GUID
		if id == "" {
			id = it.Link
		}
		out = append(out, review{
			ID: id, URL: it.Link, Stars: stars, Date: it.PubDate, Title: title,
			Text: textutil.Normalize(text), AuthorHash: hashAuthor(it.Creator),
		})
	}
	return out, nil
}
