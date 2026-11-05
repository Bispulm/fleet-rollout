package api_test

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/Bispulm/fleet-rollout/internal/apiv1"
	"github.com/Bispulm/fleet-rollout/internal/rollout"
	"github.com/Bispulm/fleet-rollout/internal/signing"
	"github.com/Bispulm/fleet-rollout/internal/signing/signingtest"
)

const testChunkSize = 1024

func testContent(version string) []byte {
	return bytes.Repeat([]byte("content of "+version+"\n"), 1000)
}

func testMeta(version string, release int64) signing.Meta {
	return signing.Meta{Name: "demo-app", Version: version, Platform: "linux/arm64", ReleaseCounter: release, ChunkSize: testChunkSize}
}

// signedRequest returns a registration request signed with the trusted key.
func (h harness) signedRequest(version string, release int64) apiv1.CreateArtifactRequest {
	h.t.Helper()
	_, envelope := signingtest.Signed(h.t, h.signer, testMeta(version, release), testContent(version))
	return apiv1.CreateArtifactRequest{Envelope: envelope}
}

// upload sends content, and returns the status and raw body.
func (h harness) upload(id string, content []byte) (int, []byte) {
	h.t.Helper()
	req, err := http.NewRequestWithContext(h.t.Context(), http.MethodPut, h.base+"/v1/artifacts/"+id+"/content", bytes.NewReader(content))
	if err != nil {
		h.t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testAdminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("upload: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("read response: %v", err)
	}
	return resp.StatusCode, raw
}

// createArtifact registers a signed artifact and uploads its content.
func (h harness) createArtifact() apiv1.Artifact {
	h.t.Helper()
	var artifact apiv1.Artifact
	h.expect(http.StatusCreated, http.MethodPost, "/v1/artifacts", h.signedRequest("1.4.0", 14), &artifact)
	if status, raw := h.upload(artifact.ID, testContent("1.4.0")); status != http.StatusNoContent {
		h.t.Fatalf("upload: status = %d, body: %s", status, raw)
	}
	artifact.ContentReady = true
	return artifact
}

func TestRegisterASignedArtifact(t *testing.T) {
	h := newHarness(t)
	var created apiv1.Artifact
	h.expect(http.StatusCreated, http.MethodPost, "/v1/artifacts", h.signedRequest("1.4.0", 14), &created)
	if created.ID == "" || created.Name != "demo-app" || created.ReleaseCounter != 14 || created.ChunkSize != testChunkSize {
		t.Fatalf("created = %+v", created)
	}
	if created.ContentReady {
		t.Fatal("content is ready before any was uploaded")
	}

	var again apiv1.Artifact
	h.expect(http.StatusOK, http.MethodPost, "/v1/artifacts", h.signedRequest("1.4.0", 14), &again)
	if again.ID != created.ID {
		t.Fatalf("a retry returned %s, want %s", again.ID, created.ID)
	}

	var got apiv1.Artifact
	h.expect(http.StatusOK, http.MethodGet, "/v1/artifacts/"+created.ID, nil, &got)
	if got.SHA256 != created.SHA256 {
		t.Fatalf("got = %+v", got)
	}
	h.problem(http.StatusNotFound, http.MethodGet, "/v1/artifacts/00000000-0000-4000-8000-000000000000", nil)
	h.problem(http.StatusNotFound, http.MethodGet, "/v1/artifacts/not-a-uuid", nil)
}

func TestReusingAVersionForDifferentContentConflicts(t *testing.T) {
	h := newHarness(t)
	h.expect(http.StatusCreated, http.MethodPost, "/v1/artifacts", h.signedRequest("1.4.0", 14), nil)
	_, other := signingtest.Signed(t, h.signer, testMeta("1.4.0", 14), []byte(strings.Repeat("different", 500)))
	p := h.problem(http.StatusConflict, http.MethodPost, "/v1/artifacts", apiv1.CreateArtifactRequest{Envelope: other})
	if p.Code != apiv1.CodeConflict {
		t.Fatalf("code = %q", p.Code)
	}
	h.problem(http.StatusConflict, http.MethodPost, "/v1/artifacts", h.signedRequest("1.5.0", 14))
}

func TestRegistrationRejectsAnythingNotSignedByATrustedKey(t *testing.T) {
	h := newHarness(t)
	stranger := signingtest.NewKey(t)
	_, foreign := signingtest.Signed(t, stranger, testMeta("1.4.0", 14), testContent("1.4.0"))
	good := h.signedRequest("1.4.0", 14)

	tampered := good.Envelope
	payload, err := base64.StdEncoding.DecodeString(tampered.Payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	changed := bytes.Replace(payload, []byte(`"release_counter":14`), []byte(`"release_counter":99`), 1)
	tampered.Payload = base64.StdEncoding.EncodeToString(changed)

	cases := map[string]signing.Envelope{
		"another key": foreign,
		"unsigned":    {Payload: good.Envelope.Payload, PayloadType: good.Envelope.PayloadType},
		"tampered":    tampered,
	}
	for name, envelope := range cases {
		t.Run(name, func(t *testing.T) {
			p := h.on(t).problem(http.StatusUnprocessableEntity, http.MethodPost, "/v1/artifacts", apiv1.CreateArtifactRequest{Envelope: envelope})
			if p.Code != apiv1.CodeInvalidSignature {
				t.Fatalf("code = %q", p.Code)
			}
		})
	}
}

func TestRegistrationRejectsAManifestWithAValidSignatureThatDoesNotAddUp(t *testing.T) {
	h := newHarness(t)
	manifest, _ := signingtest.Signed(t, h.signer, testMeta("1.4.0", 14), testContent("1.4.0"))
	manifest.Chunks = manifest.Chunks[:1]
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	envelope := signing.Envelope{
		Payload:     base64.StdEncoding.EncodeToString(encoded),
		PayloadType: signing.PayloadType,
		Signatures: []signing.Signature{{
			Sig: base64.StdEncoding.EncodeToString(ed25519.Sign(h.signer, signing.PAE(signing.PayloadType, encoded))),
		}},
	}
	p := h.problem(http.StatusUnprocessableEntity, http.MethodPost, "/v1/artifacts", apiv1.CreateArtifactRequest{Envelope: envelope})
	if p.Code != apiv1.CodeInvalidManifest {
		t.Fatalf("code = %q", p.Code)
	}
}

func TestRegistrationEnforcesTheArtifactSizeLimit(t *testing.T) {
	h := newHarnessWithLimit(t, 1000)
	p := h.problem(http.StatusUnprocessableEntity, http.MethodPost, "/v1/artifacts", h.signedRequest("1.4.0", 14))
	if len(p.Violations) != 1 || !strings.Contains(p.Violations[0].Message, "limit") {
		t.Fatalf("problem = %+v", p)
	}
}

func TestUploadRequiresTheExactBytes(t *testing.T) {
	h := newHarness(t)
	var created apiv1.Artifact
	h.expect(http.StatusCreated, http.MethodPost, "/v1/artifacts", h.signedRequest("1.4.0", 14), &created)
	content := testContent("1.4.0")
	corrupted := bytes.Clone(content)
	corrupted[2000] ^= 0xff

	for name, body := range map[string][]byte{
		"corrupted": corrupted,
		"truncated": content[:len(content)-1],
		"extended":  append(bytes.Clone(content), 'x'),
		"empty":     nil,
	} {
		t.Run(name, func(t *testing.T) {
			status, raw := h.on(t).upload(created.ID, body)
			if status != http.StatusUnprocessableEntity || !strings.Contains(string(raw), apiv1.CodeContentMismatch) {
				t.Fatalf("status = %d, body: %s", status, raw)
			}
		})
	}
	var got apiv1.Artifact
	h.expect(http.StatusOK, http.MethodGet, "/v1/artifacts/"+created.ID, nil, &got)
	if got.ContentReady {
		t.Fatal("bad uploads marked the content as ready")
	}

	if status, raw := h.upload(created.ID, content); status != http.StatusNoContent {
		t.Fatalf("good upload: status = %d, body: %s", status, raw)
	}
	h.expect(http.StatusOK, http.MethodGet, "/v1/artifacts/"+created.ID, nil, &got)
	if !got.ContentReady {
		t.Fatal("the content isn't marked as ready")
	}

	// Repeating a finished upload is harmless, and can't replace what's stored.
	if status, raw := h.upload(created.ID, corrupted); status != http.StatusNoContent {
		t.Fatalf("repeat: status = %d, body: %s", status, raw)
	}
}

func TestUploadChecksItsTarget(t *testing.T) {
	h := newHarness(t)
	if status, _ := h.upload("00000000-0000-4000-8000-000000000000", []byte("x")); status != http.StatusNotFound {
		t.Fatalf("unknown artifact: status = %d", status)
	}
	if status, _ := h.upload("not-a-uuid", []byte("x")); status != http.StatusNotFound {
		t.Fatalf("malformed ID: status = %d", status)
	}
}

// deviceResponse is what a device got back. The body is already read and closed.
type deviceResponse struct {
	StatusCode int
	Header     http.Header
}

// deviceGet runs a GET for a device, with optional Range and If-Range headers.
func (h harness) deviceGet(token, path, rangeHeader, ifRange string) (deviceResponse, []byte) {
	h.t.Helper()
	req, err := http.NewRequestWithContext(h.t.Context(), http.MethodGet, h.base+path, http.NoBody)
	if err != nil {
		h.t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	if ifRange != "" {
		req.Header.Set("If-Range", ifRange)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("read: %v", err)
	}
	return deviceResponse{StatusCode: resp.StatusCode, Header: resp.Header}, raw
}

func TestADeviceFetchesTheManifestItCanVerify(t *testing.T) {
	f := newDeviceFixture(t)
	a := f.poll(f.first).Assignment
	if a.Kind != "forward" {
		t.Fatalf("assignment = %+v", a)
	}
	resp, raw := f.deviceGet(f.first, "/v1/device/artifacts/"+a.Artifact.ID+"/manifest", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body: %s", resp.StatusCode, raw)
	}
	envelope, err := signing.ParseEnvelope(raw)
	if err != nil {
		t.Fatalf("ParseEnvelope: %v", err)
	}
	manifest, err := signingtest.Trust(t, f.signer).Verify(envelope)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if manifest.SHA256 != a.Artifact.SHA256 || manifest.ReleaseCounter != a.Artifact.ReleaseCounter {
		t.Fatalf("manifest = %+v", manifest)
	}
}

func TestDownloadsSupportRangesAndNameTheirContent(t *testing.T) {
	f := newDeviceFixture(t)
	a := f.poll(f.first).Assignment
	path := "/v1/device/artifacts/" + a.Artifact.ID + "/content"
	content := testContent("1.4.0")
	etag := `"sha256:` + a.Artifact.SHA256 + `"`

	full, raw := f.deviceGet(f.first, path, "", "")
	if full.StatusCode != http.StatusOK || !bytes.Equal(raw, content) {
		t.Fatalf("full download: status = %d, %d bytes", full.StatusCode, len(raw))
	}
	if got := full.Header.Get("ETag"); got != etag {
		t.Fatalf("ETag = %q, want %q", got, etag)
	}

	ranged, raw := f.deviceGet(f.first, path, "bytes=1024-2047", "")
	if ranged.StatusCode != http.StatusPartialContent || !bytes.Equal(raw, content[1024:2048]) {
		t.Fatalf("range: status = %d, %d bytes", ranged.StatusCode, len(raw))
	}
	want := fmt.Sprintf("bytes 1024-2047/%d", len(content))
	if got := ranged.Header.Get("Content-Range"); got != want {
		t.Fatalf("Content-Range = %q, want %q", got, want)
	}
	if got := ranged.Header.Get("ETag"); got != etag {
		t.Fatalf("ETag on a range = %q", got)
	}

	matching, _ := f.deviceGet(f.first, path, "bytes=0-9", etag)
	if matching.StatusCode != http.StatusPartialContent {
		t.Fatalf("If-Range matching: status = %d", matching.StatusCode)
	}
	stale, raw := f.deviceGet(f.first, path, "bytes=0-9", `"sha256:0000"`)
	if stale.StatusCode != http.StatusOK || !bytes.Equal(raw, content) {
		t.Fatalf("If-Range stale: status = %d, want the full content", stale.StatusCode)
	}

	beyond, _ := f.deviceGet(f.first, path, fmt.Sprintf("bytes=%d-", len(content)), "")
	if beyond.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("range past the end: status = %d", beyond.StatusCode)
	}
}

func TestOnlyADeviceWithAnOpenAssignmentCanFetchAnArtifact(t *testing.T) {
	f := newDeviceFixture(t)
	a := f.poll(f.first).Assignment
	content := "/v1/device/artifacts/" + a.Artifact.ID + "/content"
	manifest := "/v1/device/artifacts/" + a.Artifact.ID + "/manifest"

	// A later-stage device has no assignment yet.
	for _, path := range []string{content, manifest} {
		resp, raw := f.deviceGet(f.later, path, "", "")
		if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(raw), apiv1.CodeNoOpenAssignment) {
			t.Fatalf("%s for an unassigned device: status = %d, body: %s", path, resp.StatusCode, raw)
		}
	}

	// Neither credential works on the other's routes.
	if resp, _ := f.deviceGet(testAdminToken, content, "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("admin token: status = %d", resp.StatusCode)
	}
	if status, _ := f.doWithAuth("Bearer "+f.first, http.MethodPut, "/v1/artifacts/"+a.Artifact.ID+"/content", "x"); status != http.StatusUnauthorized {
		t.Fatalf("device token on an upload: status = %d", status)
	}

	// Cancelling the assignment also cuts off the download.
	if resp, _ := f.deviceGet(f.first, content, "bytes=0-9", ""); resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("before the abort: status = %d", resp.StatusCode)
	}
	f.expect(http.StatusOK, http.MethodPost, "/v1/rollouts/"+f.rollout.ID+"/abort", nil, nil)
	if resp, _ := f.deviceGet(f.first, content, "bytes=0-9", ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("after the abort: status = %d", resp.StatusCode)
	}
}

func TestPausedDevicesKeepAccessButFinishedOnesDoNot(t *testing.T) {
	f := newDeviceFixture(t)
	a := f.poll(f.first).Assignment
	path := "/v1/device/artifacts/" + a.Artifact.ID + "/content"
	f.expect(http.StatusOK, http.MethodPost, "/v1/rollouts/"+f.rollout.ID+"/pause", nil, nil)
	if resp, _ := f.deviceGet(f.first, path, "bytes=0-9", ""); resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("during a pause: status = %d", resp.StatusCode)
	}
	if status, raw := f.report(f.first, reportFor(a, 1, rollout.AssignmentFailed)); status != http.StatusOK {
		t.Fatalf("report: status = %d, body: %s", status, raw)
	}
	if resp, _ := f.deviceGet(f.first, path, "bytes=0-9", ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("after finishing: status = %d", resp.StatusCode)
	}
}

func TestArtifactsCanBeFoundByNamePlatformAndVersion(t *testing.T) {
	h := newHarness(t)
	a := h.createArtifact()
	query := url.Values{"name": {a.Name}, "platform": {a.Platform}, "version": {a.Version}}

	var found apiv1.ArtifactList
	h.expect(http.StatusOK, http.MethodGet, "/v1/artifacts?"+query.Encode(), nil, &found)
	if len(found.Artifacts) != 1 || found.Artifacts[0].ID != a.ID {
		t.Fatalf("found = %+v, want %s", found, a.ID)
	}
	query.Set("version", "9.9.9")
	h.expect(http.StatusOK, http.MethodGet, "/v1/artifacts?"+query.Encode(), nil, &found)
	if len(found.Artifacts) != 0 {
		t.Fatalf("found = %+v for a version that doesn't exist", found)
	}

	for name, q := range map[string]url.Values{
		"no name":      {"platform": {a.Platform}, "version": {a.Version}},
		"bad platform": {"name": {a.Name}, "platform": {"arm64"}, "version": {a.Version}},
		"no version":   {"name": {a.Name}, "platform": {a.Platform}},
	} {
		if status, raw := h.do(http.MethodGet, "/v1/artifacts?"+q.Encode(), nil); status != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, body: %s", name, status, raw)
		}
	}
	if status, _ := h.doWithAuth("", http.MethodGet, "/v1/artifacts?"+query.Encode(), nil); status != http.StatusUnauthorized {
		t.Errorf("without a token: status = %d", status)
	}
}
