// Package connectors defines the source-connector contract (SRC-1) and a
// registry of implementations. Connectors are deterministic code: they fetch
// and emit documents, and never call a model.
package connectors

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/kwant-dbg/auto/internal/store"
)

// Env is what a connector receives for one run.
type Env struct {
	HTTP    *http.Client
	Limiter *Limiter
	// UserAgent identifies AOE to the source.
	UserAgent string
	// Now is injectable for tests.
	Now func() time.Time
}

// Emit stores one document. It returns an error if the run should stop.
type Emit func(store.Document) error

// Connector fetches new documents since cursor.
//
// Fetch should return the cursor of the last fully processed unit of work
// even when it also returns an error, so a failed run keeps its progress.
type Connector interface {
	Fetch(ctx context.Context, env Env, cursor json.RawMessage, emit Emit) (json.RawMessage, error)
}

// Factory builds a connector from its source config.
type Factory func(config json.RawMessage) (Connector, error)

var (
	mu       sync.RWMutex
	registry = map[string]Factory{}
)

// Register adds a connector type. It panics on duplicates.
func Register(sourceType string, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := registry[sourceType]; dup {
		panic("connectors: duplicate type " + sourceType)
	}
	registry[sourceType] = f
}

// New builds the connector for a source type.
func New(sourceType string, config json.RawMessage) (Connector, error) {
	mu.RLock()
	f, ok := registry[sourceType]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("connectors: no connector registered for type %q", sourceType)
	}
	return f(config)
}

// Types lists registered connector types.
func Types() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(registry))
	for t := range registry {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// Limiter spaces requests to respect a per-minute rate limit (SRC-3).
type Limiter struct {
	mu       sync.Mutex
	interval time.Duration
	next     time.Time
	count    int
}

// NewLimiter allows perMinute requests per minute.
func NewLimiter(perMinute int) *Limiter {
	if perMinute <= 0 {
		perMinute = 60
	}
	return &Limiter{interval: time.Minute / time.Duration(perMinute)}
}

// Wait blocks until the next request is allowed.
func (l *Limiter) Wait(ctx context.Context) error {
	l.mu.Lock()
	now := time.Now()
	at := l.next
	if at.Before(now) {
		at = now
	}
	l.next = at.Add(l.interval)
	l.count++
	l.mu.Unlock()

	if d := time.Until(at); d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
	return nil
}

// Count is the number of requests made through the limiter.
func (l *Limiter) Count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.count
}
