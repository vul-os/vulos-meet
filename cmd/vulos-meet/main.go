// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

// Command vulos-meet is the Vulos video-meetings server: a small Go wrapper
// that supervises LiveKit Server (an external child process) and adds the
// Vulos-specific pieces — token validation (VULOS-MEET/1), per-tenant
// room-namespace enforcement, a small admin HTTP surface, and a region tag
// for vulos-cloud's geo-router.
//
// See README.md for the (a) vendor vs (b) supervise decision (we supervise).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/vul-os/vulos-apps/appsplatform"
	"github.com/vul-os/vulos-meet/internal/apikey"
	"github.com/vul-os/vulos-meet/internal/cp"
	"github.com/vul-os/vulos-meet/internal/wrap"
	"github.com/vul-os/vulos-meet/web"
)

// Version is set at build time via -ldflags "-X main.Version=vX.Y.Z".
// It defaults to "dev" for local builds.
var Version = "dev"

// Apps & Bots place env knobs (open-core seam).
//
//	appsDBEnv       — path to the standalone SQLite registry; empty = in-memory.
//	appsCloudURLEnv — selects a Vulos Cloud control-plane registry. This binary
//	                  ships standalone-only, so setting it is a hard error rather
//	                  than a silent downgrade.
//	mcpGatewayEnv   — selects a Vulos Cloud MCP-aggregation gateway (one agent
//	                  endpoint fanning out across products). This binary ships
//	                  standalone-only, so setting it is a hard error rather than a
//	                  silent downgrade (mirrors appsCloudURLEnv).
const (
	appsDBEnv       = "MEET_APPS_DB"
	appsCloudURLEnv = "MEET_APPS_CLOUD_URL"
	mcpGatewayEnv   = "MEET_APPS_MCP_GATEWAY_URL"
)

func main() {
	var (
		configPath  = flag.String("config", "", "path to YAML config file (required)")
		adminAddr   = flag.String("addr", "", "admin HTTP listen address (overrides config.admin.addr)")
		metricsAddr = flag.String("metrics-addr", "", "metrics HTTP listen address (default \"127.0.0.1:7882\")")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("vulos-meet", Version)
		return
	}

	if err := run(*configPath, *adminAddr, *metricsAddr); err != nil {
		log.Fatalf("vulos-meet: %v", err)
	}
}

func run(configPath, adminAddrOverride, metricsAddrOverride string) error {
	cfg, err := wrap.LoadConfig(configPath)
	if err != nil {
		return err
	}
	if adminAddrOverride != "" {
		cfg.Admin.Addr = adminAddrOverride
	}
	metricsAddr := metricsAddrOverride
	if metricsAddr == "" {
		// Default to loopback so /metrics is reachable by a localhost
		// scraper or sidecar without exposing admin to the same network.
		metricsAddr = "127.0.0.1:7882"
	}

	tenant := wrap.NewTenant(cfg.TenantSeparator)

	geo, err := wrap.NewGeoRouter(cfg.Region)
	if err != nil {
		return err
	}

	// Metrics registry. Shared by admin (request counter + per-tenant room
	// gauge), validator (token-outcome counter), and the signaling gate.
	metrics := wrap.NewMetrics()

	// Real LiveKit RoomService client (gRPC to the supervised child). The
	// admin surface and tenant gate are unchanged — they speak to the
	// RoomService interface only.
	rooms, err := wrap.NewLiveKitRoomService(wrap.LiveKitRoomServiceConfig{
		SignalingAddr: cfg.LiveKit.SignalingAddr,
		APIKey:        cfg.LiveKit.APIKey,
		APISecret:     cfg.LiveKit.APISecret,
	})
	if err != nil {
		return err
	}

	admin, err := wrap.NewAdminServer(tenant, rooms, geo, cfg.Admin.Token, Version)
	if err != nil {
		return err
	}
	admin.SetMetrics(metrics)
	// Per-IP rate limiter on the admin surface. 5 req/s burst, 2 req/s sustained.
	// Admin ops are infrequent; rate limiting protects against brute-force on the
	// admin bearer token and accidental rapid-fire scripting.
	adminLimiter := wrap.NewRateLimiter(2, 5, 10*time.Minute)
	admin.SetRateLimiter(adminLimiter)

	// vk_ API-key introspection seam. When VULOS_CP_BASE_URL is set, the admin
	// API also accepts `Authorization: Bearer vk_…` keys issued by the Vulos
	// control plane alongside the existing MEET_ADMIN_TOKEN bearer. The key is
	// validated via POST {VULOS_CP_BASE_URL}/api/keys/introspect; it must be
	// valid and carry the "meet" product scope. Results are cached ~60s.
	// When VULOS_CP_BASE_URL is unset (self-host / standalone) this seam is OFF
	// and the admin API is unchanged — only the static MEET_ADMIN_TOKEN applies.
	if apikeyIntro := apikey.NewIntrospector(apikey.FromEnv()); apikeyIntro != nil {
		admin.SetIntrospector(apikeyIntro)
		log.Printf("vulos-meet: vk_ API-key auth ENABLED on admin API (VULOS_CP_BASE_URL set)")
	} else {
		log.Printf("vulos-meet: vk_ API-key auth disabled on admin API (VULOS_CP_BASE_URL unset — standalone)")
	}

	// Surface the configured room ceilings on the metrics surface so a scrape
	// can correlate active rooms against the per-room participant cap and the
	// per-box room ceiling.
	metrics.SetRoomLimits(cfg.Room.MaxParticipants, cfg.Room.MaxRooms)

	// Validator is the admission seam used by the signaling reverse proxy.
	// We construct it here so a malformed key/secret pair fails fast at
	// startup instead of at first connection.
	validator, err := wrap.NewValidator(cfg.LiveKit.APIKey, cfg.LiveKit.APISecret, tenant)
	if err != nil {
		return err
	}
	validator.SetMetrics(metrics)

	// Signaling reverse proxy: validates VULOS-MEET/1 tokens before
	// forwarding the WebSocket upgrade to livekit-server's /rtc.
	signalGate, err := wrap.NewSignalGate(validator, cfg.LiveKit.SignalingAddr)
	if err != nil {
		return err
	}
	// Per-IP token-bucket rate limiter on /rtc. 20 req/s burst, 10 req/s
	// sustained — generous enough for browsers reconnecting on network changes
	// (the LiveKit JS SDK reconnects up to ~5× in a short window), strict enough
	// to blunt a scanner driving the JWT-signature-verify cost. TURN credential
	// exchange rides the same WebSocket upgrade on /rtc, so this limiter covers
	// the TURN-credential endpoint too.
	rtcLimiter := wrap.NewRateLimiter(10, 20, 10*time.Minute)
	signalGate.SetRateLimiter(rtcLimiter)

	// Enforce the per-box concurrent-room ceiling at the gate. LiveKit's config
	// has auto_create:true, so a join to a not-yet-existing room would otherwise
	// create it unbounded; the gate is the reliable enforcement point. A join to
	// an already-active room is unaffected (cfg.Room.MaxRooms <= 0 = unbounded).
	// Reuses the same RoomService the admin surface lists with — no extra deps.
	signalGate.SetRoomCap(rooms, cfg.Room.MaxRooms, metrics)

	// Egress Twirp reverse proxy: validates VULOS-MEET/1 tokens with the
	// per-egress RoomRecord grant invariant before forwarding the body
	// verbatim to livekit-server's /twirp/livekit.Egress/<Method> surface.
	// vulos-meet is the sole LiveKit-talking surface — cloud's
	// MEET_EGRESS_BASE_URL targets this listener, not LiveKit-Server
	// directly. See CONTRIBUTING-FORK.md §6.
	egressProxy, err := wrap.NewEgressProxy(validator, cfg.LiveKit.EgressUpstreamAddr)
	if err != nil {
		return err
	}
	egressProxy.SetMetrics(metrics)

	// Recording lifecycle ledger (MEET-RECORDING-RETENTION-06). The blob bytes
	// live in the cloud sink; this is the local state machine + retention
	// driver. The egress receiver advances each egress's lifecycle from the
	// webhook event; the driver sweeps past-retention recordings and dispatches
	// the actual blob delete through a seam.
	recStore := wrap.NewMemRecordingStore()
	recCloudTok := os.Getenv("MEET_RECORDING_CLOUD_TOKEN")

	// Egress webhook receiver: forwards LiveKit egress events to the cloud
	// (MEET-RECORDING-01) after verifying the LiveKit signature, and advances
	// the local recording lifecycle ledger.
	egressRx, err := wrap.NewEgressReceiver(wrap.EgressReceiverConfig{
		Tenant:       tenant,
		APIKey:       cfg.LiveKit.APIKey,
		APISecret:    cfg.LiveKit.APISecret,
		CloudURL:     cfg.Recording.EgressEndpoint,
		CloudAuthTok: recCloudTok,
		Store:        recStore,
		Metrics:      metrics,
	})
	if err != nil {
		return err
	}

	// OPTIONAL cp metering seam. The control-plane usage client is built ONLY
	// when CP_URL is set; when it is unset the seam is OFF and vulos-meet is the
	// standalone, cp-free product. The core (wrap) never imports internal/cp —
	// this is the ONLY place the two meet. The client satisfies
	// wrap.UsageReporter structurally; wrap sees only the interface.
	var usageReporter wrap.UsageReporter
	var cpClient *cp.UsageClient
	if cpURL := os.Getenv(cp.EnvCPURL); cpURL != "" {
		c, err := cp.NewUsageClient(cp.Config{
			URL:          cpURL,
			SharedSecret: os.Getenv(cp.EnvCPSharedSecret),
		})
		if err != nil {
			return err
		}
		cpClient = c
		usageReporter = c
		log.Printf("vulos-meet: cp metering ENABLED (CP_URL set) — reporting meet usage to control plane")
	} else {
		log.Printf("vulos-meet: cp metering disabled (CP_URL unset) — running standalone")
	}

	// Meet-usage webhook receiver: verifies the LiveKit signature on
	// room/participant lifecycle events, computes participant-minutes per room,
	// and reports them to cp (when wired) as each room finishes. With no
	// reporter it still verifies + tracks for the admin usage read.
	usageRx, err := wrap.NewUsageReceiver(wrap.UsageReceiverConfig{
		Tenant:    tenant,
		APIKey:    cfg.LiveKit.APIKey,
		APISecret: cfg.LiveKit.APISecret,
		Reporter:  usageReporter,
		Metrics:   metrics,
	})
	if err != nil {
		return err
	}
	// Expose the live participant-minute accrual on the admin stats read.
	admin.SetUsageStatter(usageRx)

	// Apps & Bots place (shared @vulos/apps platform). The registry is the
	// open-core seam: the STANDALONE DEFAULT (pure-Go SQLite, durable when
	// MEET_APPS_DB names a file, else in-memory) ships in this binary; a Vulos
	// Cloud control-plane registry implements the SAME appsplatform.Registry in
	// a separate package this core never compiles in. Selecting it is explicit
	// and env-gated here in the composition root — the OSS binary refuses to
	// silently downgrade a cloud request to standalone.
	if os.Getenv(appsCloudURLEnv) != "" {
		return fmt.Errorf("vulos-meet: %s is set but this build has no cloud apps control-plane registry compiled in (standalone-only)", appsCloudURLEnv)
	}
	var appsReg appsplatform.Registry
	if dsn := os.Getenv(appsDBEnv); dsn != "" {
		r, err := appsplatform.NewStandaloneRegistry(dsn, appsplatform.WithScopeSet(wrap.MeetAppScopeSet()))
		if err != nil {
			return fmt.Errorf("vulos-meet: open apps registry: %w", err)
		}
		defer r.Close()
		appsReg = r
		log.Printf("vulos-meet: apps & bots place ENABLED (durable registry at %s)", dsn)
	} else {
		appsReg = appsplatform.NewMemoryRegistry(appsplatform.WithScopeSet(wrap.MeetAppScopeSet()))
		log.Printf("vulos-meet: apps & bots place ENABLED (in-memory registry; set %s for durability)", appsDBEnv)
	}
	// The adapter acts/reads through the SAME RoomService the admin surface uses
	// (no new SFU dependency), and the management API reuses the admin bearer.
	appsHandler, err := wrap.NewAppsHandler(wrap.AppsConfig{
		Registry:   appsReg,
		SFU:        rooms,
		AdminToken: cfg.Admin.Token,
	})
	if err != nil {
		return err
	}

	// MCP place: the SAME Meet adapter + app registry + per-app (vat_) tokens as
	// the REST /api/apps mount, exposed as an MCP server so any LLM/agent can
	// operate Meet. The optional cloud MCP-aggregation gateway is an env-gated
	// seam: this binary compiles in no gateway, so selecting one is a hard error
	// rather than a silent downgrade to standalone (mirrors MEET_APPS_CLOUD_URL).
	if os.Getenv(mcpGatewayEnv) != "" {
		return fmt.Errorf("vulos-meet: %s is set but this build has no cloud MCP gateway compiled in (standalone-only)", mcpGatewayEnv)
	}
	mcpHandler, err := wrap.NewMCPHandler(wrap.MCPConfig{
		Registry: appsReg,
		SFU:      rooms,
	})
	if err != nil {
		return err
	}

	// Retention cleanup driver. The blob deleter is the genuinely-external seam:
	// when recording.cloud_delete_base_url is set, deletions are dispatched to
	// the vulos-cloud MEET-RECORDING-01 sink (the blob owner); otherwise the
	// box runs self-host (no central blob — the no-op deleter advances the
	// ledger). The sweep loop only runs when a retention rule is configured.
	var blobDeleter wrap.RecordingBlobDeleter
	if cfg.Recording.CloudDeleteBaseURL != "" {
		cd, err := wrap.NewCloudBlobDeleter(cfg.Recording.CloudDeleteBaseURL, recCloudTok, nil)
		if err != nil {
			return err
		}
		blobDeleter = cd
	}
	retentionDriver, err := wrap.NewRetentionDriver(recStore, cfg.RetentionPolicy(), blobDeleter)
	if err != nil {
		return err
	}
	retentionDriver.SetMetrics(metrics)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	errCh := make(chan error, 4)

	// Admin HTTP server.
	adminSrv := &http.Server{
		Addr:              cfg.Admin.Addr,
		Handler:           admin.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Printf("vulos-meet: admin listening on %s (region=%s)", cfg.Admin.Addr, cfg.Region)
		if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("admin server: %w", err)
		}
	}()

	// Metrics HTTP server (separate listener, scoped to internal network).
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", metrics.Handler())
	metricsSrv := &http.Server{
		Addr:              metricsAddr,
		Handler:           metricsMux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Printf("vulos-meet: metrics listening on %s", metricsAddr)
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("metrics server: %w", err)
		}
	}()

	// Sibling handler for the signal-gate's non-signaling routes: the egress
	// webhook receiver AND the meet-usage webhook receiver share the listener.
	// Each mounts its own distinct path, so a single mux dispatches both without
	// either shadowing the other.
	siblingMux := http.NewServeMux()
	// GET /healthz — unauthenticated liveness + version probe on the PUBLIC
	// signal-gate listener. Intended for load balancers and container health
	// checks that probe the same port clients connect on. Distinct from
	// GET /admin/health (admin listener only; no version) and GET /admin/info
	// (admin listener, version but admin-token-gated).
	siblingMux.Handle("GET /healthz", wrap.NewHealthzHandler(Version))
	siblingMux.Handle(wrap.WebhookPath, egressRx.Handler())
	siblingMux.Handle(wrap.UsageWebhookPath, usageRx.Handler())
	// Apps & Bots place: GET /api/apps (the consolidation contract Workspace
	// reads) plus the runtime (Bearer app-token) and incoming-webhook routes.
	// Registered as exact + subtree patterns so they take precedence over the
	// "/" web-client catch-all; the signal-gate still owns /rtc + egress and
	// strips its headers on those proxied paths only. Management routes reuse the
	// admin bearer; runtime routes authenticate with per-app tokens.
	siblingMux.Handle(appsHandler.BasePath, appsHandler)
	siblingMux.Handle(appsHandler.BasePath+"/", appsHandler)
	// MCP server: same adapter/registry/per-app-token surface as /api/apps, in
	// the MCP (JSON-RPC over Streamable HTTP) shape so agents can operate Meet.
	// Mounted as exact + subtree patterns behind the same signal-gate, which
	// continues to own /rtc + egress and strips its headers only on those paths.
	siblingMux.Handle(mcpHandler.BasePath, mcpHandler)
	siblingMux.Handle(mcpHandler.BasePath+"/", mcpHandler)
	// Embedded meeting/call web client. Served at the root of the public
	// signal-gate listener (the same origin as /rtc), so opening the meet
	// service in a browser yields the join UI and the LiveKit client SDK
	// connects back to /rtc on this very origin. SPA fallback handles
	// deep links (/<roomId>). The webhook paths above are registered as
	// exact subtrees and take precedence over this "/" catch-all.
	siblingMux.Handle("/", web.Handler())

	// Signaling gate (reverse proxy in front of /rtc).
	// The public listener rate-limits ALL routes (not just /rtc) to prevent
	// unauthenticated callers from abusing webhooks, the web root, /api/apps,
	// or /mcp endpoints. rtcLimiter is already attached to signalGate for /rtc
	// specifically; wrapping the whole handler here is additive — /rtc passes
	// the middleware check THEN the per-gate check, which is intentional.
	signalSrv := &http.Server{
		Addr:              cfg.SignalGateAddr(),
		Handler:           rtcLimiter.Middleware(signalGate.Handler(siblingMux, egressProxy)),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Printf("vulos-meet: signal-gate listening on %s (/rtc -> %s, /twirp/livekit.Egress -> %s)", signalSrv.Addr, cfg.LiveKit.SignalingAddr, cfg.LiveKit.EgressUpstreamAddr)
		if err := signalSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("signal-gate server: %w", err)
		}
	}()

	// Recording retention sweeper. Runs only when a retention rule is set
	// (RetentionSweepInterval() returns 0 otherwise, and RunLoop is a no-op).
	if iv := cfg.RetentionSweepInterval(); iv > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			log.Printf("vulos-meet: recording retention sweeper every %s (ttl=%s max_per_room=%d max_per_tenant=%d cloud_delete=%t)",
				iv, cfg.Recording.RetentionTTL, cfg.Recording.RetentionMaxPerRoom, cfg.Recording.RetentionMaxPerTenant, cfg.Recording.CloudDeleteBaseURL != "")
			retentionDriver.RunLoop(ctx, iv)
		}()
	}

	// LiveKit Server child process.
	lkConfigPath := filepath.Join(os.TempDir(), "vulos-meet-livekit.yaml")
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Printf("vulos-meet: supervising livekit-server (signaling=%s)", cfg.LiveKit.SignalingAddr)
		if err := wrap.SuperviseLiveKit(ctx, cfg, lkConfigPath); err != nil {
			errCh <- fmt.Errorf("livekit supervise: %w", err)
		}
	}()

	// Wait for signal or first error.
	select {
	case <-ctx.Done():
		log.Printf("vulos-meet: shutdown signal received")
	case err := <-errCh:
		log.Printf("vulos-meet: subsystem failed: %v", err)
		stop()
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = adminSrv.Shutdown(shutdownCtx)
	_ = metricsSrv.Shutdown(shutdownCtx)
	_ = signalSrv.Shutdown(shutdownCtx)
	rooms.Close()
	if cpClient != nil {
		// Flush any queued usage events before exit (bounded by the client's
		// own retry budget). nil-safe.
		_ = cpClient.Close()
	}

	wg.Wait()
	return nil
}
