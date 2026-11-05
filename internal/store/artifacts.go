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
	ChunkSize      int64
	// Envelope is the signed manifest, exactly as received. Whoever calls this has
	// already verified it, and devices verify it again for themselves.
	Envelope []byte
}

// Artifact is immutable metadata for a release.
type Artifact struct {
	NewArtifact
	ID        string
	CreatedAt time.Time
	// ContentReadyAt is when the artifact's bytes were stored and verified. It's nil
	// until then.
	ContentReadyAt *time.Time
}

// RegisterArtifact stores artifact metadata. Repeating a registration of the same content
// returns the existing artifact with created set to false, so a client can retry safely.
// Reusing a version or release counter for anything else returns a *ConflictError.
func (s *Store) RegisterArtifact(ctx context.Context, a NewArtifact) (out Artifact, created bool, err error) {
	out = Artifact{NewArtifact: a}
	err = s.pool.QueryRow(ctx, `
		INSERT INTO artifacts (name, version, platform, release_counter, size_bytes, sha256, chunk_size, envelope)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT DO NOTHING
		RETURNING id, created_at`,
		a.Name, a.Version, a.Platform, a.ReleaseCounter, a.SizeBytes, a.SHA256[:], a.ChunkSize, a.Envelope).
		Scan(&out.ID, &out.CreatedAt)
	if err == nil {
		return out, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Artifact{}, false, fmt.Errorf("insert artifact: %w", err)
	}

	conflict := &ConflictError{Message: fmt.Sprintf(
		"artifact %s for %s already uses version %q or release counter %d for different content",
		a.Name, a.Platform, a.Version, a.ReleaseCounter)}
	var existingID string
	err = s.pool.QueryRow(ctx, `SELECT id FROM artifacts WHERE name = $1 AND platform = $2 AND version = $3`,
		a.Name, a.Platform, a.Version).Scan(&existingID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Artifact{}, false, conflict
	}
	if err != nil {
		return Artifact{}, false, fmt.Errorf("query artifact: %w", err)
	}
	existing, err := readArtifact(ctx, s.pool, existingID)
	if err != nil {
		return Artifact{}, false, err
	}
	if existing.SHA256 != a.SHA256 || existing.ReleaseCounter != a.ReleaseCounter || existing.SizeBytes != a.SizeBytes {
		return Artifact{}, false, conflict
	}
	return existing, false, nil
}

// MarkArtifactContentReady records that an artifact's bytes are stored and verified. It
// does nothing if that was already recorded.
func (s *Store) MarkArtifactContentReady(ctx context.Context, id string) error {
	if !rollout.ValidUUID(id) {
		return notFound("artifact", id)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE artifacts SET content_ready_at = $2 WHERE id = $1 AND content_ready_at IS NULL`,
		id, s.now()); err != nil {
		return fmt.Errorf("record artifact content: %w", err)
	}
	return nil
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
		SELECT name, version, platform, release_counter, size_bytes, sha256, COALESCE(chunk_size, 0), envelope,
		       created_at, content_ready_at
		FROM artifacts WHERE id = $1`, id).
		Scan(&out.Name, &out.Version, &out.Platform, &out.ReleaseCounter, &out.SizeBytes, &digest, &out.ChunkSize, &out.Envelope,
			&out.CreatedAt, &out.ContentReadyAt)
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
