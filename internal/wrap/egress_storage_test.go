// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

package wrap

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"
	"google.golang.org/protobuf/encoding/protojson"
)

func testSeam() StorageSeam {
	return StorageSeam{
		Endpoint:     "https://s3.vulos.test",
		Bucket:       "user-shared",
		Prefix:       "users/u123",
		Region:       "za-jhb",
		AccessKey:    "AKIA_TEST",
		SecretKey:    "secret_test",
		SessionToken: "sess_test",
	}
}

func TestStorageSeamFromHeader_AbsentWhenNoEndpoint(t *testing.T) {
	h := http.Header{}
	h.Set(StorageBucketHeader, "b") // bucket without endpoint
	if _, ok := StorageSeamFromHeader(h); ok {
		t.Fatal("seam should be absent when endpoint header is empty")
	}
}

func TestStorageSeamFromHeader_Present(t *testing.T) {
	h := http.Header{}
	h.Set(StorageEndpointHeader, "https://s3.vulos.test")
	h.Set(StorageBucketHeader, "user-shared")
	h.Set(StoragePrefixHeader, "users/u123")
	s, ok := StorageSeamFromHeader(h)
	if !ok {
		t.Fatal("seam should be present when endpoint header is set")
	}
	if s.Bucket != "user-shared" || s.Endpoint != "https://s3.vulos.test" {
		t.Fatalf("unexpected seam: %+v", s)
	}
	if got := s.KeyPrefix(); got != "users/u123/meet/" {
		t.Fatalf("KeyPrefix=%q want users/u123/meet/", got)
	}
}

func TestKeyPrefix_EmptyPrefix(t *testing.T) {
	if got := (StorageSeam{}).KeyPrefix(); got != "meet/" {
		t.Fatalf("KeyPrefix=%q want meet/", got)
	}
}

func TestRewriteEgress_RoomComposite_FileOutputsRedirected(t *testing.T) {
	seam := testSeam()
	req := &livekit.RoomCompositeEgressRequest{
		RoomName: "acme:standup",
		Layout:   "speaker",
		FileOutputs: []*livekit.EncodedFileOutput{{
			Filepath: "recordings/standup.mp4",
			Output: &livekit.EncodedFileOutput_S3{S3: &livekit.S3Upload{
				Bucket:   "cloud-original",
				Endpoint: "https://old.example",
				Secret:   "OLD",
			}},
		}},
	}
	in, err := protojson.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	out, changed, err := rewriteEgressBodyForSeam("StartRoomCompositeEgress", "application/json", in, seam)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true")
	}
	got := &livekit.RoomCompositeEgressRequest{}
	if err := protojson.Unmarshal(out, got); err != nil {
		t.Fatal(err)
	}
	fo := got.FileOutputs[0]
	if fo.Filepath != "users/u123/meet/recordings/standup.mp4" {
		t.Fatalf("filepath=%q not re-prefixed under meet/", fo.Filepath)
	}
	s3 := fo.GetS3()
	if s3 == nil {
		t.Fatal("expected S3 output after rewrite")
	}
	if s3.Bucket != "user-shared" || s3.Endpoint != "https://s3.vulos.test" {
		t.Fatalf("S3 not repointed at seam: %+v", s3)
	}
	if s3.Secret != "secret_test" || s3.AccessKey != "AKIA_TEST" || s3.SessionToken != "sess_test" {
		t.Fatalf("S3 creds not from seam: %+v", s3)
	}
	if s3.Region != "za-jhb" {
		t.Fatalf("S3 region=%q want za-jhb", s3.Region)
	}
}

func TestRewriteEgress_StopEgressUntouched(t *testing.T) {
	body := []byte(`{"egress_id":"EG_x"}`)
	out, changed, err := rewriteEgressBodyForSeam("StopEgress", "application/json", body, testSeam())
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if changed {
		t.Fatal("StopEgress carries no storage output; should be unchanged")
	}
	if string(out) != string(body) {
		t.Fatalf("body mutated: %q", out)
	}
}

func TestRewriteEgress_StreamOnlyUnchanged(t *testing.T) {
	// A stream-only egress has no object-storage artifact: nothing to redirect.
	req := &livekit.RoomCompositeEgressRequest{
		RoomName: "acme:standup",
		StreamOutputs: []*livekit.StreamOutput{{
			Urls: []string{"rtmp://live.example/app/key"},
		}},
	}
	in, _ := protojson.Marshal(req)
	_, changed, err := rewriteEgressBodyForSeam("StartRoomCompositeEgress", "application/json", in, testSeam())
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if changed {
		t.Fatal("stream-only egress should not be rewritten")
	}
}

func TestRewriteEgress_TrackEgressDirectFile(t *testing.T) {
	req := &livekit.TrackEgressRequest{
		RoomName: "acme:standup",
		TrackId:  "TR_1",
		Output: &livekit.TrackEgressRequest_File{File: &livekit.DirectFileOutput{
			Filepath: "tracks/audio.ogg",
			Output:   &livekit.DirectFileOutput_S3{S3: &livekit.S3Upload{Bucket: "old"}},
		}},
	}
	in, _ := protojson.Marshal(req)
	out, changed, err := rewriteEgressBodyForSeam("StartTrackEgress", "application/json", in, testSeam())
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if !changed {
		t.Fatal("expected direct file output to be redirected")
	}
	got := &livekit.TrackEgressRequest{}
	if err := protojson.Unmarshal(out, got); err != nil {
		t.Fatal(err)
	}
	df := got.GetFile()
	if df.Filepath != "users/u123/meet/tracks/audio.ogg" {
		t.Fatalf("filepath=%q", df.Filepath)
	}
	if df.GetS3().Bucket != "user-shared" {
		t.Fatalf("S3 not repointed: %+v", df.GetS3())
	}
}

func TestRewriteEgress_DecodeFailureErrors(t *testing.T) {
	_, _, err := rewriteEgressBodyForSeam("StartRoomCompositeEgress", "application/json", []byte("not json"), testSeam())
	if err == nil {
		t.Fatal("expected decode error so caller fails the request")
	}
}

// TestEgressProxy_SeamRedirectsThroughProxy is the end-to-end seam path: a
// recording-tokened Start request with injected storage headers reaches the
// upstream with its S3 output repointed at the shared bucket under meet/.
func TestEgressProxy_SeamRedirectsThroughProxy(t *testing.T) {
	t.Setenv(StorageBrokerSecretEnv, "broker-secret-xyz") // gate open
	f := &fakeLiveKitTwirp{}
	upstream := newFakeLiveKitTwirp(t, f)
	defer upstream.Close()
	addr := strings.TrimPrefix(upstream.URL, "http://")

	g, _ := newGateForTest(t, addr)
	p, _ := newEgressProxyForTest(t, addr)
	gate := httptest.NewServer(g.Handler(nil, p))
	defer gate.Close()

	reqMsg := &livekit.RoomCompositeEgressRequest{
		RoomName: "acme:standup",
		FileOutputs: []*livekit.EncodedFileOutput{{
			Filepath: "standup.mp4",
			Output:   &livekit.EncodedFileOutput_S3{S3: &livekit.S3Upload{Bucket: "cloud-original"}},
		}},
	}
	body, _ := protojson.Marshal(reqMsg)

	tok := mintEgressToken(t, "acme", "standup", time.Hour)
	req, _ := http.NewRequest(http.MethodPost, gate.URL+"/twirp/livekit.Egress/StartRoomCompositeEgress", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(StorageBrokerAuthHeader, "broker-secret-xyz") // valid broker auth
	req.Header.Set(StorageEndpointHeader, "https://s3.vulos.test")
	req.Header.Set(StorageBucketHeader, "user-shared")
	req.Header.Set(StoragePrefixHeader, "users/u123")
	req.Header.Set(StorageAccessKeyHeader, "AKIA_TEST")
	req.Header.Set(StorageSecretKeyHeader, "secret_test")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}

	got := &livekit.RoomCompositeEgressRequest{}
	if err := protojson.Unmarshal(f.lastBody, got); err != nil {
		t.Fatalf("upstream body not valid proto-json: %v body=%s", err, f.lastBody)
	}
	fo := got.FileOutputs[0]
	if fo.Filepath != "users/u123/meet/standup.mp4" {
		t.Fatalf("upstream filepath=%q want users/u123/meet/standup.mp4", fo.Filepath)
	}
	if fo.GetS3().Bucket != "user-shared" || fo.GetS3().Endpoint != "https://s3.vulos.test" {
		t.Fatalf("upstream S3 not redirected: %+v", fo.GetS3())
	}
}

// sendSeamEgress fires a recording-tokened Start request through the egress
// proxy with the given storage seam headers, and returns the status code plus
// the body the upstream fake actually received. Used by the broker-auth gate
// tests to assert whether the egress output was rewritten or forwarded verbatim.
func sendSeamEgress(t *testing.T, extraHeaders map[string]string) (int, []byte) {
	t.Helper()
	f := &fakeLiveKitTwirp{}
	upstream := newFakeLiveKitTwirp(t, f)
	defer upstream.Close()
	addr := strings.TrimPrefix(upstream.URL, "http://")

	g, _ := newGateForTest(t, addr)
	p, _ := newEgressProxyForTest(t, addr)
	gate := httptest.NewServer(g.Handler(nil, p))
	defer gate.Close()

	reqMsg := &livekit.RoomCompositeEgressRequest{
		RoomName: "acme:standup",
		FileOutputs: []*livekit.EncodedFileOutput{{
			Filepath: "standup.mp4",
			Output:   &livekit.EncodedFileOutput_S3{S3: &livekit.S3Upload{Bucket: "cloud-original"}},
		}},
	}
	body, _ := protojson.Marshal(reqMsg)

	tok := mintEgressToken(t, "acme", "standup", time.Hour)
	req, _ := http.NewRequest(http.MethodPost, gate.URL+"/twirp/livekit.Egress/StartRoomCompositeEgress", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(StorageEndpointHeader, "https://s3.vulos.test")
	req.Header.Set(StorageBucketHeader, "user-shared")
	req.Header.Set(StoragePrefixHeader, "users/u123")
	req.Header.Set(StorageAccessKeyHeader, "AKIA_TEST")
	req.Header.Set(StorageSecretKeyHeader, "secret_test")
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode, f.lastBody
}

// assertVerbatim parses the upstream body and asserts the egress output was NOT
// rewritten — i.e. the seam was ignored and the original cloud-named bucket and
// un-prefixed path survived.
func assertVerbatim(t *testing.T, upstreamBody []byte) {
	t.Helper()
	got := &livekit.RoomCompositeEgressRequest{}
	if err := protojson.Unmarshal(upstreamBody, got); err != nil {
		t.Fatalf("upstream body not valid proto-json: %v body=%s", err, upstreamBody)
	}
	fo := got.FileOutputs[0]
	if fo.Filepath != "standup.mp4" {
		t.Fatalf("filepath rewritten despite closed gate: %q", fo.Filepath)
	}
	if fo.GetS3().Bucket != "cloud-original" || fo.GetS3().Endpoint != "" {
		t.Fatalf("S3 redirected despite closed gate: %+v", fo.GetS3())
	}
}

// TestEgressProxy_SeamIgnoredWithoutBrokerAuth: the secret IS configured but the
// request omits (or mismatches) X-Vulos-Storage-Broker-Auth → the injected seam
// is ignored and the egress is forwarded verbatim to the original bucket.
func TestEgressProxy_SeamIgnoredWithoutBrokerAuth(t *testing.T) {
	t.Setenv(StorageBrokerSecretEnv, "broker-secret-xyz")

	// (a) no broker-auth header at all.
	status, body := sendSeamEgress(t, nil)
	if status != http.StatusOK {
		t.Fatalf("status=%d want 200", status)
	}
	assertVerbatim(t, body)

	// (b) wrong broker-auth value.
	status, body = sendSeamEgress(t, map[string]string{StorageBrokerAuthHeader: "wrong-secret"})
	if status != http.StatusOK {
		t.Fatalf("status=%d want 200", status)
	}
	assertVerbatim(t, body)
}

// TestEgressProxy_SeamIgnoredWhenSecretUnset: with VULOS_STORAGE_BROKER_SECRET
// unset, the gate is closed even when the request presents a broker-auth header
// — standalone/self-host meet never trusts injected storage headers.
func TestEgressProxy_SeamIgnoredWhenSecretUnset(t *testing.T) {
	t.Setenv(StorageBrokerSecretEnv, "")
	status, body := sendSeamEgress(t, map[string]string{StorageBrokerAuthHeader: "anything"})
	if status != http.StatusOK {
		t.Fatalf("status=%d want 200", status)
	}
	assertVerbatim(t, body)
}

// TestEgressProxy_SeamRewrittenWithBrokerAuth: secret configured AND a matching
// broker-auth header → the seam is honored and the output is redirected.
func TestEgressProxy_SeamRewrittenWithBrokerAuth(t *testing.T) {
	t.Setenv(StorageBrokerSecretEnv, "broker-secret-xyz")
	status, body := sendSeamEgress(t, map[string]string{StorageBrokerAuthHeader: "broker-secret-xyz"})
	if status != http.StatusOK {
		t.Fatalf("status=%d want 200", status)
	}
	got := &livekit.RoomCompositeEgressRequest{}
	if err := protojson.Unmarshal(body, got); err != nil {
		t.Fatalf("upstream body not valid proto-json: %v", err)
	}
	fo := got.FileOutputs[0]
	if fo.Filepath != "users/u123/meet/standup.mp4" {
		t.Fatalf("filepath=%q not rewritten under seam", fo.Filepath)
	}
	if fo.GetS3().Bucket != "user-shared" {
		t.Fatalf("S3 not redirected: %+v", fo.GetS3())
	}
}

// TestEgressProxy_UnsafeEndpointRejected: an authenticated seam whose endpoint
// is a plaintext http public host fails closed (400) rather than shipping the
// short-lived creds in the clear or falling back to the cloud bucket.
func TestEgressProxy_UnsafeEndpointRejected(t *testing.T) {
	t.Setenv(StorageBrokerSecretEnv, "broker-secret-xyz")
	status, _ := sendSeamEgress(t, map[string]string{
		StorageBrokerAuthHeader: "broker-secret-xyz",
		StorageEndpointHeader:   "http://public.example.com",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("status=%d want 400 for unsafe http endpoint", status)
	}
}

// TestEgressProxy_StorageHeadersNotForwardedToChild asserts the defense-in-depth
// strip: the X-Vulos-Storage-* family (broker-auth secret + short-lived storage
// credentials) and the legacy X-Vulos-Broker-Auth name are consumed by vulos-meet
// and MUST NOT reach the loopback livekit-server child, even when the seam is fully
// authorized and the egress body was rewritten.
func TestEgressProxy_StorageHeadersNotForwardedToChild(t *testing.T) {
	t.Setenv(StorageBrokerSecretEnv, "broker-secret-xyz") // gate open
	f := &fakeLiveKitTwirp{}
	upstream := newFakeLiveKitTwirp(t, f)
	defer upstream.Close()
	addr := strings.TrimPrefix(upstream.URL, "http://")

	g, _ := newGateForTest(t, addr)
	p, _ := newEgressProxyForTest(t, addr)
	gate := httptest.NewServer(g.Handler(nil, p))
	defer gate.Close()

	reqMsg := &livekit.RoomCompositeEgressRequest{
		RoomName: "acme:standup",
		FileOutputs: []*livekit.EncodedFileOutput{{
			Filepath: "standup.mp4",
			Output:   &livekit.EncodedFileOutput_S3{S3: &livekit.S3Upload{Bucket: "cloud-original"}},
		}},
	}
	body, _ := protojson.Marshal(reqMsg)

	tok := mintEgressToken(t, "acme", "standup", time.Hour)
	req, _ := http.NewRequest(http.MethodPost, gate.URL+"/twirp/livekit.Egress/StartRoomCompositeEgress", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(StorageBrokerAuthHeader, "broker-secret-xyz")
	req.Header.Set("X-Vulos-Broker-Auth", "broker-secret-xyz") // legacy name
	req.Header.Set(StorageEndpointHeader, "https://s3.vulos.test")
	req.Header.Set(StorageBucketHeader, "user-shared")
	req.Header.Set(StoragePrefixHeader, "users/u123")
	req.Header.Set(StorageRegionHeader, "us-east-1")
	req.Header.Set(StorageAccessKeyHeader, "AKIA_TEST")
	req.Header.Set(StorageSecretKeyHeader, "secret_test")
	req.Header.Set(StorageSessionTokenHeader, "session_test")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}

	// Sanity: the seam WAS honored (body rewritten), so we know the headers were
	// consumed rather than ignored — yet none of them reached the child.
	got := &livekit.RoomCompositeEgressRequest{}
	if err := protojson.Unmarshal(f.lastBody, got); err != nil {
		t.Fatalf("upstream body not valid proto-json: %v", err)
	}
	if got.FileOutputs[0].GetS3().Bucket != "user-shared" {
		t.Fatalf("seam not honored, child body not rewritten: %+v", got.FileOutputs[0].GetS3())
	}

	mustNotForward := []string{
		StorageEndpointHeader, StorageBucketHeader, StoragePrefixHeader,
		StorageRegionHeader, StorageAccessKeyHeader, StorageSecretKeyHeader,
		StorageSessionTokenHeader, StorageBrokerAuthHeader, "X-Vulos-Broker-Auth",
	}
	for _, h := range mustNotForward {
		if v := f.lastHeader.Get(h); v != "" {
			t.Errorf("header %q leaked to livekit-server child: %q", h, v)
		}
	}
	// The bearer (which LiveKit re-verifies) MUST still pass through.
	if f.lastHeader.Get("Authorization") != "Bearer "+tok {
		t.Fatalf("Authorization not forwarded to child: %q", f.lastHeader.Get("Authorization"))
	}
}

func TestStorageBrokerAuthorized(t *testing.T) {
	mk := func(auth string) http.Header {
		h := http.Header{}
		if auth != "" {
			h.Set(StorageBrokerAuthHeader, auth)
		}
		return h
	}
	if storageBrokerAuthorized("", mk("s")) {
		t.Fatal("empty secret must close the gate")
	}
	if storageBrokerAuthorized("s", mk("")) {
		t.Fatal("missing auth header must close the gate")
	}
	if storageBrokerAuthorized("s", mk("other")) {
		t.Fatal("mismatched auth must close the gate")
	}
	if !storageBrokerAuthorized("s", mk("s")) {
		t.Fatal("matching secret+auth must open the gate")
	}
}

func TestEndpointAllowed(t *testing.T) {
	cases := []struct {
		endpoint string
		want     bool
	}{
		{"https://s3.vulos.test", true},
		{"https://s3.amazonaws.com", true},
		{"http://public.example.com", false},
		{"http://localhost:9000", true},
		{"http://127.0.0.1:9000", true},
		{"http://10.0.0.5:9000", true},
		{"http://192.168.1.4:9000", true},
		{"http://172.16.0.9:9000", true},
		{"http://8.8.8.8:9000", false},
		{"ftp://s3.vulos.test", false},
		{"", false},
		{"://bad", false},
	}
	for _, c := range cases {
		if got := (StorageSeam{Endpoint: c.endpoint}).endpointAllowed(); got != c.want {
			t.Errorf("endpointAllowed(%q)=%v want %v", c.endpoint, got, c.want)
		}
	}
}
