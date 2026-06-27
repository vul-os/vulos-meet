// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

// Package web embeds the built Vite/React meeting client so the vulos-meet
// binary serves its join/call UI with no separate Node runtime. Run
// `npm --prefix web run build` to refresh dist/.
//
// The SPA is served from the public signal-gate listener at the root path,
// alongside /rtc and /twirp/livekit.Egress/* — so hitting the meet service in
// a browser yields the join UI, and the client connects to LiveKit at the
// SAME origin (the signal gate fronts /rtc). Deep links like /<roomId> resolve
// to index.html (client-side routing); the existing token/admission path is
// untouched — the client merely consumes a VULOS-MEET/1 token.
package web

import (
	"embed"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// all:dist pulls every built asset (including dotfiles like .gitkeep, so the
// package compiles before the first `npm run build`). A committed dist/.gitkeep
// guarantees the embed directive always has at least one file to match.
//
//go:embed all:dist
var assets embed.FS

// FS returns the built client rooted at dist/.
func FS() (fs.FS, error) {
	return fs.Sub(assets, "dist")
}

// Handler returns an http.Handler that serves the embedded SPA with a
// single-page-app fallback: a request for an existing built asset is served
// verbatim; any other path falls back to index.html so client-side routes
// (/<roomId>, deep links) resolve. When no build is present yet (dist holds
// only the .gitkeep placeholder) it returns a short, honest 200 telling the
// operator to build the client — never a confusing blank 404.
func Handler() http.Handler {
	dist, err := FS()
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "meet client unavailable", http.StatusInternalServerError)
		})
	}
	_, indexErr := fs.Stat(dist, "index.html")
	built := indexErr == nil
	fileServer := http.FileServer(http.FS(dist))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !built {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			io.WriteString(w, "vulos-meet client not built.\n\nRun `npm --prefix web install && npm --prefix web run build` then rebuild the binary.\n")
			return
		}
		// Normalise to a clean fs path (no leading slash for fs.FS).
		reqPath := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if reqPath == "" {
			reqPath = "index.html"
		}
		if f, err := dist.Open(reqPath); err == nil {
			f.Close()
			fileServer.ServeHTTP(w, r)
			return
		}
		// SPA fallback: serve index.html for unknown (client-routed) paths.
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/"
		fileServer.ServeHTTP(w, r2)
	})
}
