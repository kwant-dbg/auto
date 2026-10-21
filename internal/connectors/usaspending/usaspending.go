// Package usaspending collects US federal contract awards from the public
// USAspending.gov API (discovery lens 3, docs/02 §3.3): a public ledger of
// money actually paid for services, with descriptions of the work.
//
// Each award becomes one document whose content is the award row as JSON,
// so later extraction can quote it verbatim.
package usaspending

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/kwant-dbg/auto/internal/connectors"
	"github.com/kwant-dbg/auto/internal/store"
)

// Type is the source type name.
const Type = "usaspending_awards"

const defaultBaseURL = "https://api.usaspending.gov"

func init() {
	connectors.Register(Type, func(raw json.RawMessage) (connectors.Connector, error) {
		cfg := Config{}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return nil, fmt.Errorf("%s config: %w", Type, err)
			}
		}
		cfg.defaults()
		return &Connector{cfg: cfg}, nil
	})
}

// Config is the per-source configuration.
type Config struct {
	BaseURL        string   `json:"base_url"`
	AwardTypeCodes []string `json:"award_type_codes"` // A–D = contracts
	MinAmount      float64  `json:"min_amount"`
	MaxAmount      float64  `json:"max_amount"`
	Keywords       []string `json:"keywords"`
	// Filters are merged verbatim into the API's "filters" object, for
	// filters not modelled here (e.g. naics_codes, psc_codes, agencies).
	Filters map[string]any `json:"filters"`
	Fields  []string       `json:"fields"`
	// ServicesOnly drops product purchases (PSC codes starting with a digit);
	// AOE looks for work people are paid to do, not goods. Default true.
	ServicesOnly *bool `json:"services_only"`
	// ExcludePSCPrefixes drops service categories that are physical by
	// definition. Default Y (construction) and Z (real-property maintenance).
	ExcludePSCPrefixes []string `json:"exclude_psc_prefixes"`
	StartDate          string   `json:"start_date"`    // first window; default today − lookback_days
	LookbackDays       int      `json:"lookback_days"` // default 30
	WindowDays         int      `json:"window_days"`   // default 1
	MaxWindowsPerRun   int      `json:"max_windows_per_run"`
	PageSize           int      `json:"page_size"`
	MaxPagesPerWindow  int      `json:"max_pages_per_window"`
}

func (c *Config) defaults() {
	if c.BaseURL == "" {
		c.BaseURL = defaultBaseURL
	}
	if len(c.AwardTypeCodes) == 0 {
		c.AwardTypeCodes = []string{"A", "B", "C", "D"}
	}
	if c.MinAmount == 0 && c.MaxAmount == 0 {
		// Small recurring service contracts: the solo-founder sweet spot.
		c.MinAmount, c.MaxAmount = 5_000, 250_000
	}
	if len(c.Fields) == 0 {
		c.Fields = []string{
			"Award ID", "Recipient Name", "Description", "Award Amount", "Start Date", "End Date",
			"Awarding Agency", "Awarding Sub Agency", "Contract Award Type", "NAICS", "PSC",
		}
	}
	if c.ServicesOnly == nil {
		yes := true
		c.ServicesOnly = &yes
	}
	if c.ExcludePSCPrefixes == nil {
		c.ExcludePSCPrefixes = []string{"Y", "Z"}
	}
	if c.LookbackDays <= 0 {
		c.LookbackDays = 30
	}
	if c.WindowDays <= 0 {
		c.WindowDays = 1
	}
	if c.MaxWindowsPerRun <= 0 {
		c.MaxWindowsPerRun = 14
	}
	if c.PageSize <= 0 || c.PageSize > 100 {
		c.PageSize = 100
	}
	if c.MaxPagesPerWindow <= 0 {
		c.MaxPagesPerWindow = 100
	}
}

// Cursor records the first day not yet collected.
type Cursor struct {
	NextStart string `json:"next_start"`
}

// Connector implements connectors.Connector.
type Connector struct {
	cfg Config
}

const day = "2006-01-02"

// Fetch collects complete days from the cursor up to yesterday, window by
// window. The cursor advances only past fully collected windows.
func (c *Connector) Fetch(ctx context.Context, env connectors.Env, raw json.RawMessage, emit connectors.Emit) (json.RawMessage, error) {
	now := time.Now
	if env.Now != nil {
		now = env.Now
	}
	today := now().UTC().Truncate(24 * time.Hour)

	start, err := c.startDate(raw, today)
	if err != nil {
		return nil, err
	}
	cursor := func(t time.Time) json.RawMessage {
		b, _ := json.Marshal(Cursor{NextStart: t.Format(day)})
		return b
	}

	for w := 0; w < c.cfg.MaxWindowsPerRun; w++ {
		end := start.AddDate(0, 0, c.cfg.WindowDays-1)
		if !end.Before(today) { // only complete days
			break
		}
		if err := c.fetchWindow(ctx, env, start, end, emit); err != nil {
			return cursor(start), fmt.Errorf("window %s..%s: %w", start.Format(day), end.Format(day), err)
		}
		start = end.AddDate(0, 0, 1)
	}
	return cursor(start), nil
}

func (c *Connector) startDate(raw json.RawMessage, today time.Time) (time.Time, error) {
	if len(raw) > 0 && string(raw) != "null" {
		var cur Cursor
		if err := json.Unmarshal(raw, &cur); err != nil {
			return time.Time{}, fmt.Errorf("cursor: %w", err)
		}
		if cur.NextStart != "" {
			return time.Parse(day, cur.NextStart)
		}
	}
	if c.cfg.StartDate != "" {
		return time.Parse(day, c.cfg.StartDate)
	}
	return today.AddDate(0, 0, -c.cfg.LookbackDays), nil
}

type searchResponse struct {
	Results      []map[string]any `json:"results"`
	PageMetadata struct {
		HasNext bool `json:"hasNext"`
	} `json:"page_metadata"`
}

// ErrWindowTooLarge means a window had more pages than allowed; reduce
// window_days or narrow the filters, or it would be silently truncated.
var ErrWindowTooLarge = errors.New("more results than max_pages_per_window allows")

func (c *Connector) fetchWindow(ctx context.Context, env connectors.Env, start, end time.Time, emit connectors.Emit) error {
	filters := map[string]any{
		"award_type_codes": c.cfg.AwardTypeCodes,
		"time_period":      []map[string]string{{"start_date": start.Format(day), "end_date": end.Format(day)}},
		"award_amounts":    []map[string]float64{{"lower_bound": c.cfg.MinAmount, "upper_bound": c.cfg.MaxAmount}},
	}
	if len(c.cfg.Keywords) > 0 {
		filters["keywords"] = c.cfg.Keywords
	}
	maps.Copy(filters, c.cfg.Filters)

	for page := 1; ; page++ {
		if page > c.cfg.MaxPagesPerWindow {
			return ErrWindowTooLarge
		}
		body, _ := json.Marshal(map[string]any{
			"filters":   filters,
			"fields":    c.cfg.Fields,
			"page":      page,
			"limit":     c.cfg.PageSize,
			"sort":      "Award Amount",
			"order":     "desc",
			"subawards": false,
		})
		var resp searchResponse
		if err := c.post(ctx, env, "/api/v2/search/spending_by_award/", body, &resp); err != nil {
			return fmt.Errorf("page %d: %w", page, err)
		}
		for _, row := range resp.Results {
			if !c.keep(row) {
				continue
			}
			doc, err := toDocument(row)
			if err != nil {
				return err
			}
			if err := emit(doc); err != nil {
				return err
			}
		}
		if !resp.PageMetadata.HasNext || len(resp.Results) == 0 {
			return nil
		}
	}
}

// keep applies the services-only and PSC-prefix filters.
func (c *Connector) keep(row map[string]any) bool {
	code := pscCode(row)
	if code == "" {
		return !*c.cfg.ServicesOnly
	}
	if *c.cfg.ServicesOnly && code[0] >= '0' && code[0] <= '9' {
		return false
	}
	for _, p := range c.cfg.ExcludePSCPrefixes {
		if p != "" && strings.HasPrefix(strings.ToUpper(code), strings.ToUpper(p)) {
			return false
		}
	}
	return true
}

// pscCode reads the Product and Service Code, which the API returns as an
// object {code, description} (or, in older responses, a plain string).
func pscCode(row map[string]any) string {
	switch v := row["PSC"].(type) {
	case map[string]any:
		s, _ := v["code"].(string)
		return strings.TrimSpace(s)
	case string:
		return strings.TrimSpace(v)
	}
	return ""
}

func (c *Connector) post(ctx context.Context, env connectors.Env, path string, body []byte, out any) error {
	for attempt := 0; ; attempt++ {
		if env.Limiter != nil {
			if err := env.Limiter.Wait(ctx); err != nil {
				return err
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+path, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		if env.UserAgent != "" {
			req.Header.Set("User-Agent", env.UserAgent)
		}
		resp, err := env.HTTP.Do(req)
		if err != nil {
			return err
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if err != nil {
			return err
		}
		// Retry throttling and transient server errors a few times in-run.
		if (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500) && attempt < 3 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt+1) * 5 * time.Second):
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("HTTP %d: %.300s", resp.StatusCode, data)
		}
		return json.Unmarshal(data, out)
	}
}

// toDocument turns one award row into a snapshot. encoding/json sorts map
// keys, so identical rows always produce identical bytes (and hashes). HTML
// escaping is off so stored text matches the source verbatim ("&", not
// "\u0026"), which span checks rely on.
func toDocument(row map[string]any) (store.Document, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(row); err != nil {
		return store.Document{}, err
	}
	content := bytes.TrimRight(buf.Bytes(), "\n")
	awardID, _ := row["Award ID"].(string)
	gid, _ := row["generated_internal_id"].(string)
	key := gid
	if key == "" {
		key = awardID
	}
	if key == "" {
		return store.Document{}, errors.New("award row has neither generated_internal_id nor Award ID")
	}
	doc := store.Document{
		CanonicalURL: "https://www.usaspending.gov/award/" + key,
		ExternalID:   awardID,
		MIME:         "application/json",
		Content:      content,
	}
	// The API filters by action date; the row carries the period-of-performance
	// start, which is the closest date it returns.
	if s, ok := row["Start Date"].(string); ok {
		if t, err := time.Parse(day, s); err == nil {
			doc.PublishedAt = &t
		}
	}
	// The buying organisation is the independent origin. The department
	// ("Department of Justice") is too coarse: its bureaus buy independently.
	if org, ok := row["Awarding Sub Agency"].(string); ok && org != "" {
		doc.OrgHint = org
	} else if org, ok := row["Awarding Agency"].(string); ok {
		doc.OrgHint = org
	}
	return doc, nil
}
