// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

package wrap

import (
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// RateLimiter is a per-key token-bucket rate limiter used to protect
// vulos-meet's hot endpoints (/rtc token validation, /admin/*) from
// high-frequency abuse.
//
// Token-bucket semantics:
//   - Each unique key (typically a client IP) has its own bucket.
//   - The bucket starts full (burst depth) so the first requests in a session
//     are never throttled.
//   - New tokens flow in at `rate` tokens/second.
//   - An Allow call consumes one token and returns true, or returns false and
//     leaves the bucket unchanged when it is empty.
//
// Memory hygiene: idle buckets are evicted after idleTTL via GCOnce, which
// the caller is expected to invoke periodically (e.g. from a background ticker
// or lazily before each Allow call). Without eviction a high-cardinality IP
// set would grow without bound.
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*tokenBucket
	rate    float64       // tokens added per second (steady-state throughput)
	burst   float64       // maximum bucket depth (short-burst headroom)
	idleTTL time.Duration // buckets idle for this long are evicted by GCOnce
	now     func() time.Time
}

type tokenBucket struct {
	tokens float64
	last   time.Time
}

// NewRateLimiter constructs a per-key token-bucket rate limiter.
//
//   - rate is the steady-state token refill speed in tokens/second.
//   - burst is the bucket depth; a burst of 20 allows 20 back-to-back requests
//     before steady-state throttling kicks in.
//   - idleTTL is how long an idle bucket is kept before GCOnce evicts it.
//     10*time.Minute is a reasonable default for per-IP limiting.
func NewRateLimiter(rate, burst float64, idleTTL time.Duration) *RateLimiter {
	return newRateLimiterWithClock(rate, burst, idleTTL, time.Now)
}

// newRateLimiterWithClock is the testable constructor that accepts a clock.
func newRateLimiterWithClock(rate, burst float64, idleTTL time.Duration, now func() time.Time) *RateLimiter {
	if now == nil {
		now = time.Now
	}
	return &RateLimiter{
		buckets: make(map[string]*tokenBucket),
		rate:    rate,
		burst:   burst,
		idleTTL: idleTTL,
		now:     now,
	}
}

// Allow returns true if the key is within the rate limit and consumes one
// token, or returns false (without consuming a token) when the bucket is
// empty. Thread-safe.
func (r *RateLimiter) Allow(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()
	b, ok := r.buckets[key]
	if !ok {
		// New key: start at full burst so the first burst of requests goes
		// through without delay (common pattern for a reconnecting browser).
		b = &tokenBucket{tokens: r.burst, last: now}
		r.buckets[key] = b
	} else {
		// Refill proportional to elapsed time, capped at burst.
		elapsed := now.Sub(b.last).Seconds()
		if elapsed > 0 {
			b.tokens += elapsed * r.rate
			if b.tokens > r.burst {
				b.tokens = r.burst
			}
			b.last = now
		}
	}

	if b.tokens < 1 {
		return false // bucket empty — throttled
	}
	b.tokens--
	return true
}

// GCOnce evicts buckets that have been idle for longer than idleTTL. If
// idleTTL is zero, GCOnce is a no-op. Call periodically from a background
// goroutine or lazily; the lock is held only for the iteration.
func (r *RateLimiter) GCOnce() {
	if r.idleTTL == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	for key, b := range r.buckets {
		if now.Sub(b.last) > r.idleTTL {
			delete(r.buckets, key)
		}
	}
}

// BucketCount returns the number of currently-tracked key buckets. Intended
// for tests and metrics; callers must not rely on the value being stable.
func (r *RateLimiter) BucketCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.buckets)
}

// TrustedProxyEnv is the environment variable that enables X-Forwarded-For
// trust in the rate-limiter IP extractor. When set to "1", "true", or "yes"
// (case-insensitive) the limiter keys on the leftmost XFF entry (the
// client IP as reported by the upstream load balancer). When unset or any
// other value, the limiter keys on RemoteAddr (the TCP peer address) to
// prevent rate-limit bypass on a directly-exposed box where XFF can be
// spoofed by clients. Default: off (safe for direct-internet exposure).
const TrustedProxyEnv = "MEET_TRUSTED_PROXY"

// Middleware returns an http.Handler that applies the rate limiter keyed on
// the client IP on every request. Throttled requests receive HTTP 429 with no
// body detail (the exact rate parameters are internal).
//
// Client IP extraction honours TrustedProxyEnv: when off (default), the key
// is the TCP RemoteAddr, which cannot be spoofed. When on (set
// MEET_TRUSTED_PROXY=1), the leftmost X-Forwarded-For entry is preferred so
// the limiter sees the real client IP from behind a trusted load balancer.
func (r *RateLimiter) Middleware(next http.Handler) http.Handler {
	return r.MiddlewareWithTrust(next, isTrustedProxy())
}

// MiddlewareWithTrust is the testable variant — it accepts the XFF-trust flag
// directly rather than reading the environment, so tests can exercise both
// paths deterministically.
func (r *RateLimiter) MiddlewareWithTrust(next http.Handler, trustXFF bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !r.Allow(clientIP(req, trustXFF)) {
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, req)
	})
}

// isTrustedProxy returns true when MEET_TRUSTED_PROXY is set to a truthy value.
func isTrustedProxy() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(TrustedProxyEnv))) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

// clientIP returns the best-available client IP string for use as a rate-
// limiter key. When trustXFF is true, it prefers the first entry in
// X-Forwarded-For (trusting the upstream proxy chain); when false, it uses
// the raw TCP remote address only — safe on boxes without a trusted proxy in
// front, where XFF headers can be injected by clients to rotate the bucket key.
func clientIP(r *http.Request, trustXFF bool) string {
	if trustXFF {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// May be comma-separated (proxy chain); take the leftmost (client) entry.
			if i := strings.IndexByte(xff, ','); i > 0 {
				xff = strings.TrimSpace(xff[:i])
			} else {
				xff = strings.TrimSpace(xff)
			}
			if ip := net.ParseIP(xff); ip != nil {
				return ip.String()
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr // malformed RemoteAddr — use as-is
	}
	return host
}
