package store_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Bispulm/fleet-rollout/internal/rollout"
	"github.com/Bispulm/fleet-rollout/internal/store"
	"github.com/Bispulm/fleet-rollout/internal/testdb"
)

func newStore(t *testing.T, opts ...store.Option) (*store.Store, context.Context) {
	t.Helper()
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return store.New(testdb.New(t), opts...), ctx
}

func newDevices(n int) []store.NewDevice {
	devices := make([]store.NewDevice, n)
	for i := range devices {
		devices[i] = store.NewDevice{
			ID:       fmt.Sprintf("sim-%05d", i),
			Platform: "linux/arm64",
			Labels:   map[string]string{"site": "lab"},
		}
		devices[i].CredentialHash = sha256.Sum256([]byte(devices[i].ID))
	}
	return devices
}

func TestCreateAndGetFleet(t *testing.T) {
	s, ctx := newStore(t)
	created, err := s.CreateFleet(ctx, "lab-a", map[string]string{"site": "lab"})
	if err != nil {
		t.Fatalf("CreateFleet: %v", err)
	}
	if created.ID == "" || created.Name != "lab-a" || created.Labels["site"] != "lab" || created.CreatedAt.IsZero() {
		t.Fatalf("created = %+v", created)
	}
	got, err := s.GetFleet(ctx, "lab-a")
	if err != nil {
		t.Fatalf("GetFleet: %v", err)
	}
	if got.ID != created.ID || got.Labels["site"] != "lab" {
		t.Fatalf("got = %+v, want %+v", got, created)
	}
}

func TestCreateFleetAcceptsNilLabels(t *testing.T) {
	s, ctx := newStore(t)
	fleet, err := s.CreateFleet(ctx, "lab-a", nil)
	if err != nil {
		t.Fatalf("CreateFleet: %v", err)
	}
	if fleet.Labels == nil || len(fleet.Labels) != 0 {
		t.Fatalf("labels = %#v, want an empty map", fleet.Labels)
	}
}

func TestCreateFleetRejectsDuplicateName(t *testing.T) {
	s, ctx := newStore(t)
	if _, err := s.CreateFleet(ctx, "lab-a", nil); err != nil {
		t.Fatalf("CreateFleet: %v", err)
	}
	_, err := s.CreateFleet(ctx, "lab-a", nil)
	var conflict *store.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("error = %v, want *ConflictError", err)
	}
}

func TestGetFleetReportsNotFound(t *testing.T) {
	s, ctx := newStore(t)
	if _, err := s.GetFleet(ctx, "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

func TestRegisterDevicesPersistsThemWithActiveStatus(t *testing.T) {
	s, ctx := newStore(t)
	if _, err := s.CreateFleet(ctx, "lab-a", nil); err != nil {
		t.Fatalf("CreateFleet: %v", err)
	}
	input := newDevices(3)
	input[1].InstalledRelease = 7

	registered, err := s.RegisterDevices(ctx, "lab-a", input)
	if err != nil {
		t.Fatalf("RegisterDevices: %v", err)
	}
	if len(registered) != 3 {
		t.Fatalf("registered %d devices, want 3", len(registered))
	}

	page, err := s.ListDevices(ctx, "lab-a", "", 10)
	if err != nil {
		t.Fatalf("ListDevices: %v", err)
	}
	if len(page) != 3 {
		t.Fatalf("listed %d devices, want 3", len(page))
	}
	d := page[1]
	if d.ID != "sim-00001" || d.Status != rollout.DeviceActive || d.InstalledRelease != 7 ||
		d.Platform != "linux/arm64" || d.Labels["site"] != "lab" {
		t.Fatalf("device = %+v", d)
	}
}

func TestRegisterDevicesIsAllOrNothingAndNamesConflicts(t *testing.T) {
	s, ctx := newStore(t)
	if _, err := s.CreateFleet(ctx, "lab-a", nil); err != nil {
		t.Fatalf("CreateFleet: %v", err)
	}
	if _, err := s.RegisterDevices(ctx, "lab-a", newDevices(1)); err != nil {
		t.Fatalf("first registration: %v", err)
	}

	// sim-00000 already exists. sim-00001 is new and must not be left behind.
	_, err := s.RegisterDevices(ctx, "lab-a", newDevices(2))
	var conflict *store.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("error = %v, want *ConflictError", err)
	}
	if !strings.Contains(conflict.Error(), "sim-00000") {
		t.Fatalf("conflict %q should name the existing device", conflict)
	}

	page, err := s.ListDevices(ctx, "lab-a", "", 10)
	if err != nil {
		t.Fatalf("ListDevices: %v", err)
	}
	if len(page) != 1 {
		t.Fatalf("fleet has %d devices after a failed batch, want 1", len(page))
	}
}

func TestRegisterDevicesRejectsDeviceInAnotherFleet(t *testing.T) {
	s, ctx := newStore(t)
	for _, name := range []string{"lab-a", "lab-b"} {
		if _, err := s.CreateFleet(ctx, name, nil); err != nil {
			t.Fatalf("CreateFleet: %v", err)
		}
	}
	if _, err := s.RegisterDevices(ctx, "lab-a", newDevices(1)); err != nil {
		t.Fatalf("first registration: %v", err)
	}
	_, err := s.RegisterDevices(ctx, "lab-b", newDevices(1))
	var conflict *store.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("error = %v, want *ConflictError", err)
	}
}

func TestRegisterDevicesRequiresExistingFleet(t *testing.T) {
	s, ctx := newStore(t)
	if _, err := s.RegisterDevices(ctx, "missing", newDevices(1)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

func TestListDevicesPagesInStableOrder(t *testing.T) {
	s, ctx := newStore(t)
	if _, err := s.CreateFleet(ctx, "lab-a", nil); err != nil {
		t.Fatalf("CreateFleet: %v", err)
	}
	if _, err := s.RegisterDevices(ctx, "lab-a", newDevices(5)); err != nil {
		t.Fatalf("RegisterDevices: %v", err)
	}

	var seen []string
	after := ""
	for {
		page, err := s.ListDevices(ctx, "lab-a", after, 2)
		if err != nil {
			t.Fatalf("ListDevices: %v", err)
		}
		if len(page) == 0 {
			break
		}
		for _, d := range page {
			seen = append(seen, d.ID)
		}
		after = page[len(page)-1].ID
	}
	want := []string{"sim-00000", "sim-00001", "sim-00002", "sim-00003", "sim-00004"}
	if !slices.Equal(seen, want) {
		t.Fatalf("paged devices = %v", seen)
	}
}

func TestListDevicesOnlyReturnsThatFleet(t *testing.T) {
	s, ctx := newStore(t)
	for _, name := range []string{"lab-a", "lab-b"} {
		if _, err := s.CreateFleet(ctx, name, nil); err != nil {
			t.Fatalf("CreateFleet: %v", err)
		}
	}
	if _, err := s.RegisterDevices(ctx, "lab-a", newDevices(2)); err != nil {
		t.Fatalf("RegisterDevices: %v", err)
	}
	page, err := s.ListDevices(ctx, "lab-b", "", 10)
	if err != nil {
		t.Fatalf("ListDevices: %v", err)
	}
	if len(page) != 0 {
		t.Fatalf("lab-b listed %d devices from another fleet", len(page))
	}
	if _, err := s.ListDevices(ctx, "missing", "", 10); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing fleet error = %v, want ErrNotFound", err)
	}
}

func TestSetDeviceStatus(t *testing.T) {
	s, ctx := newStore(t)
	if _, err := s.CreateFleet(ctx, "lab-a", nil); err != nil {
		t.Fatalf("CreateFleet: %v", err)
	}
	if _, err := s.RegisterDevices(ctx, "lab-a", newDevices(1)); err != nil {
		t.Fatalf("RegisterDevices: %v", err)
	}
	updated, err := s.SetDeviceStatus(ctx, "sim-00000", rollout.DeviceExcluded)
	if err != nil {
		t.Fatalf("SetDeviceStatus: %v", err)
	}
	if updated.Status != rollout.DeviceExcluded {
		t.Fatalf("status = %q", updated.Status)
	}
	if _, err := s.SetDeviceStatus(ctx, "missing", rollout.DeviceExcluded); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing device error = %v, want ErrNotFound", err)
	}
}
