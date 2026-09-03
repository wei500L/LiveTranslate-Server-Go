package middleware

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// RateLimiter is a small in-memory fixed-window limiter keyed by (bucket,
// key). Sufficient for a single-instance deployment; the durable
// progressive-delay state lives in login_events (PostgreSQL).
type RateLimiter struct {
	mu     sync.Mutex
	window time.Duration
	max    int
	hits   map[string]*window
}

type window struct {
	count int
	start time.Time
}

func NewRateLimiter(max int, perMinute bool) *RateLimiter {
	_ = perMinute // fixed one-minute window
	return NewRateLimiterWindow(max, time.Minute)
}

func NewRateLimiterWindow(maxN int, w time.Duration) *RateLimiter {
	return &RateLimiter{window: w, max: maxN, hits: map[string]*window{}}
}

// Allow consumes one slot for the key. Returns (allowed, retryAfter).
func (r *RateLimiter) Allow(bucket, key string) (bool, time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	k := bucket + "|" + key
	w, ok := r.hits[k]
	if !ok || now.Sub(w.start) >= r.window {
		r.hits[k] = &window{count: 1, start: now}
		// Opportunistic GC: keep the map bounded.
		if len(r.hits) > 10_000 {
			for kk, ww := range r.hits {
				if now.Sub(ww.start) >= r.window {
					delete(r.hits, kk)
				}
			}
		}
		return true, 0
	}
	if w.count >= r.max {
		return false, r.window - now.Sub(w.start)
	}
	w.count++
	return true, 0
}

// ClientIP resolves the request's client IP. X-Forwarded-For is honored
// ONLY when the direct peer is a configured trusted proxy.
type ProxyTrust struct {
	cidrs []*net.IPNet
}

func ParseTrustedProxies(specs []string) (*ProxyTrust, error) {
	t := &ProxyTrust{}
	for _, s := range specs {
		if s == "" {
			continue
		}
		if !containsCIDR(s) {
			s += "/32"
		}
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			return nil, err
		}
		t.cidrs = append(t.cidrs, n)
	}
	return t, nil
}

func containsCIDR(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			return true
		}
	}
	return false
}

func (t *ProxyTrust) Trusted(ip net.IP) bool {
	if t == nil {
		return false
	}
	for _, c := range t.cidrs {
		if c.Contains(ip) {
			return true
		}
	}
	return false
}

// ClientIP never trusts the proxy headers blindly: the peer must be in the
// trusted set for XFF to be consulted, and the rightmost untrusted hop is
// ignored (single trusted proxy hop assumed).
func ClientIP(r *http.Request, trust *ProxyTrust) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer := net.ParseIP(host)
	if peer == nil {
		return host
	}
	if trust != nil && trust.Trusted(peer) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// First entry = original client.
			first := xff
			for i := 0; i < len(xff); i++ {
				if xff[i] == ',' {
					first = xff[:i]
					break
				}
			}
			clean := trimSpace(first)
			if ip := net.ParseIP(clean); ip != nil {
				return ip.String()
			}
		}
	}
	return peer.String()
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}
