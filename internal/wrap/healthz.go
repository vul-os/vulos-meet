// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

package wrap

import "net/http"

// HealthzResponse is the JSON body returned by GET /healthz. It is
// intentionally minimal: just enough for a load balancer or container health
// check to confirm the signal-gate is running and to correlate the running
// build version with a deployment tag.
type HealthzResponse struct {
	Status  string `json:"status"`  // "ok" while the server is accepting traffic
	Version string `json:"version"` // build version of this binary (e.g. "0.0.1-dev")
}

// NewHealthzHandler returns an http.Handler that serves GET /healthz.
//
// The response is HTTP 200 + {"status":"ok","version":"<version>"} to all
// callers without authentication. The healthz endpoint is deliberately
// public — load balancers and Kubernetes liveness probes must be able to
// reach it. Unlike /admin/health (which lives on the admin listener), this
// handler mounts on the signal-gate listener so external health checks hit
// the same port as clients.
//
// version should be the build version string (the `version` constant in
// cmd/vulos-meet/main.go). An empty version is coerced to "0.0.0-unknown".
func NewHealthzHandler(version string) http.Handler {
	if version == "" {
		version = "0.0.0-unknown"
	}
	resp := HealthzResponse{Status: "ok", Version: version}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, resp)
	})
}
