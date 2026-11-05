package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Bispulm/fleet-rollout/internal/rollout"
)

// maxReportedConflicts caps how many device IDs a conflict error lists.
const maxReportedConflicts = 10

// Fleet is a named group of devices.
type Fleet struct {
	ID        string
	Name      string
	Labels    map[string]string
	CreatedAt time.Time
}

// NewDevice is a device being registered. CredentialHash is the SHA-256 of a token
// that only the caller and the device ever see.
type NewDevice struct {
	ID               string
	Platform         string
	Labels           map[string]string
	InstalledRelease int64
	CredentialHash   [sha256.Size]byte
}

// Device is a registered device.
type Device struct {
	ID               string
	FleetID          string
	Platform         string
	Labels           map[string]string
	Status           rollout.DeviceStatus
	InstalledRelease int64
	LastSeenAt       *time.Time
	CreatedAt        time.Time
}

// CreateFleet creates a fleet. A duplicate name returns a *ConflictError.
func (s *Store) CreateFleet(ctx context.Context, name string, labels map[string]string) (Fleet, error) {
	fleet := Fleet{Name: name, Labels: orEmpty(labels)}
	err := s.pool.QueryRow(ctx,
		`INSERT INTO fleets (name, labels) VALUES ($1, $2) RETURNING id, created_at`,
		name, fleet.Labels).Scan(&fleet.ID, &fleet.CreatedAt)
	if isUniqueViolation(err, "") {
		return Fleet{}, &ConflictError{Message: fmt.Sprintf("fleet %q already exists", name)}
	}
	if err != nil {
		return Fleet{}, fmt.Errorf("insert fleet: %w", err)
	}
	return fleet, nil
}

// GetFleet returns a fleet by name.
func (s *Store) GetFleet(ctx context.Context, name string) (Fleet, error) {
	fleet := Fleet{Name: name}
	err := s.pool.QueryRow(ctx, `SELECT id, labels, created_at FROM fleets WHERE name = $1`, name).
		Scan(&fleet.ID, &fleet.Labels, &fleet.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Fleet{}, notFound("fleet", name)
	}
	if err != nil {
		return Fleet{}, fmt.Errorf("query fleet: %w", err)
	}
	return fleet, nil
}

// RegisterDevices adds devices to a fleet, all or nothing. If any ID is already
// registered in any fleet, it returns a *ConflictError naming the IDs.
func (s *Store) RegisterDevices(ctx context.Context, fleetName string, devices []NewDevice) ([]Device, error) {
	ids := make([]string, len(devices))
	platforms := make([]string, len(devices))
	labels := make([]string, len(devices))
	releases := make([]int64, len(devices))
	hashes := make([][]byte, len(devices))
	for i, d := range devices {
		encoded, err := json.Marshal(orEmpty(d.Labels))
		if err != nil {
			return nil, fmt.Errorf("encode labels of device %q: %w", d.ID, err)
		}
		ids[i], platforms[i], labels[i], releases[i] = d.ID, d.Platform, string(encoded), d.InstalledRelease
		hashes[i] = d.CredentialHash[:]
	}

	var registered []Device
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var fleetID string
		err := tx.QueryRow(ctx, `SELECT id FROM fleets WHERE name = $1`, fleetName).Scan(&fleetID)
		if errors.Is(err, pgx.ErrNoRows) {
			return notFound("fleet", fleetName)
		}
		if err != nil {
			return fmt.Errorf("query fleet: %w", err)
		}

		rows, err := tx.Query(ctx, `
			INSERT INTO devices (id, fleet_id, platform, labels, installed_release, credential_hash)
			SELECT d.id, $1, d.platform, d.labels::jsonb, d.release, d.hash
			FROM unnest($2::text[], $3::text[], $4::text[], $5::bigint[], $6::bytea[])
				AS d(id, platform, labels, release, hash)
			ON CONFLICT (id) DO NOTHING
			RETURNING `+deviceColumns, fleetID, ids, platforms, labels, releases, hashes)
		if err != nil {
			return fmt.Errorf("insert devices: %w", err)
		}
		registered, err = pgx.CollectRows(rows, scanDevice)
		if err != nil {
			return fmt.Errorf("insert devices: %w", err)
		}

		if len(registered) != len(devices) {
			inserted := make(map[string]bool, len(registered))
			for _, d := range registered {
				inserted[d.ID] = true
			}
			conflicts := slices.DeleteFunc(slices.Clone(ids), func(id string) bool { return inserted[id] })
			return devicesAlreadyRegistered(conflicts)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return registered, nil
}

func devicesAlreadyRegistered(ids []string) error {
	shown := ids[:min(len(ids), maxReportedConflicts)]
	message := fmt.Sprintf("%d of the devices are already registered: %s", len(ids), strings.Join(shown, ", "))
	if len(ids) > len(shown) {
		message += ", ..."
	}
	return &ConflictError{Message: message}
}

// ListDevices returns up to limit devices in ID order, after the given ID.
func (s *Store) ListDevices(ctx context.Context, fleetName, afterID string, limit int) ([]Device, error) {
	fleet, err := s.GetFleet(ctx, fleetName)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+deviceColumns+` FROM devices
		WHERE fleet_id = $1 AND id > $2 ORDER BY id LIMIT $3`, fleet.ID, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("query devices: %w", err)
	}
	devices, err := pgx.CollectRows(rows, scanDevice)
	if err != nil {
		return nil, fmt.Errorf("query devices: %w", err)
	}
	return devices, nil
}

// SetDeviceStatus changes a device's administrative status. It affects rollouts
// that start afterward. Membership of a started rollout stays frozen.
func (s *Store) SetDeviceStatus(ctx context.Context, id string, status rollout.DeviceStatus) (Device, error) {
	rows, err := s.pool.Query(ctx, `UPDATE devices SET status = $2 WHERE id = $1 RETURNING `+deviceColumns, id, status)
	if err != nil {
		return Device{}, fmt.Errorf("update device: %w", err)
	}
	device, err := pgx.CollectExactlyOneRow(rows, scanDevice)
	if errors.Is(err, pgx.ErrNoRows) {
		return Device{}, notFound("device", id)
	}
	if err != nil {
		return Device{}, fmt.Errorf("update device: %w", err)
	}
	return device, nil
}

const deviceColumns = `id, fleet_id, platform, labels, status, installed_release, last_seen_at, created_at`

func scanDevice(row pgx.CollectableRow) (Device, error) {
	var d Device
	err := row.Scan(&d.ID, &d.FleetID, &d.Platform, &d.Labels, &d.Status, &d.InstalledRelease, &d.LastSeenAt, &d.CreatedAt)
	return d, err
}

func orEmpty(labels map[string]string) map[string]string {
	if labels == nil {
		return map[string]string{}
	}
	return labels
}
