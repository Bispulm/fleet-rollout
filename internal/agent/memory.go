package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"sync"
)

// MemoryCheckpoints keeps state in memory. It's for tests and the simulator, where a crash is
// simulated by throwing away an Agent and building a new one from this.
type MemoryCheckpoints struct {
	mu      sync.Mutex
	encoded []byte
	saveErr error
	saves   int
}

// NewMemoryCheckpoints returns an empty store.
func NewMemoryCheckpoints() *MemoryCheckpoints { return &MemoryCheckpoints{} }

// Load implements Checkpoints. Going through JSON means that anything the file store couldn't
// round-trip would fail here as well.
func (m *MemoryCheckpoints) Load() (Checkpoint, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.encoded == nil {
		return Checkpoint{}, false, nil
	}
	var cp Checkpoint
	if err := json.Unmarshal(m.encoded, &cp); err != nil {
		return Checkpoint{}, false, fmt.Errorf("decode checkpoint: %w", err)
	}
	return cp, true, nil
}

// Save implements Checkpoints.
func (m *MemoryCheckpoints) Save(cp Checkpoint) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.saveErr != nil {
		return m.saveErr
	}
	encoded, err := json.Marshal(cp)
	if err != nil {
		return fmt.Errorf("encode checkpoint: %w", err)
	}
	m.encoded = encoded
	m.saves++
	return nil
}

// FailSaves makes every save return err, until it's called again with nil.
func (m *MemoryCheckpoints) FailSaves(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saveErr = err
}

// MemoryPartials keeps partial downloads in memory.
type MemoryPartials struct {
	mu   sync.Mutex
	data map[string][]byte
}

// NewMemoryPartials returns an empty store.
func NewMemoryPartials() *MemoryPartials { return &MemoryPartials{data: map[string][]byte{}} }

// Truncate implements Partials.
func (m *MemoryPartials) Truncate(id string, size int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.data[id]
	if int64(len(current)) < size {
		return fmt.Errorf("partial %s has %d bytes, can't truncate to %d", id, len(current), size)
	}
	m.data[id] = current[:size:size]
	return nil
}

// Append implements Partials.
func (m *MemoryPartials) Append(id string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[id] = append(m.data[id], data...)
	return nil
}

// Open implements Partials.
func (m *MemoryPartials) Open(id string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.data[id]
	if !ok {
		return nil, fmt.Errorf("no partial download for %s", id)
	}
	return io.NopCloser(bytes.NewReader(slices.Clone(data))), nil
}

// Remove implements Partials.
func (m *MemoryPartials) Remove(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, id)
	return nil
}

// RemoveExcept implements Partials.
func (m *MemoryPartials) RemoveExcept(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	maps.DeleteFunc(m.data, func(key string, _ []byte) bool { return key != id })
	return nil
}

// Exists reports whether a partial download exists.
func (m *MemoryPartials) Exists(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.data[id]
	return ok
}

// Corrupt flips a byte of a partial download, as storage that goes bad at rest might.
func (m *MemoryPartials) Corrupt(id string, offset int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[id][offset] ^= 0xff
}

// Size reports a partial download's size.
func (m *MemoryPartials) Size(id string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return int64(len(m.data[id]))
}

// MemoryInstaller keeps slots in memory.
type MemoryInstaller struct {
	mu         sync.Mutex
	slots      map[string][]byte
	installs   int
	installErr error
	healthErr  error
}

// NewMemoryInstaller returns an installer with empty slots.
func NewMemoryInstaller() *MemoryInstaller { return &MemoryInstaller{slots: map[string][]byte{}} }

// Install implements Installer. A slot only appears once it's written in full.
func (m *MemoryInstaller) Install(_ context.Context, slot string, content io.Reader, size int64) error {
	data, err := io.ReadAll(content)
	if err != nil {
		return fmt.Errorf("read artifact: %w", err)
	}
	if int64(len(data)) != size {
		return fmt.Errorf("artifact has %d bytes, want %d", len(data), size)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.installErr != nil {
		return m.installErr
	}
	m.slots[slot] = data
	m.installs++
	return nil
}

// HealthCheck implements Installer.
func (m *MemoryInstaller) HealthCheck(_ context.Context, slot string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.slots[slot]; !ok {
		return errors.New("slot is empty")
	}
	return m.healthErr
}

// Discard implements Installer.
func (m *MemoryInstaller) Discard(_ context.Context, slot string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.slots, slot)
	return nil
}

// FailInstall makes installs return err.
func (m *MemoryInstaller) FailInstall(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.installErr = err
}

// FailHealth makes health checks return err.
func (m *MemoryInstaller) FailHealth(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.healthErr = err
}

// Installs reports how many installs have finished.
func (m *MemoryInstaller) Installs() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.installs
}

// Content returns a slot's bytes, or nil if it's empty.
func (m *MemoryInstaller) Content(slot string) []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.slots[slot]
}
