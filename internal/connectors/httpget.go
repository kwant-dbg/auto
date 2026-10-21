package connectors

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// MaxResponseBytes caps any single response a connector reads.
const MaxResponseBytes = 32 << 20

// Get fetches a URL through the rate limiter, retrying throttling and
// transient server errors a few times within the run.
func Get(ctx context.Context, env Env, url string) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		if env.Limiter != nil {
			if err := env.Limiter.Wait(ctx); err != nil {
				return nil, err
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		if env.UserAgent != "" {
			req.Header.Set("User-Agent", env.UserAgent)
		}
		resp, err := env.HTTP.Do(req)
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes))
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500) && attempt < 3 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt+1) * 5 * time.Second):
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GET %s: HTTP %d: %.300s", url, resp.StatusCode, body)
		}
		return body, nil
	}
}

// GetJSON fetches a URL and decodes its JSON body into out.
func GetJSON(ctx context.Context, env Env, url string, out any) error {
	body, err := Get(ctx, env, url)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("GET %s: decode: %w", url, err)
	}
	return nil
}

// MarshalVerbatim encodes v as indented JSON without HTML escaping, so stored
// text matches the source ("&", not "&"). Map keys are sorted, so equal
// values always produce equal bytes.
func MarshalVerbatim(v any) ([]byte, error) {
	var buf jsonBuffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	b := buf.b
	for len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	return b, nil
}

type jsonBuffer struct{ b []byte }

func (j *jsonBuffer) Write(p []byte) (int, error) { j.b = append(j.b, p...); return len(p), nil }
