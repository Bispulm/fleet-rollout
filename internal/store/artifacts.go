package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Bispulm/fleet-rollout/internal/rollout"
)

// NewArtifact is artifact metadata being registered.
type NewArtifact struct {
	Name     string
	Version  string
	Platform string
	// ReleaseCounter is the monotonic value used for downgrade protection.
	ReleaseCounter int64
	SizeBytes      int64
	SHA256         [sha256.Size]byte
}

// Artifact is immutable metadata for a release.
type Artifact struct {
	NewArtifact
	ID        string
	CreatedAt time.Time
}

// RegisterArtifact stores artifact metadata. Reusing a version or release counter
// for the same name and platform returns a *ConflictError.
func (s *Store) RegisterArtifact(ctx context.Context, a NewArtifact) (Artifact, error) {
	out := Artifact{NewArtifact: a}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO artifacts (name, version, platform, release_counter, size_bytes, sha256)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, created_at`,
		a.Name, a.Version, a.Platform, a.ReleaseCounter, a.SizeBytes, a.SHA256[:]).
		Scan(&out.ID, &out.CreatedAt)
	if isUniqueViolation(err, "") {
		return Artifact{}, &ConflictError{Message: fmt.Sprintf(
			"artifact %s for %s already uses version %q or release counter %d",
			a.Name, a.Platform, a.Version, a.ReleaseCounter)}
	}
	if err != nil {
		return Artifact{}, fmt.Errorf("insert artifact: %w", err)
	}
	return out, nil
}

// GetArtifact returns an artifact by ID.
func (s *Store) GetArtifact(ctx context.Context, id string) (Artifact, error) {
	if !rollout.ValidUUID(id) {
		return Artifact{}, notFound("artifact", id)
	}
	return readArtifact(ctx, s.pool, id)
}

func readArtifact(ctx context.Context, q querier, id string) (Artifact, error) {
	out := Artifact{ID: id}
	var digest []byte
	err := q.QueryRow(ctx, `
		SELECT name, version, platform, release_counter, size_bytes, sha256, created_at
		FROM artifacts WHERE id = $1`, id).
		Scan(&out.Name, &out.Version, &out.Platform, &out.ReleaseCounter, &out.SizeBytes, &digest, &out.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Artifact{}, notFound("artifact", id)
	}
	if err != nil {
		return Artifact{}, fmt.Errorf("query artifact: %w", err)
	}
	if copy(out.SHA256[:], digest) != sha256.Size {
		return Artifact{}, fmt.Errorf("artifact %s has a malformed digest", id)
	}
	return out, nil
}
