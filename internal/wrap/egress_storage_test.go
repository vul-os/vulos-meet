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
