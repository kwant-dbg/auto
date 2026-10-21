package tools

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ErrBlockedAddress means a URL resolved to a private, loopback or otherwise
// internal address. Agents read untrusted text, so fetches must never reach
// internal services or cloud metadata endpoints (docs/04 §8).
var ErrBlockedAddress = errors.New("blocked: destination is not a public internet address")

// blockedIP reports whether ip is internal.
func blockedIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() ||
		// Carrier-grade NAT and other shared/reserved ranges.
		cgnat.Contains(ip) || benchmark.Contains(ip)
}

var (
	_, cgnat, _     = net.ParseCIDR("100.64.0.0/10")
	_, benchmark, _ = net.ParseCIDR("198.18.0.0/15")
)

// SafeClient returns an HTTP client that refuses internal destinations at
// connect time (so DNS rebinding and redirects can't get around the check),
// limits redirects and times out.
//
// If the environment configures an egress proxy, requests go through it: the
// dialer then only ever connects to the proxy, whose own address is allowed,
// and preflight (below) checks the target's DNS before each request.
func SafeClient(timeout time.Duration, allowInternal bool) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = http.ProxyFromEnvironment
	if !allowInternal {
		proxies := proxyAddrs()
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			if proxies[address] {
				return nil
			}
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(host); ip == nil || blockedIP(ip) {
				return ErrBlockedAddress
			}
			return nil
		}
	}
	tr.DialContext = dialer.DialContext
	return &http.Client{
		Timeout:   timeout,
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if _, err := checkURL(req.URL.String(), allowInternal); err != nil {
				return err
			}
			return preflight(req.Context(), req.URL.Hostname(), allowInternal)
		},
	}
}

// proxyAddrs resolves the configured proxies to the ip:port pairs the dialer
// will see.
func proxyAddrs() map[string]bool {
	out := map[string]bool{}
	for _, scheme := range []string{"https", "http"} {
		p, err := http.ProxyFromEnvironment(&http.Request{URL: &url.URL{Scheme: scheme, Host: "example.com"}})
		if err != nil || p == nil {
			continue
		}
		port := p.Port()
		if port == "" {
			port = map[string]string{"https": "443", "http": "80"}[p.Scheme]
		}
		ips, err := net.LookupIP(p.Hostname())
		if err != nil {
			continue
		}
		for _, ip := range ips {
			out[net.JoinHostPort(ip.String(), port)] = true
		}
	}
	return out
}

// preflight rejects hostnames that resolve to internal addresses. If the name
// doesn't resolve locally (e.g. only the egress proxy can resolve it), the
// request proceeds and the proxy's own policy applies.
func preflight(ctx context.Context, host string, allowInternal bool) error {
	if allowInternal {
		return nil
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil
	}
	for _, ip := range ips {
		if blockedIP(ip) {
			return ErrBlockedAddress
		}
	}
	return nil
}

// checkURL validates a URL before fetching. With a proxy the dialer only sees
// the proxy, so literal internal hosts are rejected here as well.
func checkURL(raw string, allowInternal bool) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("only http and https URLs can be fetched")
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("URL has no host")
	}
	if !allowInternal {
		h := strings.ToLower(u.Hostname())
		if h == "localhost" || strings.HasSuffix(h, ".localhost") || strings.HasSuffix(h, ".internal") || strings.HasSuffix(h, ".local") {
			return nil, ErrBlockedAddress
		}
		if ip := net.ParseIP(h); ip != nil && blockedIP(ip) {
			return nil, ErrBlockedAddress
		}
	}
	u.Fragment = ""
	return u, nil
}

// robots caches robots.txt rules per host.
type robots struct {
	mu      sync.Mutex
	agent   string
	entries map[string]robotsEntry
}

type robotsEntry struct {
	rules   []robotsRule
	fetched time.Time
	denyAll bool
}

type robotsRule struct {
	allow bool
	path  string
}

// allowed reports whether agent may fetch u, fetching robots.txt if needed.
// A missing robots.txt (4xx) allows everything; a server error disallows.
func (r *robots) allowed(ctx context.Context, client *http.Client, u *url.URL) (bool, error) {
	key := u.Scheme + "://" + u.Host
	r.mu.Lock()
	e, ok := r.entries[key]
	r.mu.Unlock()
	if !ok || time.Since(e.fetched) > time.Hour {
		e = r.fetch(ctx, client, key)
		r.mu.Lock()
		if r.entries == nil {
			r.entries = map[string]robotsEntry{}
		}
		r.entries[key] = e
		r.mu.Unlock()
	}
	if e.denyAll {
		return false, nil
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	best, allow := -1, true
	for _, rule := range e.rules {
		if rule.path == "" || !strings.HasPrefix(path, rule.path) {
			continue
		}
		if len(rule.path) > best || (len(rule.path) == best && rule.allow) {
			best, allow = len(rule.path), rule.allow
		}
	}
	return allow, nil
}

func (r *robots) fetch(ctx context.Context, client *http.Client, origin string) robotsEntry {
	e := robotsEntry{fetched: time.Now()}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/robots.txt", nil)
	if err != nil {
		return e
	}
	req.Header.Set("User-Agent", r.agent)
	resp, err := client.Do(req)
	if err != nil {
		e.denyAll = true
		return e
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode >= 500:
		e.denyAll = true
		return e
	case resp.StatusCode != http.StatusOK:
		return e
	}
	e.rules = parseRobots(io.LimitReader(resp.Body, 512<<10), r.agent)
	return e
}

// parseRobots returns the rules of the most specific group for agent: a group
// naming the agent's product token wins over "*".
func parseRobots(rd io.Reader, agent string) []robotsRule {
	token := strings.ToLower(strings.SplitN(strings.Fields(agent + " x")[0], "/", 2)[0])
	groups := map[string][]robotsRule{}
	var current []string
	inRules := false
	sc := bufio.NewScanner(rd)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
		switch k {
		case "user-agent":
			if inRules {
				current, inRules = nil, false
			}
			current = append(current, strings.ToLower(v))
		case "allow", "disallow":
			inRules = true
			for _, ua := range current {
				groups[ua] = append(groups[ua], robotsRule{allow: k == "allow", path: v})
			}
		}
	}
	if rules, ok := groups[token]; ok {
		return rules
	}
	return groups["*"]
}

// hostThrottle spaces requests to the same host.
type hostThrottle struct {
	mu   sync.Mutex
	gap  time.Duration
	next map[string]time.Time
}

func (h *hostThrottle) wait(ctx context.Context, host string) error {
	h.mu.Lock()
	if h.next == nil {
		h.next = map[string]time.Time{}
	}
	at := h.next[host]
	now := time.Now()
	if at.Before(now) {
		at = now
	}
	h.next[host] = at.Add(h.gap)
	h.mu.Unlock()
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
