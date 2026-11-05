package artifactstore_test

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Bispulm/fleet-rollout/internal/artifactstore"
)

// TestEveryStoreKeepsTheContract runs what the server relies on against every implementation.
func TestEveryStoreKeepsTheContract(t *testing.T) {
	stores := map[string]func(t *testing.T) artifactstore.Store{
		"filesystem": func(t *testing.T) artifactstore.Store {
			store, _ := newStore(t)
			return store
		},
		"memory": func(*testing.T) artifactstore.Store { return artifactstore.NewMemory() },
	}
	for name, newStore := range stores {
		t.Run(name, func(t *testing.T) {
			read := func(t *testing.T, store artifactstore.Store, offset int64) (string, error) {
				t.Helper()
				object, err := store.Open(t.Context(), testID)
				if err != nil {
					return "", err
				}
				defer func() { _ = object.Close() }()
				if _, err := object.Seek(offset, io.SeekStart); err != nil {
					t.Fatalf("Seek: %v", err)
				}
				got, err := io.ReadAll(object)
				if err != nil {
					t.Fatalf("read: %v", err)
				}
				// Every test that gets this far stored the same content.
				if object.Size() != int64(len("artifact bytes")) {
					t.Errorf("Size = %d", object.Size())
				}
				return string(got), nil
			}

			t.Run("content can be read back from any offset", func(t *testing.T) {
				store := newStore(t)
				if err := store.Put(t.Context(), testID, strings.NewReader("artifact bytes"), accept); err != nil {
					t.Fatalf("Put: %v", err)
				}
				if got, err := read(t, store, 9); err != nil || got != "bytes" {
					t.Fatalf("got %q, %v", got, err)
				}
			})
			t.Run("missing content is reported", func(t *testing.T) {
				if _, err := read(t, newStore(t), 0); !errors.Is(err, artifactstore.ErrNotFound) {
					t.Fatalf("error = %v, want ErrNotFound", err)
				}
			})
			t.Run("content that fails the check is never visible", func(t *testing.T) {
				store := newStore(t)
				refuse := errors.New("digest mismatch")
				if err := store.Put(t.Context(), testID, strings.NewReader("bad"), func(io.Reader) error { return refuse }); !errors.Is(err, refuse) {
					t.Fatalf("error = %v, want the check's error", err)
				}
				if _, err := read(t, store, 0); !errors.Is(err, artifactstore.ErrNotFound) {
					t.Fatalf("rejected content is visible: %v", err)
				}
			})
			t.Run("a failed replace keeps what was there", func(t *testing.T) {
				store := newStore(t)
				if err := store.Put(t.Context(), testID, strings.NewReader("artifact bytes"), accept); err != nil {
					t.Fatalf("Put: %v", err)
				}
				if err := store.Put(t.Context(), testID, &failingReader{remaining: 3}, accept); err == nil {
					t.Fatal("Put succeeded on a failed read")
				}
				if got, err := read(t, store, 0); err != nil || got != "artifact bytes" {
					t.Fatalf("got %q, %v", got, err)
				}
			})
			t.Run("an ID that isn't a UUID is refused", func(t *testing.T) {
				if err := newStore(t).Put(t.Context(), "../escape", strings.NewReader("x"), accept); err == nil {
					t.Fatal("Put accepted a path as an ID")
				}
			})
		})
	}
}
