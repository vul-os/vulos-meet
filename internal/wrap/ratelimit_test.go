// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

package wrap

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestClock returns a fake-clock function starting at a fixed time,
// along with an advance helper. Used to make rate-limiter token-refill
// deterministic without sleeping.
func newTestClock(start time.Time) (now func() time.Time, advance func(time.Duration)) {
	var mu sync.Mutex
	t := start
	now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return t
	}
	advance = func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		t = t.Add(d)
	}
	return
}

// TestRateLimiter_BurstAllowed verifies that a fresh bucket allows `burst`
// consecutive requests without throttling (all arriving at the same instant).
func TestRateLimiter_BurstAllowed(t *testing.T) {
	const burst = 5
	now, _ := newTestClock(time.Unix(1_000, 0))
	rl := newRateLimiterWithClock(1, burst, time.Minute, now)

	for i := 0; i < burst; i++ {
		if !rl.Allow("client1") {
			t.Fatalf("burst request %d blocked (want allowed)", i+1)
		}
	}
}

// TestRateLimiter_ExhaustedBucketThrottles verifies that once the burst is
// consumed the next request is refused without any time advance.
func TestRateLimiter_ExhaustedBucketThrottles(t *testing.T) {
	const burst = 3
	now, _ := newTestClock(time.Unix(1_000, 0))
	rl := newRateLimiterWithClock(1, burst, time.Minute, now)

	for i := 0; i < burst; i++ {
		rl.Allow("x") // drain
	}
	if rl.Allow("x") {
		t.Fatalf("request past burst should be throttled")
	}
}

// TestRateLimiter_RefillAfterTime verifies that tokens accumulate at the
// configured rate after a delay.
func TestRateLimiter_RefillAfterTime(t *testing.T) {
	const rate = 2.0 // 2 tokens/second
	const burst = 2.0
	now, advance := newTestClock(time.Unix(1_000, 0))
	rl := newRateLimiterWithClock(rate, burst, time.Minute, now)

	// Drain the bucket.
	rl.Allow("ip")
	rl.Allow("ip")
	if rl.Allow("ip") {
		t.Fatalf("should be throttled after draining burst")
	}

	// Advance 1 second → 2 new tokens should arrive (rate=2/s).
	advance(time.Second)

	if !rl.Allow("ip") {
		t.Fatalf("first request after 1s refill should be allowed (rate=2/s)")
	}
	if !rl.Allow("ip") {
		t.Fatalf("second request after 1s refill should be allowed (rate=2/s)")
	}
	// Third request: bucket exhausted again.
	if rl.Allow("ip") {
		t.Fatalf("third request after 1s at rate=2 should be throttled")
	}
}

// TestRateLimiter_BurstCap verifies tokens never exceed burst even after a
// long idle period.
func TestRateLimiter_BurstCap(t *testing.T) {
	const rate = 10.0
	const burst = 3.0
	now, advance := newTestClock(time.Unix(1_000, 0))
	rl := newRateLimiterWithClock(rate, burst, time.Minute, now)

	// Drain once so the bucket is initialised, then wait a very long time.
	rl.Allow("ip")
	advance(1 * time.Hour) // would be 36,000 tokens without the cap

	// Should only be able to fire `burst` requests.
	allowed := 0
	for i := 0; i < int(burst)+5; i++ {
		if rl.Allow("ip") {
			allowed++
		}
	}
	if allowed != int(burst) {
		t.Fatalf("expected burst=%d requests after long idle, got %d", int(burst), allowed)
	}
}

// TestRateLimiter_PerKeyIsolation verifies different keys have independent
// buckets: exhausting one key's bucket does not affect another.
func TestRateLimiter_PerKeyIsolation(t *testing.T) {
	now, _ := newTestClock(time.Unix(1_000, 0))
	rl := newRateLimiterWithClock(1, 2, time.Minute, now)

	// Drain key "a".
	rl.Allow("a")
	rl.Allow("a")
	if rl.Allow("a") {
		t.Fatalf("key 'a' should be throttled after draining")
	}

	// Key "b" is unaffected — its bucket is fresh.
	if !rl.Allow("b") {
		t.Fatalf("key 'b' should be allowed (independent bucket)")
	}
}

// TestRateLimiter_GCEvictsIdleBuckets verifies that GCOnce removes buckets
// that have not been used for longer than idleTTL, releasing memory.
func TestRateLimiter_GCEvictsIdleBuckets(t *testing.T) {
	now, advance := newTestClock(time.Unix(1_000, 0))
	const ttl = 5 * time.Minute
	rl := newRateLimiterWithClock(1, 10, ttl, now)

	rl.Allow("old-client")
	if rl.BucketCount() != 1 {
		t.Fatalf("expected 1 bucket after Allow, got %d", rl.BucketCount())
	}

	// Advance past TTL and GC.
	advance(ttl + time.Second)
	rl.GCOnce()

	if rl.BucketCount() != 0 {
		t.Fatalf("expected 0 buckets after GC past TTL, got %d", rl.BucketCount())
	}
}

// TestRateLimiter_GCKeepsActiveKeys verifies GCOnce does NOT evict a key
// that was recently active (last access < idleTTL).
func TestRateLimiter_GCKeepsActiveKeys(t *testing.T) {
	now, advance := newTestClock(time.Unix(1_000, 0))
	const ttl = 5 * time.Minute
	rl := newRateLimiterWithClock(1, 10, ttl, now)

	rl.Allow("active")
	advance(ttl - time.Second) // still within TTL
	rl.GCOnce()

	if rl.BucketCount() != 1 {
		t.Fatalf("expected active bucket to survive GC, got count=%d", rl.BucketCount())
	}
}

// TestRateLimiter_ZeroTTLGCIsNoop verifies GCOnce is a no-op when idleTTL=0
// (caller manages eviction externally).
func TestRateLimiter_ZeroTTLGCIsNoop(t *testing.T) {
	now, _ := newTestClock(time.Unix(1_000, 0))
	rl := newRateLimiterWithClock(1, 10, 0, now)
	rl.Allow("x")
	rl.GCOnce() // must not panic, must not evict
	if rl.BucketCount() != 1 {
		t.Fatalf("expected bucket to survive zero-TTL GC, got count=%d", rl.BucketCount())
	}
}

// TestRateLimiter_Middleware_Allows verifies the Middleware passes requests
// that are within the rate limit through to the wrapped handler.
func TestRateLimiter_Middleware_Allows(t *testing.T) {
	now, _ := newTestClock(time.Unix(1_000, 0))
	rl := newRateLimiterWithClock(1, 5, time.Minute, now)

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := rl.Middleware(inner)

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.0.0.1:55000"
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("within-limit request: got %d want 200", rw.Code)
	}
}

// TestRateLimiter_Middleware_Throttles verifies the Middleware returns HTTP 429
// once the bucket is exhausted.
func TestRateLimiter_Middleware_Throttles(t *testing.T) {
	now, _ := newTestClock(time.Unix(1_000, 0))
	rl := newRateLimiterWithClock(0.001, 2, time.Minute, now) // nearly zero refill

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := rl.Middleware(inner)

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.0.0.2:55001"

	// First two allowed (burst=2).
	for i := 0; i < 2; i++ {
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, req)
		if rw.Code != http.StatusOK {
			t.Fatalf("burst request %d: got %d want 200", i+1, rw.Code)
		}
	}
	// Third is throttled.
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code != http.StatusTooManyRequests {
		t.Fatalf("throttled request: got %d want 429", rw.Code)
	}
}

// TestRateLimiter_Middleware_XFFPreferred verifies that when XFF trust is
// enabled the rate limiter keys on the X-Forwarded-For client IP rather than
// the proxy's TCP address.
func TestRateLimiter_Middleware_XFFPreferred(t *testing.T) {
	now, _ := newTestClock(time.Unix(1_000, 0))
	rl := newRateLimiterWithClock(1, 2, time.Minute, now)

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// Use MiddlewareWithTrust(true) to enable XFF for this test.
	h := rl.MiddlewareWithTrust(inner, true)

	// Two requests from the SAME proxy (RemoteAddr) but DIFFERENT XFF client IPs.
	for i, xff := range []string{"203.0.113.1", "203.0.113.2"} {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = "10.10.10.10:9090" // proxy
		req.Header.Set("X-Forwarded-For", xff)
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, req)
		if rw.Code != http.StatusOK {
			t.Fatalf("XFF IP %d (%s): got %d want 200", i, xff, rw.Code)
		}
	}

	// Both XFF IPs still have tokens in their fresh buckets (each got 1 of 2).
	for i, xff := range []string{"203.0.113.1", "203.0.113.2"} {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = "10.10.10.10:9090"
		req.Header.Set("X-Forwarded-For", xff)
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, req)
		if rw.Code != http.StatusOK {
			t.Fatalf("second request from XFF IP %d (%s): got %d want 200 (expected independent buckets)", i, xff, rw.Code)
		}
	}
}

// TestRateLimiter_Middleware_XFFChainTakesFirst verifies that when XFF trust
// is enabled, a comma-separated X-Forwarded-For uses the leftmost (client)
// entry as the rate-limiter key.
func TestRateLimiter_Middleware_XFFChainTakesFirst(t *testing.T) {
	now, _ := newTestClock(time.Unix(1_000, 0))
	rl := newRateLimiterWithClock(0.001, 1, time.Minute, now) // burst=1 so second request throttles

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	// Use MiddlewareWithTrust(true) to enable XFF for this test.
	h := rl.MiddlewareWithTrust(inner, true)

	mkReq := func() *http.Request {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = "10.0.0.1:80"
		req.Header.Set("X-Forwarded-For", "198.51.100.7, 10.0.0.1") // client, proxy
		return req
	}

	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, mkReq())
	if rw.Code != http.StatusOK {
		t.Fatalf("first: %d", rw.Code)
	}
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, mkReq()) // same XFF client → same bucket → throttled
	if rw.Code != http.StatusTooManyRequests {
		t.Fatalf("second (same XFF client): got %d want 429", rw.Code)
	}
}

// TestRateLimiter_SignalGate_ThrottlesHighFrequencyClient verifies that the
// signal gate's /rtc endpoint returns 429 when the per-IP rate limiter is
// exhausted, and that the upstream LiveKit is never reached for the throttled
// request. XFF trust is enabled (MEET_TRUSTED_PROXY=1) so the limiter keys
// on the client IP passed in X-Forwarded-For.
func TestRateLimiter_SignalGate_ThrottlesHighFrequencyClient(t *testing.T) {
	// Enable XFF trust so the rate limiter keys on the XFF client IP rather
	// than the loopback TCP addr (which changes per-connection in httptest).
	t.Setenv(TrustedProxyEnv, "1")

	f := &fakeLiveKitSignal{}
	upstream := newFakeLiveKitSignal(t, f)
	defer upstream.Close()
	addr := strings.TrimPrefix(upstream.URL, "http://")

	g, _ := newGateForTest(t, addr)
	// Very tight limiter: 1 burst, ~0 refill — so first request passes, second throttles.
	now, _ := newTestClock(time.Unix(1_000, 0))
	rl := newRateLimiterWithClock(0.001, 1, time.Minute, now)
	g.SetRateLimiter(rl)

	gate := httptest.NewServer(g.Handler(nil, nil))
	defer gate.Close()

	tok := mintToken(t, "acme", "standup", time.Hour)
	getFrom := func(ip string) int {
		req, _ := http.NewRequest("GET", gate.URL+"/rtc?access_token="+tok, nil)
		req.Header.Set("X-Forwarded-For", ip)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// First request from this IP: allowed (burst=1).
	if code := getFrom("203.0.113.99"); code != http.StatusOK {
		t.Fatalf("first request: got %d want 200", code)
	}
	// Second request immediately: throttled (burst exhausted, refill ~0).
	if code := getFrom("203.0.113.99"); code != http.StatusTooManyRequests {
		t.Fatalf("throttled request: got %d want 429", code)
	}
	// The throttled request must NOT have reached the upstream.
	if len(f.hits) != 1 {
		t.Fatalf("upstream should be hit exactly once (the allowed request), got %v", f.hits)
	}
}

// TestRateLimiter_AdminServer_ThrottlesHighFrequency verifies the admin
// server's rate limiter returns 429 when the per-IP bucket is exhausted.
// XFF trust is enabled so the limiter keys on the client IP in the header.
func TestRateLimiter_AdminServer_ThrottlesHighFrequency(t *testing.T) {
	// Enable XFF trust so the rate limiter sees the client IP from XFF
	// rather than the loopback TCP address (which varies per-connection).
	t.Setenv(TrustedProxyEnv, "1")

	admin, rooms := newTestAdminServer(t)
	ctx := context.Background()
	rooms.CreateRoom(ctx, "acme:standup")

	now, _ := newTestClock(time.Unix(1_000, 0))
	rl := newRateLimiterWithClock(0.001, 1, time.Minute, now)
	admin.SetRateLimiter(rl)

	srv := httptest.NewServer(admin.Handler())
	defer srv.Close()

	do := func(ip string) int {
		req, _ := http.NewRequest("GET", srv.URL+"/admin/tenants/acme/rooms", nil)
		req.Header.Set("Authorization", "Bearer supersecrettoken")
		req.Header.Set("X-Forwarded-For", ip)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if code := do("10.0.0.5"); code != http.StatusOK {
		t.Fatalf("first request: got %d want 200", code)
	}
	if code := do("10.0.0.5"); code != http.StatusTooManyRequests {
		t.Fatalf("throttled request: got %d want 429", code)
	}
}

// TestRateLimiter_XFFUntrustedDefaultPreventsRotation verifies that when XFF
// trust is OFF (the default) a client cannot bypass the rate limiter by rotating
// its X-Forwarded-For header — the limiter keys on RemoteAddr instead.
func TestRateLimiter_XFFUntrustedDefaultPreventsRotation(t *testing.T) {
	// Ensure TrustedProxyEnv is absent for this test (default = off).
	t.Setenv(TrustedProxyEnv, "")

	now, _ := newTestClock(time.Unix(1_000, 0))
	rl := newRateLimiterWithClock(0.001, 1, time.Minute, now) // burst=1

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := rl.Middleware(inner) // XFF trust determined by env (off)

	// First request from a stable RemoteAddr: allowed.
	req1 := httptest.NewRequest("GET", "/", nil)
	req1.RemoteAddr = "198.51.100.5:12345"
	req1.Header.Set("X-Forwarded-For", "10.0.0.1") // claimed client IP
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req1)
	if rw.Code != http.StatusOK {
		t.Fatalf("first request: got %d want 200", rw.Code)
	}

	// Second request from the SAME RemoteAddr but a DIFFERENT XFF header:
	// without XFF trust the limiter still sees the same RemoteAddr bucket → 429.
	req2 := httptest.NewRequest("GET", "/", nil)
	req2.RemoteAddr = "198.51.100.5:12345"
	req2.Header.Set("X-Forwarded-For", "10.0.0.99") // rotated XFF — must be ignored
	rw2 := httptest.NewRecorder()
	h.ServeHTTP(rw2, req2)
	if rw2.Code != http.StatusTooManyRequests {
		t.Fatalf("rotated-XFF bypass attempt: got %d want 429 (XFF must be ignored when MEET_TRUSTED_PROXY is off)", rw2.Code)
	}
}

// TestRateLimiter_DifferentIPsAreIndependent verifies that throttling one IP
// does NOT affect a different IP (critical: a noisy client must not DoS others).
func TestRateLimiter_DifferentIPsAreIndependent(t *testing.T) {
	now, _ := newTestClock(time.Unix(1_000, 0))
	rl := newRateLimiterWithClock(0.001, 1, time.Minute, now)

	// Exhaust IP A's bucket.
	rl.Allow("192.0.2.1")
	if rl.Allow("192.0.2.1") {
		t.Fatalf("IP A: should be throttled after draining burst")
	}

	// IP B (a different client) must NOT be affected.
	if !rl.Allow("192.0.2.2") {
		t.Fatalf("IP B should not be throttled because IP A was throttled (independent buckets)")
	}
}

// TestRateLimiter_ConcurrentAccess exercises the rate limiter under concurrent
// load to shake out data races (run with -race).
func TestRateLimiter_ConcurrentAccess(t *testing.T) {
	rl := NewRateLimiter(1000, 1000, time.Minute) // high limits so rarely blocks
	var wg sync.WaitGroup
	var allowed, denied atomic.Int64

	for i := 0; i < 50; i++ {
		key := fmt.Sprintf("ip-%d", i%5) // 5 distinct keys across 50 goroutines
		wg.Add(1)
		go func(k string) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if rl.Allow(k) {
					allowed.Add(1)
				} else {
					denied.Add(1)
				}
			}
		}(key)
	}
	wg.Wait()
	// Sanity check: with a very permissive limiter most requests are allowed.
	if allowed.Load() == 0 {
		t.Fatalf("no requests were allowed under permissive limiter")
	}
}
