package api_test

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"

	"github.com/Bispulm/fleet-rollout/internal/apiv1"
)

func artifactRequest(version string, release int64) apiv1.CreateArtifactRequest {
	digest := sha256.Sum256([]byte(version))
	return apiv1.CreateArtifactRequest{
		Name: "demo-app", Version: version, Platform: "linux/arm64",
		ReleaseCounter: release, SizeBytes: 2048, SHA256: hex.EncodeToString(digest[:]),
	}
}

func (h harness) createArtifact() apiv1.Artifact {
	h.t.Helper()
	var artifact apiv1.Artifact
	h.expect(http.StatusCreated, http.MethodPost, "/v1/artifacts", artifactRequest("1.4.0", 14), &artifact)
	return artifact
}

func TestArtifactEndpoints(t *testing.T) {
	h := newHarness(t)
	created := h.createArtifact()
	if created.ID == "" || created.SHA256 != artifactRequest("1.4.0", 14).SHA256 {
		t.Fatalf("created = %+v", created)
	}

	var got apiv1.Artifact
	h.expect(http.StatusOK, http.MethodGet, "/v1/artifacts/"+created.ID, nil, &got)
	if got.Version != "1.4.0" || got.ReleaseCounter != 14 {
		t.Fatalf("got = %+v", got)
	}

	h.problem(http.StatusConflict, http.MethodPost, "/v1/artifacts", artifactRequest("1.4.0", 15))
	h.problem(http.StatusNotFound, http.MethodGet, "/v1/artifacts/00000000-0000-4000-8000-000000000000", nil)
	h.problem(http.StatusNotFound, http.MethodGet, "/v1/artifacts/not-a-uuid", nil)
}

func TestArtifactValidation(t *testing.T) {
	h := newHarness(t)
	cases := map[string]func(*apiv1.CreateArtifactRequest){
		"bad name":         func(a *apiv1.CreateArtifactRequest) { a.Name = "Bad Name" },
		"bad version":      func(a *apiv1.CreateArtifactRequest) { a.Version = "1 0" },
		"bad platform":     func(a *apiv1.CreateArtifactRequest) { a.Platform = "linux" },
		"zero release":     func(a *apiv1.CreateArtifactRequest) { a.ReleaseCounter = 0 },
		"zero size":        func(a *apiv1.CreateArtifactRequest) { a.SizeBytes = 0 },
		"short digest":     func(a *apiv1.CreateArtifactRequest) { a.SHA256 = "abc123" },
		"non-hex digest":   func(a *apiv1.CreateArtifactRequest) { a.SHA256 = strings.Repeat("z", 64) },
		"uppercase digest": func(a *apiv1.CreateArtifactRequest) { a.SHA256 = strings.Repeat("A", 64) },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			req := artifactRequest("1.4.0", 14)
			edit(&req)
			h.problem(http.StatusUnprocessableEntity, http.MethodPost, "/v1/artifacts", req)
		})
	}
}
