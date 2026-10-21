// Package collect runs one source's connector and snapshots what it emits.
package collect

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/kwant-dbg/auto/internal/connectors"
	"github.com/kwant-dbg/auto/internal/store"
)

// Stats summarises a run.
type Stats struct {
	Source    string `json:"source"`
	Emitted   int    `json:"emitted"`
	New       int    `json:"new"`
	Changed   int    `json:"changed"`
	Requests  int    `json:"requests"`
	Metrics   int    `json:"metrics"`
	Error     string `json:"error,omitempty"`
	ElapsedMS int64  `json:"elapsed_ms"`
}

// Options configure outbound HTTP for connectors.
type Options struct {
	HTTP      *http.Client
	UserAgent string
}

// ErrAlreadyRunning means another collection of the same source holds its lock.
var ErrAlreadyRunning = errors.New("collect: source is already being collected")

// lockNamespace scopes collection advisory locks (key = namespace, source id).
const lockNamespace = 7_004_134

// Run collects one source: build the connector, fetch from the stored cursor,
// save each document, then record cursor, health and the next run time.
// Progress is kept even when the connector fails part-way. A per-source
// advisory lock stops two runs of the same source from overlapping.
func Run(ctx context.Context, st *store.Store, src store.Source, opt Options) (Stats, error) {
	start := time.Now()
	stats := Stats{Source: src.Name}

	lockConn, err := st.DB.Acquire(ctx)
	if err != nil {
		return stats, err
	}
	defer lockConn.Release()
	var locked bool
	if err := lockConn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1, $2)`, lockNamespace, int32(src.ID)).Scan(&locked); err != nil {
		return stats, err
	}
	if !locked {
		return stats, ErrAlreadyRunning
	}
	defer lockConn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1, $2)`, lockNamespace, int32(src.ID)) //nolint:errcheck

	// A cancelled run (e.g. worker shutdown) must not reset the cursor, so
	// reload the source under the lock to pick up the latest one.
	if fresh, err := st.GetSource(ctx, src.Name); err == nil {
		src = fresh
	}

	conn, err := connectors.New(src.Type, src.Config)
	if err != nil {
		stats.Error = err.Error()
		return stats, st.RecordRun(ctx, src.ID, store.RunOutcome{Err: err})
	}
	httpClient := opt.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	limiter := connectors.NewLimiter(src.RateLimitPerMin)
	env := connectors.Env{HTTP: httpClient, Limiter: limiter, UserAgent: opt.UserAgent,
		Metric: func(subject, name string, value float64) error {
			stats.Metrics++
			return st.RecordMetric(ctx, src.ID, subject, name, value)
		}}

	cursor, fetchErr := conn.Fetch(ctx, env, src.Cursor, func(d store.Document) error {
		stats.Emitted++
		res, err := st.SaveDocument(ctx, src.ID, d)
		if err != nil {
			return err
		}
		if res.New {
			stats.New++
		}
		if res.Changed {
			stats.Changed++
		}
		return nil
	})
	stats.Requests = limiter.Count()
	stats.ElapsedMS = time.Since(start).Milliseconds()
	if fetchErr != nil {
		stats.Error = fetchErr.Error()
	}
	// Record with a context that survives cancellation so progress isn't lost.
	recErr := st.RecordRun(context.WithoutCancel(ctx), src.ID, store.RunOutcome{
		NewDocuments: stats.New, Cursor: cursor, Err: fetchErr,
	})
	if fetchErr != nil {
		return stats, fetchErr
	}
	return stats, recErr
}
