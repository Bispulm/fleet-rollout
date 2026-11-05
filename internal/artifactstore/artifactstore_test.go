package artifactstore_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Bispulm/fleet-rollout/internal/artifactstore"
)

const testID = "11111111-1111-4111-8111-111111111111"

func newStore(t *testing.T) (*artifactstore.Filesystem, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "artifacts")
	store, err := artifactstore.NewFilesystem(dir)
	if err != nil {
		t.Fatalf("NewFilesystem: %v", err)
	}
	return store, dir
}

func accept(io.Reader) error { return nil }

func TestPutThenOpenReturnsTheContent(t *testing.T) {
	store, _ := newStore(t)
	content := []byte("artifact bytes")
	if err := store.Put(t.Context(), testID, bytes.NewReader(content), accept); err != nil {
		t.Fatalf("Put: %v", err)
	}
	object, err := store.Open(t.Context(), testID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = object.Close() }()
	got, err := io.ReadAll(object)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("got %q", got)
	}
	// Range requests depend on being able to seek.
	if _, err := object.Seek(9, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	rest, err := io.ReadAll(object)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(rest) != "bytes" {
		t.Fatalf("after seeking, got %q", rest)
	}
}

func TestOpenReportsMissingContent(t *testing.T) {
	store, _ := newStore(t)
	if _, err := store.Open(t.Context(), testID); !errors.Is(err, artifactstore.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

func TestFailedCheckLeavesNothingBehind(t *testing.T) {
	store, dir := newStore(t)
	refuse := errors.New("digest mismatch")
	err := store.Put(t.Context(), testID, strings.NewReader("bad bytes"), func(content io.Reader) error {
		if got, _ := io.ReadAll(content); string(got) != "bad bytes" {
			t.Errorf("the check saw %q", got)
		}
		return refuse
	})
	if !errors.Is(err, refuse) {
		t.Fatalf("error = %v, want the check's error", err)
	}
	if _, err := store.Open(t.Context(), testID); !errors.Is(err, artifactstore.ErrNotFound) {
		t.Fatalf("rejected content is visible: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("left behind: %v", entries)
	}
}

type failingReader struct{ remaining int }

func (r *failingReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, errors.New("connection reset")
	}
	n := min(len(p), r.remaining)
	r.remaining -= n
	return n, nil
}

func TestInterruptedUploadLeavesNothingBehind(t *testing.T) {
	store, dir := newStore(t)
	if err := store.Put(t.Context(), testID, &failingReader{remaining: 10}, accept); err == nil {
		t.Fatal("Put succeeded on a failed read")
	}
	if _, err := store.Open(t.Context(), testID); !errors.Is(err, artifactstore.ErrNotFound) {
		t.Fatalf("partial content is visible: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("left behind: %v", entries)
	}
}

func TestFailedReplaceKeepsTheExistingContent(t *testing.T) {
	store, _ := newStore(t)
	if err := store.Put(t.Context(), testID, strings.NewReader("good"), accept); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := store.Put(t.Context(), testID, strings.NewReader("bad"), func(io.Reader) error { return errors.New("no") }); err == nil {
		t.Fatal("Put succeeded")
	}
	object, err := store.Open(t.Context(), testID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = object.Close() }()
	if got, _ := io.ReadAll(object); string(got) != "good" {
		t.Fatalf("got %q", got)
	}
}

func TestIDsCannotEscapeTheDirectory(t *testing.T) {
	store, dir := newStore(t)
	outside := filepath.Join(filepath.Dir(dir), "outside")
	for _, id := range []string{"../outside", "a/b", "", ".", "..", "/etc/passwd", "not-a-uuid"} {
		if err := store.Put(t.Context(), id, strings.NewReader("x"), accept); err == nil {
			t.Errorf("Put(%q) succeeded", id)
		}
		if _, err := store.Open(t.Context(), id); err == nil {
			t.Errorf("Open(%q) succeeded", id)
		}
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatal("a file was written outside the store")
	}
}

func TestStoredFilesAreNotWorldReadable(t *testing.T) {
	store, dir := newStore(t)
	if err := store.Put(t.Context(), testID, strings.NewReader("x"), accept); err != nil {
		t.Fatalf("Put: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, testID))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("mode = %o", perm)
	}
}

func TestCanceledContextStopsAnUpload(t *testing.T) {
	store, _ := newStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := store.Put(ctx, testID, strings.NewReader("x"), accept); err == nil {
		t.Fatal("Put succeeded after cancellation")
	}
}
