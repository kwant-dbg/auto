package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// TOSEntry is a ToS-register row (SRC-2).
type TOSEntry struct {
	SourceType      string `yaml:"source_type"`
	AccessMethod    string `yaml:"access_method"`
	TermsURL        string `yaml:"terms_url"`
	ReviewedAt      string `yaml:"reviewed_at"` // YYYY-MM-DD
	AllowedUse      string `yaml:"allowed_use"`
	RateLimitPerMin int    `yaml:"rate_limit_per_min"`
	Notes           string `yaml:"notes"`
}

// SourceSpec is a source as declared in configuration.
type SourceSpec struct {
	Name    string         `yaml:"name"`
	Type    string         `yaml:"type"`
	Enabled bool           `yaml:"enabled"`
	Cadence time.Duration  `yaml:"cadence"`
	Config  map[string]any `yaml:"config"`
}

// Source is a source row joined with its rate limit.
type Source struct {
	ID                   int64
	Name                 string
	Type                 string
	Config               json.RawMessage
	Cadence              time.Duration
	BackoffFactor        int
	Cursor               json.RawMessage
	Enabled              bool
	NextRunAt            time.Time
	LastSuccessAt        *time.Time
	LastNewDocumentAt    *time.Time
	ConsecutiveEmptyRuns int
	ConsecutiveErrors    int
	LastError            *string
	RateLimitPerMin      int
}

// MaxBackoffFactor caps how far an unproductive source's cadence stretches.
const MaxBackoffFactor = 8

// EmptyRunsBeforeBackoff is how many consecutive runs without new documents
// double the cadence (docs/04 §5 rule 7).
const EmptyRunsBeforeBackoff = 3

// SyncTOS inserts or updates ToS-register entries.
func (s *Store) SyncTOS(ctx context.Context, entries []TOSEntry) error {
	for _, e := range entries {
		reviewed, err := time.Parse("2006-01-02", e.ReviewedAt)
		if err != nil {
			return fmt.Errorf("tos %s: reviewed_at: %w", e.SourceType, err)
		}
		_, err = s.DB.Exec(ctx, `
			INSERT INTO tos_entry (source_type, access_method, terms_url, reviewed_at, allowed_use, rate_limit_per_min, notes)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (source_type) DO UPDATE SET
				access_method = EXCLUDED.access_method, terms_url = EXCLUDED.terms_url,
				reviewed_at = EXCLUDED.reviewed_at, allowed_use = EXCLUDED.allowed_use,
				rate_limit_per_min = EXCLUDED.rate_limit_per_min, notes = EXCLUDED.notes`,
			e.SourceType, e.AccessMethod, e.TermsURL, reviewed, e.AllowedUse, e.RateLimitPerMin, e.Notes)
		if err != nil {
			return fmt.Errorf("tos %s: %w", e.SourceType, err)
		}
	}
	return nil
}

// SyncSources inserts or updates sources from configuration, linking each to
// the ToS entry for its type. Enabling a source without one fails (SRC-2).
// Cursor and health state are preserved.
func (s *Store) SyncSources(ctx context.Context, specs []SourceSpec) error {
	for _, sp := range specs {
		cfg, err := json.Marshal(sp.Config)
		if err != nil {
			return err
		}
		if sp.Config == nil {
			cfg = []byte("{}")
		}
		cadence := sp.Cadence
		if cadence <= 0 {
			cadence = 24 * time.Hour
		}
		_, err = s.DB.Exec(ctx, `
			INSERT INTO source (name, type, config, cadence, enabled, tos_entry_id)
			VALUES ($1, $2, $3, make_interval(secs => $4), $5,
			        (SELECT id FROM tos_entry WHERE source_type = $2))
			ON CONFLICT (name) DO UPDATE SET
				type = EXCLUDED.type, config = EXCLUDED.config, cadence = EXCLUDED.cadence,
				enabled = EXCLUDED.enabled, tos_entry_id = EXCLUDED.tos_entry_id`,
			sp.Name, sp.Type, cfg, cadence.Seconds(), sp.Enabled)
		if err != nil {
			return fmt.Errorf("source %s: %w (an enabled source needs a ToS entry for type %q)", sp.Name, err, sp.Type)
		}
	}
	return nil
}

const sourceColumns = `
	s.id, s.name, s.type, s.config, extract(epoch FROM s.cadence)::bigint, s.backoff_factor, s.cursor,
	s.enabled, s.next_run_at, s.last_success_at, s.last_new_document_at, s.consecutive_empty_runs,
	s.consecutive_errors, s.last_error, coalesce(t.rate_limit_per_min, 60)`

func scanSource(row pgx.Row) (Source, error) {
	var src Source
	var cadenceSecs int64
	err := row.Scan(&src.ID, &src.Name, &src.Type, &src.Config, &cadenceSecs, &src.BackoffFactor, &src.Cursor,
		&src.Enabled, &src.NextRunAt, &src.LastSuccessAt, &src.LastNewDocumentAt, &src.ConsecutiveEmptyRuns,
		&src.ConsecutiveErrors, &src.LastError, &src.RateLimitPerMin)
	src.Cadence = time.Duration(cadenceSecs) * time.Second
	return src, err
}

// ListSources returns every source, ordered by name.
func (s *Store) ListSources(ctx context.Context) ([]Source, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+sourceColumns+`
		FROM source s LEFT JOIN tos_entry t ON t.id = s.tos_entry_id ORDER BY s.name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Source, error) { return scanSource(r) })
}

// DueSources returns enabled sources whose next run time has passed.
func (s *Store) DueSources(ctx context.Context, now time.Time) ([]Source, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+sourceColumns+`
		FROM source s JOIN tos_entry t ON t.id = s.tos_entry_id
		WHERE s.enabled AND s.next_run_at <= $1 ORDER BY s.next_run_at`, now)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Source, error) { return scanSource(r) })
}

// GetSource loads one source by name.
func (s *Store) GetSource(ctx context.Context, name string) (Source, error) {
	src, err := scanSource(s.DB.QueryRow(ctx, `SELECT `+sourceColumns+`
		FROM source s LEFT JOIN tos_entry t ON t.id = s.tos_entry_id WHERE s.name = $1`, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return src, fmt.Errorf("source %q not found", name)
	}
	return src, err
}

// RunOutcome is the result of one collection run.
type RunOutcome struct {
	NewDocuments int
	Cursor       json.RawMessage // nil leaves the stored cursor unchanged
	Err          error
}

// RecordRun updates a source's cursor, health and next run time.
//
// Success with new documents resets the back-off. Every EmptyRunsBeforeBackoff
// consecutive empty runs double the cadence, up to MaxBackoffFactor. Errors
// retry sooner than the cadence (15 min × 2^errors, capped at the cadence),
// so a transient failure doesn't wait a full cycle.
func (s *Store) RecordRun(ctx context.Context, sourceID int64, out RunOutcome) error {
	var cursor any
	if out.Cursor != nil {
		cursor = out.Cursor
	}
	if out.Err != nil {
		_, err := s.DB.Exec(ctx, `
			UPDATE source SET
				last_run_at = now(),
				consecutive_errors = consecutive_errors + 1,
				last_error = $2,
				cursor = coalesce($3, cursor),
				next_run_at = now() + least(cadence, interval '15 minutes' * power(2, consecutive_errors))
			WHERE id = $1`, sourceID, out.Err.Error(), cursor)
		return err
	}
	_, err := s.DB.Exec(ctx, `
		WITH next AS (
			SELECT id,
			       CASE WHEN $2 > 0 THEN 0 ELSE consecutive_empty_runs + 1 END AS empty_runs,
			       CASE
			           WHEN $2 > 0 THEN 1
			           WHEN (consecutive_empty_runs + 1) % $4 = 0 THEN least(backoff_factor * 2, $5)
			           ELSE backoff_factor
			       END AS backoff
			FROM source WHERE id = $1
		)
		UPDATE source s SET
			last_run_at = now(),
			last_success_at = now(),
			last_new_document_at = CASE WHEN $2 > 0 THEN now() ELSE s.last_new_document_at END,
			consecutive_errors = 0,
			last_error = NULL,
			consecutive_empty_runs = next.empty_runs,
			backoff_factor = next.backoff,
			cursor = coalesce($3, s.cursor),
			next_run_at = now() + s.cadence * next.backoff
		FROM next WHERE s.id = next.id`,
		sourceID, out.NewDocuments, cursor, EmptyRunsBeforeBackoff, MaxBackoffFactor)
	return err
}
