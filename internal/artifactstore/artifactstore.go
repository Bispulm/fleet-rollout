// Package artifactstore holds artifact bytes. The local filesystem is the only driver for
// now, behind an interface small enough for an object store to fit later.
package artifactstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Bispulm/fleet-rollout/internal/rollout"
)

// ErrNotFound reports that no content exists for an artifact.
var ErrNotFound = errors.New("artifact content not found")

// Object is stored content, open for reading. Seeking is what makes Range requests work.
type Object interface {
	io.ReadSeekCloser
	Size() int64
}

// Store holds artifact content by artifact ID.
type Store interface {
	// Put reads content to its end and passes it through check. The content becomes
	// visible only if both succeed, and otherwise nothing changes.
	Put(ctx context.Context, id string, content io.Reader, check func(io.Reader) error) error
	// Open returns stored content, or ErrNotFound.
	Open(ctx context.Context, id string) (Object, error)
}

// Filesystem stores each artifact as a file named by its ID.
type Filesystem struct {
	dir string
}

// NewFilesystem creates dir if needed, readable only by this user.
func NewFilesystem(dir string) (*Filesystem, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create artifact directory: %w", err)
	}
	return &Filesystem{dir: dir}, nil
}

// path turns an ID into a file path. Only canonical UUIDs are accepted, so a request can't
// name anything outside the directory.
func (f *Filesystem) path(id string) (string, error) {
	if !rollout.ValidUUID(id) {
		return "", fmt.Errorf("artifact ID %q isn't a canonical UUID", id)
	}
	return filepath.Join(f.dir, id), nil
}

// Put writes to a temporary file, checks it, syncs it, and renames it into place, so a
// crash at any point leaves either the old content or the new, and never a partial file.
func (f *Filesystem) Put(ctx context.Context, id string, content io.Reader, check func(io.Reader) error) (err error) {
	target, err := f.path(id)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("store artifact: %w", err)
	}
	temp, err := os.CreateTemp(f.dir, ".upload-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	defer func() {
		_ = temp.Close()
		if err != nil {
			_ = os.Remove(temp.Name())
		}
	}()

	if _, err := io.Copy(temp, content); err != nil {
		return fmt.Errorf("write artifact: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("store artifact: %w", err)
	}
	if _, err := temp.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind artifact: %w", err)
	}
	if err := check(temp); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("sync artifact: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close artifact: %w", err)
	}
	if err := os.Rename(temp.Name(), target); err != nil {
		return fmt.Errorf("move artifact into place: %w", err)
	}
	return syncDir(f.dir)
}

// Open returns stored content.
func (f *Filesystem) Open(_ context.Context, id string) (Object, error) {
	target, err := f.path(id)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(target) //nolint:gosec // The path is built from a validated UUID.
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("artifact %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("open artifact: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("stat artifact: %w", err)
	}
	return &fileObject{File: file, size: info.Size()}, nil
}

type fileObject struct {
	*os.File
	size int64
}

func (o *fileObject) Size() int64 { return o.size }

// syncDir makes a rename durable. Without it, a crash could undo the rename.
func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // The directory is this store's own.
	if err != nil {
		return fmt.Errorf("open artifact directory: %w", err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync artifact directory: %w", err)
	}
	return nil
}
