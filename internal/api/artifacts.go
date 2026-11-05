package api

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/Bispulm/fleet-rollout/internal/apiv1"
	"github.com/Bispulm/fleet-rollout/internal/rollout"
	"github.com/Bispulm/fleet-rollout/internal/signing"
	"github.com/Bispulm/fleet-rollout/internal/store"
)

const (
	defaultMaxArtifactBytes = 1 << 30
	uploadTimeout           = 30 * time.Minute
	// downloadChunkTimeout bounds serving one Range request, however slow the device.
	downloadChunkTimeout = 5 * time.Minute
	// maxEnvelopeRequestBytes leaves room for the request's JSON around the envelope.
	maxEnvelopeRequestBytes = signing.MaxEnvelopeBytes + 1024
)

func (s *Server) handleCreateArtifact(w http.ResponseWriter, r *http.Request) error {
	var req apiv1.CreateArtifactRequest
	if err := decodeJSONLimit(w, r, &req, maxEnvelopeRequestBytes); err != nil {
		return err
	}
	manifest, err := s.trust.Verify(req.Envelope)
	if err != nil {
		return err
	}
	if manifest.SizeBytes > s.maxArtifactBytes {
		return invalid("envelope", fmt.Sprintf("the artifact is %d bytes, above the limit of %d", manifest.SizeBytes, s.maxArtifactBytes))
	}
	envelope, err := json.Marshal(req.Envelope)
	if err != nil {
		return fmt.Errorf("encode envelope: %w", err)
	}
	digest, err := hex.DecodeString(manifest.SHA256)
	if err != nil {
		return fmt.Errorf("decode digest: %w", err)
	}
	artifact := store.NewArtifact{
		Name:           manifest.Name,
		Version:        manifest.Version,
		Platform:       manifest.Platform,
		ReleaseCounter: manifest.ReleaseCounter,
		SizeBytes:      manifest.SizeBytes,
		ChunkSize:      manifest.ChunkSize,
		Envelope:       envelope,
	}
	copy(artifact.SHA256[:], digest)

	registered, created, err := s.store.RegisterArtifact(r.Context(), artifact)
	if err != nil {
		return err
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, artifactDocument(registered))
	return nil
}

// handleFindArtifacts finds the artifact with a name, platform, and version, which together
// identify at most one. The result is a list, so that finding nothing isn't an error.
func (s *Server) handleFindArtifacts(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	name, platform, version := q.Get("name"), q.Get("platform"), q.Get("version")
	switch {
	case !rollout.ValidName(name):
		return invalid("name", "required, as lowercase letters, digits, and hyphens")
	case !rollout.ValidPlatform(platform):
		return invalid("platform", "required, in the form os/arch")
	case !rollout.ValidVersion(version):
		return invalid("version", "required, as letters, digits, '.', '_', '+', and '-'")
	}
	out := apiv1.ArtifactList{Artifacts: []apiv1.Artifact{}}
	artifact, err := s.store.FindArtifact(r.Context(), name, platform, version)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return err
	default:
		out.Artifacts = append(out.Artifacts, artifactDocument(artifact))
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) handleGetArtifact(w http.ResponseWriter, r *http.Request) error {
	artifact, err := s.store.GetArtifact(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, artifactDocument(artifact))
	return nil
}

// handleUploadContent stores an artifact's bytes, once they've proved to be exactly what
// its signed manifest describes. Repeating a finished upload changes nothing.
func (s *Server) handleUploadContent(w http.ResponseWriter, r *http.Request) error {
	artifact, err := s.store.GetArtifact(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	if artifact.ContentReadyAt != nil {
		w.WriteHeader(http.StatusNoContent)
		return nil
	}
	envelope, err := signing.ParseEnvelope(artifact.Envelope)
	if err != nil {
		return fmt.Errorf("parse stored envelope: %w", err)
	}
	manifest, err := s.trust.Verify(envelope)
	if err != nil {
		return err
	}

	// The server's read deadline is meant for ordinary requests, not large uploads. The
	// request's own context still bounds this one.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(uploadTimeout))
	body := http.MaxBytesReader(w, r.Body, manifest.SizeBytes+1)
	err = s.artifacts.Put(r.Context(), artifact.ID, body, manifest.CheckContent)
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return fmt.Errorf("%w: more bytes than the manifest describes", signing.ErrContentMismatch)
	}
	if err != nil {
		return err
	}
	if err := s.store.MarkArtifactContentReady(r.Context(), artifact.ID); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) handleDeviceManifest(w http.ResponseWriter, r *http.Request, device store.Device) error {
	artifact, err := s.store.DeviceArtifact(r.Context(), device.ID, r.PathValue("id"))
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, err = w.Write(artifact.Envelope)
	if err != nil {
		return nil //nolint:nilerr // The client went away, and there's nobody left to tell.
	}
	return nil
}

// handleDeviceContent serves artifact bytes with Range support. The ETag names the content's
// digest, so a device can tell whether it's still getting the same artifact. A device trusts
// none of this, though: it checks every chunk against its signed manifest.
func (s *Server) handleDeviceContent(w http.ResponseWriter, r *http.Request, device store.Device) error {
	artifact, err := s.store.DeviceArtifact(r.Context(), device.ID, r.PathValue("id"))
	if err != nil {
		return err
	}
	object, err := s.artifacts.Open(r.Context(), artifact.ID)
	if err != nil {
		return err
	}
	defer func() { _ = object.Close() }()

	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(downloadChunkTimeout))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("ETag", artifactETag(artifact))
	http.ServeContent(w, r, "", time.Time{}, object)
	return nil
}

func artifactETag(a store.Artifact) string {
	return `"sha256:` + hex.EncodeToString(a.SHA256[:]) + `"`
}

func artifactDocument(a store.Artifact) apiv1.Artifact {
	return apiv1.Artifact{
		ID:             a.ID,
		Name:           a.Name,
		Version:        a.Version,
		Platform:       a.Platform,
		ReleaseCounter: a.ReleaseCounter,
		SizeBytes:      a.SizeBytes,
		SHA256:         hex.EncodeToString(a.SHA256[:]),
		ChunkSize:      a.ChunkSize,
		ContentReady:   a.ContentReadyAt != nil,
		CreatedAt:      a.CreatedAt,
	}
}
