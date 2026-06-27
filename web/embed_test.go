// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestFS confirms the embed directive resolves (a committed dist/.gitkeep
// guarantees at least one file), so the binary always compiles and serves.
func TestFS(t *testing.T) {
	fsys, err := FS()
	if err != nil {
		t.Fatalf("FS(): %v", err)
	}
	if _, err := fsys.Open("."); err != nil {
		t.Fatalf("open dist root: %v", err)
	}
}

// TestHandlerRoot asserts the SPA handler always answers the root with 200 —
// the built index.html when present, otherwise an honest "not built" notice —
// so opening the meet service in a browser never yields a confusing blank 404.
func TestHandlerRoot(t *testing.T) {
	h := Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}
	body, _ := io.ReadAll(rec.Result().Body)
	if len(body) == 0 {
		t.Fatal("GET / returned empty body")
	}
}

// TestHandlerSPAFallback asserts an unknown (client-routed) deep link does not
// 404: it falls back to index.html when the client is built, or to the
// not-built notice otherwise. Either way it must be a 200 the SPA can boot from.
func TestHandlerSPAFallback(t *testing.T) {
	h := Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/acme:standup-2026-06-27", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /<roomId> = %d, want 200 (SPA fallback)", rec.Code)
	}
	// A path-traversal style request must never escape the embedded FS; the
	// cleaned path collapses to the fallback rather than serving host files.
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/../../etc/passwd", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("traversal request = %d, want 200 fallback", rec2.Code)
	}
	body, _ := io.ReadAll(rec2.Result().Body)
	if strings.Contains(string(body), "root:") {
		t.Fatal("traversal served host file contents")
	}
}
