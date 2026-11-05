package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	checkpointFile  = "checkpoint.json"
	partialSuffix   = ".part"
	unhealthyMarker = "unhealthy"
)

var safeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`)

// writeFileAtomically replaces a file so that a crash leaves either the old content or the
// new, never a mix: write a temporary file, sync it, rename it into place, and sync the
// directory so the rename itself survives.
func writeFileAtomically(dir, name string, write func(io.Writer) error) (err error) {
	temp, err := os.CreateTemp(dir, "."+strings.TrimSuffix(name, filepath.Ext(name))+"-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	defer func() {
		_ = temp.Close()
		if err != nil {
			_ = os.Remove(temp.Name())
		}
	}()
	if err := write(temp); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", name, err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}
	if err := os.Rename(temp.Name(), filepath.Join(dir, name)); err != nil {
		return fmt.Errorf("move %s into place: %w", name, err)
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // One of the agent's own directories.
	if err != nil {
		return fmt.Errorf("open %s: %w", dir, err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", dir, err)
	}
	return nil
}

// FileCheckpoints keeps the agent's state in a file.
type FileCheckpoints struct {
	dir string
}

// NewFileCheckpoints uses dir, creating it if needed, readable only by this user.
func NewFileCheckpoints(dir string) (*FileCheckpoints, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	return &FileCheckpoints{dir: dir}, nil
}

// Load implements Checkpoints. A file that can't be decoded is an error rather than a fresh
// start, because starting over would reuse report sequences and forget what's installed.
func (f *FileCheckpoints) Load() (Checkpoint, bool, error) {
	data, err := os.ReadFile(filepath.Join(f.dir, checkpointFile))
	if errors.Is(err, os.ErrNotExist) {
		return Checkpoint{}, false, nil
	}
	if err != nil {
		return Checkpoint{}, false, fmt.Errorf("read checkpoint: %w", err)
	}
	var cp Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return Checkpoint{}, false, fmt.Errorf("checkpoint is corrupt: %w", err)
	}
	return cp, true, nil
}

// Save implements Checkpoints.
func (f *FileCheckpoints) Save(cp Checkpoint) error {
	encoded, err := json.Marshal(cp)
	if err != nil {
		return fmt.Errorf("encode checkpoint: %w", err)
	}
	return writeFileAtomically(f.dir, checkpointFile, func(w io.Writer) error {
		_, err := w.Write(encoded)
		return err
	})
}

// FilePartials keeps partial downloads as files.
type FilePartials struct {
	dir string
}

// NewFilePartials uses dir, creating it if needed.
func NewFilePartials(dir string) (*FilePartials, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create partial download directory: %w", err)
	}
	return &FilePartials{dir: dir}, nil
}

func (f *FilePartials) path(id string) (string, error) {
	if !safeName.MatchString(id) {
		return "", fmt.Errorf("artifact ID %q isn't safe to use as a file name", id)
	}
	return filepath.Join(f.dir, id+partialSuffix), nil
}

// Truncate implements Partials. It never extends a file, which would invent bytes.
func (f *FilePartials) Truncate(id string, size int64) error {
	path, err := f.path(id)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // Validated name.
	if err != nil {
		return fmt.Errorf("open partial download: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat partial download: %w", err)
	}
	if info.Size() < size {
		return fmt.Errorf("partial download has %d bytes, can't truncate to %d", info.Size(), size)
	}
	if err := file.Truncate(size); err != nil {
		return fmt.Errorf("truncate partial download: %w", err)
	}
	return file.Sync()
}

// Append implements Partials. The data is synced before it returns.
func (f *FilePartials) Append(id string, data []byte) error {
	path, err := f.path(id)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600) //nolint:gosec // Validated name.
	if err != nil {
		return fmt.Errorf("open partial download: %w", err)
	}
	defer func() { _ = file.Close() }()
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write partial download: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync partial download: %w", err)
	}
	return file.Close()
}

// Open implements Partials.
func (f *FilePartials) Open(id string) (io.ReadCloser, error) {
	path, err := f.path(id)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path) //nolint:gosec // Validated name.
	if err != nil {
		return nil, fmt.Errorf("open partial download: %w", err)
	}
	return file, nil
}

// Remove implements Partials.
func (f *FilePartials) Remove(id string) error {
	path, err := f.path(id)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove partial download: %w", err)
	}
	return nil
}

// RemoveExcept implements Partials.
func (f *FilePartials) RemoveExcept(id string) error {
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		return fmt.Errorf("list partial downloads: %w", err)
	}
	for _, entry := range entries {
		name, ok := strings.CutSuffix(entry.Name(), partialSuffix)
		if !ok || name == id {
			continue
		}
		if err := os.Remove(filepath.Join(f.dir, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove partial download: %w", err)
		}
	}
	return nil
}

// FileInstaller simulates A/B slots as files. Nothing is unpacked or run: an artifact is
// opaque bytes, copied into a slot. Creating a file named "unhealthy" in its directory makes
// health checks fail, which is a way to try a bad release by hand.
type FileInstaller struct {
	dir string
}

// NewFileInstaller uses dir, creating it if needed.
func NewFileInstaller(dir string) (*FileInstaller, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create slot directory: %w", err)
	}
	return &FileInstaller{dir: dir}, nil
}

func slotName(slot string) (string, error) {
	if slot != slotA && slot != slotB {
		return "", fmt.Errorf("unknown slot %q", slot)
	}
	return "slot-" + slot, nil
}

// Install implements Installer. The slot appears only once it's written in full.
func (f *FileInstaller) Install(_ context.Context, slot string, content io.Reader, size int64) error {
	name, err := slotName(slot)
	if err != nil {
		return err
	}
	return writeFileAtomically(f.dir, name, func(w io.Writer) error {
		written, err := io.Copy(w, io.LimitReader(content, size+1))
		if err != nil {
			return fmt.Errorf("write slot: %w", err)
		}
		if written != size {
			return fmt.Errorf("artifact has %d bytes, want %d", written, size)
		}
		return nil
	})
}

// HealthCheck implements Installer.
func (f *FileInstaller) HealthCheck(_ context.Context, slot string) error {
	name, err := slotName(slot)
	if err != nil {
		return err
	}
	info, err := os.Stat(filepath.Join(f.dir, name))
	if err != nil {
		return fmt.Errorf("slot %s: %w", slot, err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("slot %s is empty", slot)
	}
	if _, err := os.Stat(filepath.Join(f.dir, unhealthyMarker)); err == nil {
		return errors.New("the release was marked unhealthy")
	}
	return nil
}

// Discard implements Installer.
func (f *FileInstaller) Discard(_ context.Context, slot string) error {
	name, err := slotName(slot)
	if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(f.dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("discard slot: %w", err)
	}
	return nil
}
