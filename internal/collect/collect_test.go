package collect_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kwant-dbg/auto/internal/collect"
	"github.com/kwant-dbg/auto/internal/connectors"
	"github.com/kwant-dbg/auto/internal/store"
	"github.com/kwant-dbg/auto/internal/testdb"
)

type fakeConn struct{}

func (fakeConn) Fetch(ctx context.Context, env connectors.Env, cursor json.RawMessage, emit connectors.Emit) (json.RawMessage, error) {
	for i := 0; i < 3; i++ {
		if err := emit(store.Document{CanonicalURL: fmt.Sprint("https://example.test/", i), MIME: "text/plain", Content: []byte(fmt.Sprint("doc ", i))}); err != nil {
			return cursor, err
		}
	}
	return json.RawMessage(`{"done":true}`), nil
}

func init() {
	connectors.Register("fake_collect", func(json.RawMessage) (connectors.Connector, error) { return fakeConn{}, nil })
}

// More concurrent collections than pgx's default pool size (4) must not
// deadlock: each run holds one connection for its advisory lock.
func TestConcurrentCollectionsDoNotExhaustPool(t *testing.T) {
	st := testdb.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := st.SyncTOS(ctx, []store.TOSEntry{{SourceType: "fake_collect", AccessMethod: "a", TermsURL: "u", ReviewedAt: "2026-10-22", AllowedUse: "x", RateLimitPerMin: 6000}}); err != nil {
		t.Fatal(err)
	}
	const n = 8
	var specs []store.SourceSpec
	for i := 0; i < n; i++ {
		specs = append(specs, store.SourceSpec{Name: fmt.Sprint("s", i), Type: "fake_collect", Enabled: true})
	}
	if err := st.SyncSources(ctx, specs); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		src, err := st.GetSource(ctx, fmt.Sprint("s", i))
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			stats, err := collect.Run(ctx, st, src, collect.Options{})
			if err == nil && stats.New != 3 {
				err = fmt.Errorf("%s: %d new documents", src.Name, stats.New)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}
