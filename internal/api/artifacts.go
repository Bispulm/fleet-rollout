package api

import (
	"encoding/hex"
	"net/http"
	"regexp"

	"github.com/Bispulm/fleet-rollout/internal/apiv1"
	"github.com/Bispulm/fleet-rollout/internal/rollout"
	"github.com/Bispulm/fleet-rollout/internal/store"
)

// sha256Hex accepts only the canonical lowercase form, so one digest has one spelling.
var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (s *Server) handleCreateArtifact(w http.ResponseWriter, r *http.Request) error {
	var req apiv1.CreateArtifactRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return err
	}
	switch {
	case !rollout.ValidName(req.Name):
		return invalid("name", "must be lowercase letters, digits, and hyphens, at most 63 characters")
	case !rollout.ValidVersion(req.Version):
		return invalid("version", "must be letters, digits, '.', '_', '+', and '-', at most 64 characters")
	case !rollout.ValidPlatform(req.Platform):
		return invalid("platform", "must have the form os/arch")
	case req.ReleaseCounter < 1:
		return invalid("release_counter", "must be at least 1")
	case req.SizeBytes < 1:
		return invalid("size_bytes", "must be at least 1")
	case !sha256Hex.MatchString(req.SHA256):
		return invalid("sha256", "must be 64 lowercase hex characters")
	}
	artifact := store.NewArtifact{
		Name:           req.Name,
		Version:        req.Version,
		Platform:       req.Platform,
		ReleaseCounter: req.ReleaseCounter,
		SizeBytes:      req.SizeBytes,
	}
	if _, err := hex.Decode(artifact.SHA256[:], []byte(req.SHA256)); err != nil {
		return invalid("sha256", "must be 64 lowercase hex characters")
	}
	created, err := s.store.RegisterArtifact(r.Context(), artifact)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, artifactDocument(created))
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

func artifactDocument(a store.Artifact) apiv1.Artifact {
	return apiv1.Artifact{
		ID:             a.ID,
		Name:           a.Name,
		Version:        a.Version,
		Platform:       a.Platform,
		ReleaseCounter: a.ReleaseCounter,
		SizeBytes:      a.SizeBytes,
		SHA256:         hex.EncodeToString(a.SHA256[:]),
		CreatedAt:      a.CreatedAt,
	}
}
