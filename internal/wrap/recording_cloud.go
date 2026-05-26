// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

package wrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// CloudBlobDeleter is the FLAGGED external seam for recording-blob deletion.
//
// WHY THIS IS A SEAM AND NOT A FULL IMPLEMENTATION:
//
//	The recording blob (the actual .mp4/.ogg) is owned by the vulos-cloud
//	MEET-RECORDING-01 sink (S3/Tigris). vulos-meet is the LiveKit-talking
//	surface and the policy/lifecycle owner, but it NEVER holds the bytes and
//	MUST NOT hold the S3 credentials (keeping object-store creds off the SFU
//	box is a deliberate blast-radius decision). So the actual DeleteObject is
//	performed by the cloud, behind an authenticated control endpoint.
//
//	This deleter issues a DELETE request to that cloud endpoint. The CLOUD side
//	(MEET-RECORDING-01) must implement the receiving handler: authenticate the
//	bearer, map (tenant, egress_id) to the stored object key, and issue the S3
//	DeleteObject. That handler is the genuinely-external piece. The contract is
//	pinned below (CloudDeleteRequest) so the cloud wire-up is mechanical.
//
// Contract (vulos-meet → cloud):
//
//	DELETE  {BaseURL}/v1/recordings/{egress_id}
//	Header  Authorization: Bearer <AuthTok>   (same token family as the egress
//	                                            forward leg; cloud rotates its
//	                                            inbound auth independently)
//	Header  X-Vulos-Tenant: <tenant>
//	Body    JSON CloudDeleteRequest (tenant/room/egress_id) — redundant with the
//	        path+header so the cloud can authorise without parsing the path.
//
//	Cloud responses treated as "blob gone" (advance ledger to Deleted):
//	    200/202/204  — deleted (or accepted for async delete)
//	    404          — already absent (idempotent delete is success)
//	Any other status / transport error → leave the recording Expired; the
//	driver retries on the next sweep. Deletion is at-least-once.
type CloudBlobDeleter struct {
	baseURL string
	authTok string
	httpc   *http.Client
}

// CloudDeleteRequest is the JSON body sent to the cloud delete endpoint. Narrow
// by design — the cloud already filed the recording against (tenant, egress_id)
// when it received the egress webhook envelope, so this is enough to locate it.
type CloudDeleteRequest struct {
	Schema   string `json:"schema"` // "vulos-meet/recording-delete/v1"
	Tenant   string `json:"tenant"`
	Room     string `json:"room"`
	EgressID string `json:"egress_id"`
}

// CloudDeleteSchema is the schema tag on the delete request body.
const CloudDeleteSchema = "vulos-meet/recording-delete/v1"

// NewCloudBlobDeleter builds the deleter targeting the cloud recording sink's
// delete control endpoint. baseURL is the same MEET-RECORDING-01 base the
// egress receiver forwards to (without the /webhook suffix); authTok is the
// bearer for the delete leg (env MEET_RECORDING_CLOUD_TOKEN by convention, the
// same family the forward leg uses). A nil httpc defaults to a 10s client.
func NewCloudBlobDeleter(baseURL, authTok string, httpc *http.Client) (*CloudBlobDeleter, error) {
	if baseURL == "" {
		return nil, errors.New("vulos-meet: cloud blob deleter requires a base url")
	}
	if httpc == nil {
		httpc = &http.Client{Timeout: 10 * time.Second}
	}
	return &CloudBlobDeleter{baseURL: baseURL, authTok: authTok, httpc: httpc}, nil
}

// Delete issues the DELETE against the cloud sink. See the type doc for the
// success/retry contract. Returns nil only when the cloud confirms the blob is
// gone (or was already absent).
func (d *CloudBlobDeleter) Delete(ctx context.Context, r Recording) error {
	if r.EgressID == "" {
		return errors.New("vulos-meet: cloud delete requires an egress id")
	}
	body, err := json.Marshal(CloudDeleteRequest{
		Schema:   CloudDeleteSchema,
		Tenant:   r.Tenant,
		Room:     r.Room,
		EgressID: r.EgressID,
	})
	if err != nil {
		return err
	}
	url := d.baseURL + "/v1/recordings/" + r.EgressID
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Vulos-Schema", CloudDeleteSchema)
	req.Header.Set("X-Vulos-Tenant", r.Tenant)
	if d.authTok != "" {
		req.Header.Set("Authorization", "Bearer "+d.authTok)
	}
	resp, err := d.httpc.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusAccepted, http.StatusNoContent, http.StatusNotFound:
		// 404 = already absent; idempotent delete is success.
		return nil
	default:
		return fmt.Errorf("vulos-meet: cloud recording delete returned %d", resp.StatusCode)
	}
}
