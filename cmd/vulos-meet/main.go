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

	"github.com/vul-os/vulos-meet/internal/wrap"
)

const version = "0.0.1-dev"

func main() {
	var (
		configPath  = flag.String("config", "", "path to YAML config file (required)")
		adminAddr   = flag.String("addr", "", "admin HTTP listen address (overrides config.admin.addr)")
		metricsAddr = flag.String("metrics-addr", "", "metrics HTTP listen address (default \"127.0.0.1:7882\")")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("vulos-meet", version)
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

	admin, err := wrap.NewAdminServer(tenant, rooms, geo, cfg.Admin.Token, version)
	if err != nil {
		return err
	}
	admin.SetMetrics(metrics)

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

	// Signaling gate (reverse proxy in front of /rtc).
	signalSrv := &http.Server{
		Addr:              cfg.SignalGateAddr(),
		Handler:           signalGate.Handler(egressRx.Handler(), egressProxy),
		ReadHeaderTimeout: 5 * time.Second,
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

	wg.Wait()
	return nil
}
