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
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("vulos-meet", version)
		return
	}

	if err := run(*configPath, *adminAddr); err != nil {
		log.Fatalf("vulos-meet: %v", err)
	}
}

func run(configPath, adminAddrOverride string) error {
	cfg, err := wrap.LoadConfig(configPath)
	if err != nil {
		return err
	}
	if adminAddrOverride != "" {
		cfg.Admin.Addr = adminAddrOverride
	}

	tenant := wrap.NewTenant(cfg.TenantSeparator)

	geo, err := wrap.NewGeoRouter(cfg.Region)
	if err != nil {
		return err
	}

	// In-memory room service for now. The follow-up MEET-ROOMSVC task swaps
	// this for the real livekit/protocol RoomServiceClient (gRPC). The
	// admin surface and tenant gate do not change.
	rooms := wrap.NewMemoryRoomService()

	admin, err := wrap.NewAdminServer(tenant, rooms, geo, cfg.Admin.Token, version)
	if err != nil {
		return err
	}

	// Validator is constructed but only used by the (future) signaling
	// gate. We construct it now so a malformed key/secret pair fails fast
	// at startup instead of at first connection.
	if _, err := wrap.NewValidator(cfg.LiveKit.APIKey, cfg.LiveKit.APISecret, tenant); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	errCh := make(chan error, 2)

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

	wg.Wait()
	return nil
}
