// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

// Package cp is the OPTIONAL Vulos control-plane (cp) metering adapter.
//
// It is the removable seam that lets a vulos-meet box report meet usage
// (participant-minutes, room lifecycle) to a central Vulos control plane so
// the suite-wide billing model can meter meet alongside vulos-mail,
// vulos-office, and llmux.
//
// IMPORTANT — the import boundary that keeps vulos-meet standalone:
//
//   - The core (internal/wrap) NEVER imports this package. It defines a tiny
//     reporter interface (wrap.UsageReporter) and accepts an implementation
//     at construction. When no implementation is wired, the core is exactly
//     the standalone, cp-free vulos-meet it has always been.
//   - main.go is the ONLY place the two are stitched together: it constructs a
//     cp.UsageClient (this package) ONLY when CP_URL is configured, and hands
//     it to the core as a wrap.UsageReporter. When CP_URL is unset, no client
//     is built and the seam is OFF — vulos-meet is unchanged.
//
// The wire contract (frozen):
//
//	POST {CP_URL}/api/usage
//	Header: X-Relay-Auth: <CP_SHARED_SECRET>
//	Body:   {"product":"meet","account_id":"<tenant>","kind":"meet_minutes","count":<n>}
//
// Delivery is fire-and-forget with a bounded retry: a Report call enqueues
// the event and returns immediately; a background worker drains the queue and
// retries transient failures a bounded number of times. The metering path must
// never block the LiveKit webhook hot path.
package cp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"
)

// Product is the suite-wide product identifier for meet usage rows.
const Product = "meet"

// KindMeetMinutes is the usage "kind" for participant-minute metering.
const KindMeetMinutes = "meet_minutes"

// Env var names for the cp seam. When CP_URL is empty the seam is OFF.
const (
	EnvCPURL          = "CP_URL"
	EnvCPSharedSecret = "CP_SHARED_SECRET"
)

// usageEvent is the JSON body shape POSTed to {CP_URL}/api/usage.
type usageEvent struct {
	Product   string `json:"product"`
	AccountID string `json:"account_id"`
	Kind      string `json:"kind"`
	Count     int64  `json:"count"`
}

// Config configures the cp usage client.
type Config struct {
	// URL is the cp base URL (CP_URL). REQUIRED — NewUsageClient returns an
	// error when empty so a misconfigured deploy fails fast instead of
	// silently dropping usage.
	URL string

	// SharedSecret is sent as the X-Relay-Auth header (CP_SHARED_SECRET).
	SharedSecret string

	// HTTPClient is the client used for the POST. Defaults to a 10s-timeout
	// client.
	HTTPClient *http.Client

	// MaxAttempts caps the per-event retry count. Defaults to 4 (initial + 3
	// retries).
	MaxAttempts int

	// BaseBackoff is the initial retry backoff, doubled each retry. Defaults
	// to 250ms.
	BaseBackoff time.Duration

	// QueueSize bounds the in-flight event queue. When full, Report drops the
	// event (and logs) rather than blocking the caller — the metering path
	// must never block the webhook hot path. Defaults to 1024.
	QueueSize int

	// Logger is used for drop/failure diagnostics. Defaults to the standard
	// logger.
	Logger *log.Logger
}

// UsageClient is the fire-and-forget cp metering client. It satisfies the
// wrap.UsageReporter interface (Report + Close) by structural match — wrap
// does not import this package; main.go passes a *UsageClient where a
// wrap.UsageReporter is expected.
type UsageClient struct {
	cfg    Config
	queue  chan usageEvent
	wg     sync.WaitGroup
	logger *log.Logger

	closeOnce sync.Once
	done      chan struct{}
}

// NewUsageClient builds a cp usage client and starts its background drain
// worker. URL must be non-empty (the caller is responsible for only building a
// client when CP_URL is set — see main.go). Call Close to flush + stop.
func NewUsageClient(cfg Config) (*UsageClient, error) {
	if cfg.URL == "" {
		return nil, errors.New("vulos-meet/cp: usage client requires CP_URL")
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 4
	}
	if cfg.BaseBackoff <= 0 {
		cfg.BaseBackoff = 250 * time.Millisecond
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 1024
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	logger := cfg.Logger
	if logger == nil {
		logger = log.Default()
	}
	c := &UsageClient{
		cfg:    cfg,
		queue:  make(chan usageEvent, cfg.QueueSize),
		logger: logger,
		done:   make(chan struct{}),
	}
	c.wg.Add(1)
	go c.drain()
	return c, nil
}

// ReportMeetMinutes enqueues a meet_minutes usage event for the given account
// (tenant). It is fire-and-forget: it returns immediately and never blocks. A
// non-positive count or empty account is ignored. When the queue is full the
// event is dropped with a log line rather than blocking the caller.
func (c *UsageClient) ReportMeetMinutes(accountID string, minutes int64) {
	if c == nil || accountID == "" || minutes <= 0 {
		return
	}
	ev := usageEvent{
		Product:   Product,
		AccountID: accountID,
		Kind:      KindMeetMinutes,
		Count:     minutes,
	}
	select {
	case c.queue <- ev:
	default:
		// Bounded queue full — drop rather than block the webhook path. Usage
		// metering is best-effort; back-pressure on the LiveKit hot path is not
		// acceptable.
		c.logger.Printf("vulos-meet/cp: usage queue full, dropping %s account=%s count=%d", ev.Kind, ev.AccountID, ev.Count)
	}
}

// Report is the generic enqueue used by the wrap.UsageReporter seam. account is
// the tenant; kind is the usage kind (KindMeetMinutes); count is the magnitude.
func (c *UsageClient) Report(account, kind string, count int64) {
	if c == nil || account == "" || count <= 0 {
		return
	}
	ev := usageEvent{Product: Product, AccountID: account, Kind: kind, Count: count}
	select {
	case c.queue <- ev:
	default:
		c.logger.Printf("vulos-meet/cp: usage queue full, dropping %s account=%s count=%d", ev.Kind, ev.AccountID, ev.Count)
	}
}

// drain is the single background worker that POSTs queued events with bounded
// retries. It exits when the queue is closed (Close) and fully drained.
func (c *UsageClient) drain() {
	defer c.wg.Done()
	for ev := range c.queue {
		c.post(ev)
	}
}

// post sends one usage event with bounded exponential-backoff retries. It logs
// (and gives up) after MaxAttempts rather than blocking forever — metering is
// best-effort.
func (c *UsageClient) post(ev usageEvent) {
	body, err := json.Marshal(ev)
	if err != nil {
		c.logger.Printf("vulos-meet/cp: marshal usage event: %v", err)
		return
	}
	backoff := c.cfg.BaseBackoff
	for attempt := 0; attempt < c.cfg.MaxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-c.done:
				// Closing: do not sleep through shutdown, but make a final
				// best-effort attempt below was already accounted for.
			case <-time.After(backoff):
			}
			backoff *= 2
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.URL+"/api/usage", bytes.NewReader(body))
		if err != nil {
			cancel()
			c.logger.Printf("vulos-meet/cp: build usage request: %v", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if c.cfg.SharedSecret != "" {
			req.Header.Set("X-Relay-Auth", c.cfg.SharedSecret)
		}
		resp, err := c.cfg.HTTPClient.Do(req)
		cancel()
		if err != nil {
			continue // transport error — retry
		}
		_ = resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return
		}
		// 4xx is a caller bug the cp will keep rejecting — do not retry.
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			c.logger.Printf("vulos-meet/cp: usage POST rejected %d (account=%s kind=%s) — not retrying", resp.StatusCode, ev.AccountID, ev.Kind)
			return
		}
		// 5xx — retry.
	}
	c.logger.Printf("vulos-meet/cp: usage POST gave up after %d attempts (account=%s kind=%s count=%d)", c.cfg.MaxAttempts, ev.AccountID, ev.Kind, ev.Count)
}

// Close stops accepting new events, drains what is queued, and waits for the
// worker to exit. Safe to call more than once. Nil-tolerant.
func (c *UsageClient) Close() error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		close(c.done)
		close(c.queue)
	})
	c.wg.Wait()
	return nil
}
