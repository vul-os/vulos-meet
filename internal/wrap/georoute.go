// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

package wrap

import "errors"

// GeoRouter is the read-side hook into vulos-cloud's CLOUD-REGION-01 georoute.
// The cloud's georouter polls /admin/health to learn which region this Meet
// box serves; from there it picks the closest box for a given tenant on the
// token-mint path.
//
// This package only EXPOSES the region; it does NOT decide routing. Routing
// lives in vulos-cloud where the tenant->region map is canonical. Keeping
// the decision there means a single source of truth and lets the SFU stay
// dumb.
type GeoRouter struct {
	region string
}

// NewGeoRouter constructs a router stamping `region` onto health responses.
// region MUST be non-empty; an empty region would let a misconfigured box
// drift into a "global" pool the cloud cannot reason about.
func NewGeoRouter(region string) (*GeoRouter, error) {
	if region == "" {
		return nil, errors.New("vulos-meet: georouter requires a non-empty region")
	}
	return &GeoRouter{region: region}, nil
}

// Region returns the region string this box advertises.
func (g *GeoRouter) Region() string { return g.region }
