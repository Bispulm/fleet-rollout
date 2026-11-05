package store_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/Bispulm/fleet-rollout/internal/store"
)

func newArtifact(version string, release int64) store.NewArtifact {
	return store.NewArtifact{
		Name:           "demo-app",
		Version:        version,
		Platform:       "linux/arm64",
		ReleaseCounter: release,
		SizeBytes:      1024,
		SHA256:         sha256.Sum256([]byte(version)),
	}
}

func mustRegisterArtifact(t *testing.T, ctx context.Context, s *store.Store, a store.NewArtifact) store.Artifact {
	t.Helper()
	artifact, err := s.RegisterArtifact(ctx, a)
	if err != nil {
		t.Fatalf("RegisterArtifact: %v", err)
	}
	return artifact
}

func TestRegisterAndGetArtifact(t *testing.T) {
	s, ctx := newStore(t)
	input := newArtifact("1.4.0", 14)
	created := mustRegisterArtifact(t, ctx, s, input)
	if created.ID == "" || created.CreatedAt.IsZero() {
		t.Fatalf("created = %+v", created)
	}
	got, err := s.GetArtifact(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetArtifact: %v", err)
	}
	if got.Name != input.Name || got.Version != input.Version || got.Platform != input.Platform ||
		got.ReleaseCounter != 14 || got.SizeBytes != 1024 || got.SHA256 != input.SHA256 {
		t.Fatalf("got = %+v, want values from %+v", got, input)
	}
}

func TestRegisterArtifactRejectsReusedVersionOrRelease(t *testing.T) {
	s, ctx := newStore(t)
	mustRegisterArtifact(t, ctx, s, newArtifact("1.4.0", 14))
	for name, dup := range map[string]store.NewArtifact{
		"same version":         newArtifact("1.4.0", 15),
		"same release counter": newArtifact("1.5.0", 14),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := s.RegisterArtifact(ctx, dup)
			var conflict *store.ConflictError
			if !errors.As(err, &conflict) {
				t.Fatalf("error = %v, want *ConflictError", err)
			}
		})
	}
}

func TestRegisterArtifactAllowsSameVersionOnAnotherPlatform(t *testing.T) {
	s, ctx := newStore(t)
	mustRegisterArtifact(t, ctx, s, newArtifact("1.4.0", 14))
	other := newArtifact("1.4.0", 14)
	other.Platform = "linux/amd64"
	mustRegisterArtifact(t, ctx, s, other)
}

func TestGetArtifactReportsNotFound(t *testing.T) {
	s, ctx := newStore(t)
	for _, id := range []string{"00000000-0000-4000-8000-000000000000", "not-a-uuid"} {
		if _, err := s.GetArtifact(ctx, id); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("GetArtifact(%q) error = %v, want ErrNotFound", id, err)
		}
	}
}
