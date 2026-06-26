// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

package wrap

import (
	"fmt"
	"strings"

	"github.com/livekit/protocol/livekit"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// rewriteEgressBodyForSeam redirects the object-storage destination of a Twirp
// egress Start request to the injected shared per-user bucket (under meet/).
//
// This is the one place in the box where Meet "configures" recording/egress
// output: vulos-meet is the sole LiveKit-talking surface, so the cloud's
// StartRoomCompositeEgress (and siblings) flow through the egress proxy on
// their way to livekit-server. When the OS gateway has injected a storage seam,
// we parse the request, repoint every file/segment/image output at the seam's
// S3 bucket + credentials, and file the keys under the per-user meet/ prefix.
// Stream outputs (RTMP/SRT) are left alone — they are not storage artifacts.
//
// Behaviour:
//   - method that is not a Start*Egress request → (body, false, nil): no-op.
//   - request carries no rewritable storage output (e.g. stream-only) →
//     (body, false, nil): nothing to redirect, forward verbatim.
//   - decode/encode failure → (nil, false, err): the caller must FAIL the
//     request rather than silently store to the wrong (original) bucket.
//   - otherwise → (rewritten, true, nil).
//
// The seam is only consulted when present (see StorageSeamFromHeader); absent,
// the egress proxy never calls this and the original body is forwarded as-is.
func rewriteEgressBodyForSeam(method, contentType string, body []byte, seam StorageSeam) ([]byte, bool, error) {
	msg := egressRequestForMethod(method)
	if msg == nil {
		return body, false, nil // not a storage-bearing Start request
	}
	useJSON := bodyIsJSON(contentType)
	if useJSON {
		if err := protojson.Unmarshal(body, msg); err != nil {
			return nil, false, fmt.Errorf("vulos-meet: decode egress %s (json): %w", method, err)
		}
	} else {
		if err := proto.Unmarshal(body, msg); err != nil {
			return nil, false, fmt.Errorf("vulos-meet: decode egress %s (proto): %w", method, err)
		}
	}

	if !applySeamToEgress(msg, seam) {
		return body, false, nil // no storage output to redirect
	}

	var out []byte
	var err error
	if useJSON {
		out, err = protojson.Marshal(msg)
	} else {
		out, err = proto.Marshal(msg)
	}
	if err != nil {
		return nil, false, fmt.Errorf("vulos-meet: re-encode egress %s: %w", method, err)
	}
	return out, true, nil
}

// egressRequestForMethod maps a Twirp egress method name to a fresh request
// message of the right type, or nil for methods that carry no storage output
// (StopEgress, ListEgress, UpdateLayout, UpdateStream).
func egressRequestForMethod(method string) proto.Message {
	switch method {
	case "StartRoomCompositeEgress":
		return &livekit.RoomCompositeEgressRequest{}
	case "StartWebEgress":
		return &livekit.WebEgressRequest{}
	case "StartParticipantEgress":
		return &livekit.ParticipantEgressRequest{}
	case "StartTrackCompositeEgress":
		return &livekit.TrackCompositeEgressRequest{}
	case "StartTrackEgress":
		return &livekit.TrackEgressRequest{}
	default:
		return nil
	}
}

// bodyIsJSON reports whether a Twirp body is proto-JSON (Content-Type
// application/json) vs protobuf binary. Twirp uses exactly these two encodings;
// anything not JSON is treated as protobuf (the Twirp default).
func bodyIsJSON(contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "json")
}

// applySeamToEgress redirects every storage output on the request to the seam.
// Returns true when at least one output was rewritten.
func applySeamToEgress(msg proto.Message, seam StorageSeam) bool {
	switch r := msg.(type) {
	case *livekit.RoomCompositeEgressRequest:
		changed := applyEncodedFile(r.GetFile(), seam) // deprecated single oneof
		changed = applySegmented(r.GetSegments(), seam) || changed
		changed = applyEncodedFiles(r.FileOutputs, seam) || changed
		changed = applySegments(r.SegmentOutputs, seam) || changed
		changed = applyImages(r.ImageOutputs, seam) || changed
		return changed
	case *livekit.WebEgressRequest:
		changed := applyEncodedFile(r.GetFile(), seam)
		changed = applySegmented(r.GetSegments(), seam) || changed
		changed = applyEncodedFiles(r.FileOutputs, seam) || changed
		changed = applySegments(r.SegmentOutputs, seam) || changed
		changed = applyImages(r.ImageOutputs, seam) || changed
		return changed
	case *livekit.ParticipantEgressRequest:
		changed := applyEncodedFiles(r.FileOutputs, seam)
		changed = applySegments(r.SegmentOutputs, seam) || changed
		changed = applyImages(r.ImageOutputs, seam) || changed
		return changed
	case *livekit.TrackCompositeEgressRequest:
		changed := applyEncodedFile(r.GetFile(), seam)
		changed = applySegmented(r.GetSegments(), seam) || changed
		changed = applyEncodedFiles(r.FileOutputs, seam) || changed
		changed = applySegments(r.SegmentOutputs, seam) || changed
		changed = applyImages(r.ImageOutputs, seam) || changed
		return changed
	case *livekit.TrackEgressRequest:
		return applyDirectFile(r.GetFile(), seam)
	default:
		return false
	}
}

// applyEncodedFile repoints one EncodedFileOutput at the seam's S3 bucket and
// re-files its key under the per-user meet/ prefix. Nil output → no-op.
func applyEncodedFile(o *livekit.EncodedFileOutput, seam StorageSeam) bool {
	if o == nil {
		return false
	}
	o.Filepath = seam.prefixKey(o.Filepath)
	o.Output = &livekit.EncodedFileOutput_S3{S3: seam.s3Upload()}
	return true
}

func applyEncodedFiles(os []*livekit.EncodedFileOutput, seam StorageSeam) bool {
	changed := false
	for _, o := range os {
		changed = applyEncodedFile(o, seam) || changed
	}
	return changed
}

// applySegmented repoints one SegmentedFileOutput (HLS) at the seam. All three
// path fields (segment prefix + both playlist names) are re-filed under meet/.
func applySegmented(o *livekit.SegmentedFileOutput, seam StorageSeam) bool {
	if o == nil {
		return false
	}
	o.FilenamePrefix = seam.prefixKey(o.FilenamePrefix)
	if o.PlaylistName != "" {
		o.PlaylistName = seam.prefixKey(o.PlaylistName)
	}
	if o.LivePlaylistName != "" {
		o.LivePlaylistName = seam.prefixKey(o.LivePlaylistName)
	}
	o.Output = &livekit.SegmentedFileOutput_S3{S3: seam.s3Upload()}
	return true
}

func applySegments(os []*livekit.SegmentedFileOutput, seam StorageSeam) bool {
	changed := false
	for _, o := range os {
		changed = applySegmented(o, seam) || changed
	}
	return changed
}

// applyImages repoints thumbnail/image outputs at the seam.
func applyImages(os []*livekit.ImageOutput, seam StorageSeam) bool {
	changed := false
	for _, o := range os {
		if o == nil {
			continue
		}
		o.FilenamePrefix = seam.prefixKey(o.FilenamePrefix)
		o.Output = &livekit.ImageOutput_S3{S3: seam.s3Upload()}
		changed = true
	}
	return changed
}

// applyDirectFile repoints a TrackEgress DirectFileOutput at the seam. A
// websocket-stream track egress (the other oneof arm) has no file output, so
// GetFile() is nil and this is a no-op.
func applyDirectFile(o *livekit.DirectFileOutput, seam StorageSeam) bool {
	if o == nil {
		return false
	}
	o.Filepath = seam.prefixKey(o.Filepath)
	o.Output = &livekit.DirectFileOutput_S3{S3: seam.s3Upload()}
	return true
}
