// Package signing creates and verifies signed artifact manifests. A manifest describes
// an artifact down to each chunk, and its signature covers all of that, so a device can
// check every byte it downloads against something the release's signer vouched for.
//
// Manifests are carried in DSSE envelopes (github.com/secure-systems-lab/dsse), signed
// with Ed25519.
package signing

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"

	"github.com/Bispulm/fleet-rollout/internal/rollout"
)

const (
	// ManifestSchema identifies the manifest format.
	ManifestSchema = "fleet-rollout/artifact-manifest/v1"

	// DefaultChunkSize is how much a device downloads and verifies at a time.
	DefaultChunkSize = 1 << 20

	minChunkSize = 1 << 10
	maxChunkSize = 64 << 20
	// maxChunks bounds a manifest's size, and so what a device has to hold in memory.
	maxChunks = 1 << 16
)

var (
	// ErrInvalidManifest reports a manifest that isn't acceptable, whoever signed it.
	ErrInvalidManifest = errors.New("invalid artifact manifest")

	// ErrContentMismatch reports bytes that aren't what a manifest describes.
	ErrContentMismatch = errors.New("artifact content doesn't match its manifest")

	sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Meta is what a signer states about an artifact. The rest of a manifest is computed from
// its content.
type Meta struct {
	Name           string
	Version        string
	Platform       string
	ReleaseCounter int64
	ChunkSize      int64
}

// Manifest describes an artifact exactly. Field order is fixed by this struct, though
// nothing depends on a canonical encoding, because a signature covers the exact bytes.
type Manifest struct {
	Schema         string   `json:"schema"`
	Name           string   `json:"name"`
	Version        string   `json:"version"`
	Platform       string   `json:"platform"`
	ReleaseCounter int64    `json:"release_counter"`
	SizeBytes      int64    `json:"size_bytes"`
	SHA256         string   `json:"sha256"`
	ChunkSize      int64    `json:"chunk_size"`
	Chunks         []string `json:"chunks"`
}

// BuildManifest reads content once, computing the digests as it goes, so memory use
// doesn't grow with the artifact's size.
func BuildManifest(meta Meta, content io.Reader) (Manifest, error) {
	m := Manifest{
		Schema:         ManifestSchema,
		Name:           meta.Name,
		Version:        meta.Version,
		Platform:       meta.Platform,
		ReleaseCounter: meta.ReleaseCounter,
		ChunkSize:      meta.ChunkSize,
	}
	if m.ChunkSize < minChunkSize || m.ChunkSize > maxChunkSize {
		return Manifest{}, fmt.Errorf("%w: chunk size must be between %d and %d bytes", ErrInvalidManifest, minChunkSize, maxChunkSize)
	}

	full := sha256.New()
	buffer := make([]byte, m.ChunkSize)
	for {
		n, err := io.ReadFull(content, buffer)
		if n > 0 {
			chunk := sha256.Sum256(buffer[:n])
			m.Chunks = append(m.Chunks, hex.EncodeToString(chunk[:]))
			full.Write(buffer[:n])
			m.SizeBytes += int64(n)
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			return Manifest{}, fmt.Errorf("read artifact: %w", err)
		}
	}
	m.SHA256 = hex.EncodeToString(full.Sum(nil))
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// Validate checks that a manifest is complete and consistent with itself.
func (m Manifest) Validate() error {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidManifest, fmt.Sprintf(format, args...))
	}
	switch {
	case m.Schema != ManifestSchema:
		return invalid("unknown schema %q", m.Schema)
	case !rollout.ValidName(m.Name):
		return invalid("malformed name")
	case !rollout.ValidVersion(m.Version):
		return invalid("malformed version")
	case !rollout.ValidPlatform(m.Platform):
		return invalid("malformed platform")
	case m.ReleaseCounter < 1:
		return invalid("release counter must be at least 1")
	case m.SizeBytes < 1:
		return invalid("size must be at least 1 byte")
	case !sha256Hex.MatchString(m.SHA256):
		return invalid("malformed digest")
	case m.ChunkSize < minChunkSize || m.ChunkSize > maxChunkSize:
		return invalid("chunk size must be between %d and %d bytes", minChunkSize, maxChunkSize)
	case len(m.Chunks) > maxChunks:
		return invalid("too many chunks")
	}
	if want := (m.SizeBytes + m.ChunkSize - 1) / m.ChunkSize; int64(len(m.Chunks)) != want {
		return invalid("%d chunks can't describe %d bytes in chunks of %d", len(m.Chunks), m.SizeBytes, m.ChunkSize)
	}
	for i, digest := range m.Chunks {
		if !sha256Hex.MatchString(digest) {
			return invalid("malformed digest for chunk %d", i)
		}
	}
	return nil
}

// ChunkOffset returns where a chunk starts.
func (m Manifest) ChunkOffset(i int) int64 { return int64(i) * m.ChunkSize }

// ChunkLength returns a chunk's size. Only the last one can be short.
func (m Manifest) ChunkLength(i int) int64 {
	return min(m.ChunkSize, m.SizeBytes-m.ChunkOffset(i))
}

// ChunkMatches reports whether data is exactly chunk i.
func (m Manifest) ChunkMatches(i int, data []byte) bool {
	if i < 0 || i >= len(m.Chunks) || int64(len(data)) != m.ChunkLength(i) {
		return false
	}
	sum := sha256.Sum256(data)
	return subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(m.Chunks[i])) == 1
}

// CheckContent reads content to its end, and returns ErrContentMismatch unless it's
// exactly what the manifest describes.
func (m Manifest) CheckContent(content io.Reader) error {
	full := sha256.New()
	buffer := make([]byte, m.ChunkSize)
	var total int64
	for i := 0; ; i++ {
		n, err := io.ReadFull(content, buffer)
		if n > 0 {
			if i >= len(m.Chunks) || !m.ChunkMatches(i, buffer[:n]) {
				return fmt.Errorf("%w: chunk %d", ErrContentMismatch, i)
			}
			full.Write(buffer[:n])
			total += int64(n)
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read artifact: %w", err)
		}
	}
	if total != m.SizeBytes {
		return fmt.Errorf("%w: %d bytes, want %d", ErrContentMismatch, total, m.SizeBytes)
	}
	if !bytes.Equal([]byte(hex.EncodeToString(full.Sum(nil))), []byte(m.SHA256)) {
		return fmt.Errorf("%w: full digest", ErrContentMismatch)
	}
	return nil
}
