// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

package wrap

import (
	"crypto/subtle"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/livekit/protocol/livekit"
)

// Storage seam (Vulos OS gateway → vulos-meet).
//
// The Vulos OS gateway injects a per-request, per-user object-storage
// destination on requests it proxies to vulos-meet. When these headers are
// present, recording/egress artifacts must land in the shared per-user bucket
// (under the meet/ key-space) instead of whatever storage the egress request
// originally named. When ABSENT (the endpoint header is empty/missing), the
// box is NOT behind the gateway and we fall back to the existing egress storage
// config untouched — that fallback is the contract's explicit signal.
//
// These names are pinned by the OS gateway; do not rename without coordinating
// the gateway side.
const (
	StorageEndpointHeader     = "X-Vulos-Storage-Endpoint"      // S3 URL; empty/absent ⇒ fall back
	StorageBucketHeader       = "X-Vulos-Storage-Bucket"        // shared per-user bucket
	StoragePrefixHeader       = "X-Vulos-Storage-Prefix"        // per-user key prefix
	StorageRegionHeader       = "X-Vulos-Storage-Region"        // S3 region
	StorageAccessKeyHeader    = "X-Vulos-Storage-Access-Key"    // short-lived access key
	StorageSecretKeyHeader    = "X-Vulos-Storage-Secret-Key"    // short-lived secret
	StorageSessionTokenHeader = "X-Vulos-Storage-Session-Token" // optional STS session token

	// StorageBrokerAuthHeader carries the shared broker secret the OS gateway
	// presents to prove that it (and not some other on-box caller) injected the
	// X-Vulos-Storage-* headers. The seam is honored ONLY when this matches the
	// configured StorageBrokerSecretEnv value (constant-time). Mirrors lilmail's
	// X-Vulos-Broker-Auth mail-broker gate.
	StorageBrokerAuthHeader = "X-Vulos-Storage-Broker-Auth"
)

// StorageBrokerSecretEnv is the env var that gates the whole storage seam. When
// empty, the gate is closed: injected X-Vulos-Storage-* headers are ignored and
// egress is forwarded verbatim (the legacy/self-host contract). The OS gateway
// sets the matching X-Vulos-Storage-Broker-Auth header on requests it proxies.
const StorageBrokerSecretEnv = "VULOS_STORAGE_BROKER_SECRET"

// storageBrokerAuthorized reports whether a request is allowed to drive the
// storage seam: the secret must be configured (non-empty) AND the presented
// X-Vulos-Storage-Broker-Auth header must match it under a constant-time
// compare. Closed gate (unset secret or missing/mismatched header) ⇒ false, so
// the caller ignores the injected headers and forwards egress unchanged.
func storageBrokerAuthorized(secret string, h http.Header) bool {
	if secret == "" {
		return false // gate disabled — never trust injected storage headers
	}
	presented := strings.TrimSpace(h.Get(StorageBrokerAuthHeader))
	if presented == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(secret)) == 1
}

// StorageSpace is the per-product key-space all Meet artifacts are filed under
// inside the shared per-user bucket. The contract reserves meet/ for this box.
const StorageSpace = "meet/"

// StorageSeam is the resolved, per-request object-storage destination injected
// by the OS gateway. A zero StorageSeam means "no seam present" — see
// StorageSeamFromHeader.
type StorageSeam struct {
	Endpoint     string
	Bucket       string
	Prefix       string
	Region       string
	AccessKey    string
	SecretKey    string
	SessionToken string
}

// StorageSeamFromHeader extracts the injected storage seam from a request's
// headers. ok is false (and the returned seam is the zero value) when the
// gateway did not inject an endpoint: that is the contract's signal to fall
// back to Meet's existing egress storage config. We key the present/absent
// decision on the endpoint header alone because it is the one field with no
// sensible default — a seam without an endpoint cannot address a bucket.
func StorageSeamFromHeader(h http.Header) (StorageSeam, bool) {
	endpoint := strings.TrimSpace(h.Get(StorageEndpointHeader))
	if endpoint == "" {
		return StorageSeam{}, false
	}
	return StorageSeam{
		Endpoint:     endpoint,
		Bucket:       strings.TrimSpace(h.Get(StorageBucketHeader)),
		Prefix:       strings.TrimSpace(h.Get(StoragePrefixHeader)),
		Region:       strings.TrimSpace(h.Get(StorageRegionHeader)),
		AccessKey:    strings.TrimSpace(h.Get(StorageAccessKeyHeader)),
		SecretKey:    strings.TrimSpace(h.Get(StorageSecretKeyHeader)),
		SessionToken: strings.TrimSpace(h.Get(StorageSessionTokenHeader)),
	}, true
}

// KeyPrefix returns the object-key prefix all Meet artifacts are filed under:
// the gateway-supplied per-user prefix joined with the meet/ key-space, always
// terminating in a single slash. Empty per-user prefix ⇒ just "meet/".
func (s StorageSeam) KeyPrefix() string {
	p := strings.Trim(s.Prefix, "/")
	if p == "" {
		return StorageSpace
	}
	return p + "/" + StorageSpace
}

// endpointAllowed reports whether the injected S3 endpoint is safe to ship the
// (short-lived) credentials to. We require https:// for any public host so the
// keys never cross the wire in plaintext; plain http:// is tolerated only for a
// loopback or private-network host (local dev / in-cluster MinIO). An
// unparseable or hostless endpoint is rejected.
func (s StorageSeam) endpointAllowed() bool {
	u, err := url.Parse(s.Endpoint)
	if err != nil || u.Host == "" {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		return isLoopbackOrPrivateHost(u.Hostname())
	default:
		return false
	}
}

// isLoopbackOrPrivateHost reports whether host is "localhost" or an IP literal
// on a loopback, RFC1918/ULA private, or link-local network. A non-IP, non-
// localhost hostname over plain http is treated as public (not allowed).
func isLoopbackOrPrivateHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

// s3Upload builds a LiveKit S3 upload destination addressing the injected
// shared bucket with the injected (short-lived) credentials.
func (s StorageSeam) s3Upload() *livekit.S3Upload {
	return &livekit.S3Upload{
		AccessKey:    s.AccessKey,
		Secret:       s.SecretKey,
		SessionToken: s.SessionToken,
		Region:       s.Region,
		Endpoint:     s.Endpoint,
		Bucket:       s.Bucket,
	}
}

// prefixKey prepends the seam key-space prefix to an output's path. A leading
// slash on the original path is stripped so the result is always relative to
// the per-user prefix (an absolute-looking path must not escape the key-space).
func (s StorageSeam) prefixKey(path string) string {
	return s.KeyPrefix() + strings.TrimPrefix(path, "/")
}
