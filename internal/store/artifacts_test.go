package store_test

import (
	"bytes"
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
		ChunkSize:      1024,
		// The store keeps this as given. Verifying signatures happens above it.
		Envelope: []byte(`{"envelope":"` + version + `"}`),
	}
}

func mustRegisterArtifact(t *testing.T, ctx context.Context, s *store.Store, a store.NewArtifact) store.Artifact {
	t.Helper()
	artifact, _, err := s.RegisterArtifact(ctx, a)
	if err != nil {
		t.Fatalf("RegisterArtifact: %v", err)
	}
	if err := s.MarkArtifactContentReady(ctx, artifact.ID); err != nil {
		t.Fatalf("MarkArtifactContentReady: %v", err)
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
		got.ReleaseCounter != 14 || got.SizeBytes != 1024 || got.SHA256 != input.SHA256 || got.ChunkSize != 1024 {
		t.Fatalf("got = %+v, want values from %+v", got, input)
	}
	if !bytes.Equal(got.Envelope, input.Envelope) {
		t.Fatalf("envelope = %s, want it stored exactly", got.Envelope)
	}
	if got.ContentReadyAt == nil {
		t.Fatal("content wasn't recorded as ready")
	}
}

func TestRegisteringTheSameContentAgainReturnsTheArtifact(t *testing.T) {
	s, ctx := newStore(t)
	first, created, err := s.RegisterArtifact(ctx, newArtifact("1.4.0", 14))
	if err != nil || !created {
		t.Fatalf("first: created = %v, err = %v", created, err)
	}
	if first.ContentReadyAt != nil {
		t.Fatal("content is ready before any was uploaded")
	}
	again, created, err := s.RegisterArtifact(ctx, newArtifact("1.4.0", 14))
	if err != nil || created {
		t.Fatalf("retry: created = %v, err = %v", created, err)
	}
	if again.ID != first.ID {
		t.Fatalf("retry returned %s, want %s", again.ID, first.ID)
	}
}

func TestMarkingContentReadyIsIdempotent(t *testing.T) {
	s, ctx := newStore(t)
	artifact := mustRegisterArtifact(t, ctx, s, newArtifact("1.4.0", 14))
	if err := s.MarkArtifactContentReady(ctx, artifact.ID); err != nil {
		t.Fatalf("second MarkArtifactContentReady: %v", err)
	}
	if err := s.MarkArtifactContentReady(ctx, "not-a-uuid"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("malformed ID: error = %v", err)
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
			_, _, err := s.RegisterArtifact(ctx, dup)
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
