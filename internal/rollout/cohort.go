package rollout

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/binary"
	"slices"
)

// rankDomain versions the ranking algorithm. Changing how keys are computed
// requires a new tag, so existing rollouts can still be explained.
const rankDomain = "fleet-rollout/cohort/v1"

// RankedDevice is a device's place in a rollout's frozen order.
type RankedDevice struct {
	DeviceID string
	// Position is 1-based and unique within the rollout.
	Position int
	Key      [sha256.Size]byte
}

// RankKey computes the sort key for a device within a rollout. Each input is
// length-prefixed, so no two distinct inputs share an encoding.
func RankKey(rolloutID, deviceID string) [sha256.Size]byte {
	h := sha256.New()
	for _, field := range []string{rankDomain, rolloutID, deviceID} {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(field)))
		h.Write(length[:])
		h.Write([]byte(field))
	}
	var key [sha256.Size]byte
	h.Sum(key[:0])
	return key
}

// RankDevices returns the devices in rollout order: by rank key, with the device
// ID as a tie-breaker. The result depends only on the rollout ID and the set of
// device IDs, not on the order they were supplied in.
func RankDevices(rolloutID string, deviceIDs []string) []RankedDevice {
	ranked := make([]RankedDevice, len(deviceIDs))
	for i, id := range deviceIDs {
		ranked[i] = RankedDevice{DeviceID: id, Key: RankKey(rolloutID, id)}
	}
	slices.SortFunc(ranked, func(a, b RankedDevice) int {
		if c := bytes.Compare(a.Key[:], b.Key[:]); c != 0 {
			return c
		}
		return cmp.Compare(a.DeviceID, b.DeviceID)
	})
	for i := range ranked {
		ranked[i].Position = i + 1
	}
	return ranked
}

// StageTargets returns each stage's cumulative device count for n members,
// rounding up so that a non-empty fleet's first stage always exposes a device.
// Integer arithmetic keeps the result exact.
func StageTargets(n int, percents []int) []int {
	targets := make([]int, len(percents))
	for i, percent := range percents {
		targets[i] = int((int64(percent)*int64(n) + 99) / 100)
	}
	return targets
}

// StageOfPosition returns the 1-based stage that first exposes a position, or 0
// if the position is beyond the final target.
func StageOfPosition(position int, targets []int) int {
	for i, target := range targets {
		if position <= target {
			return i + 1
		}
	}
	return 0
}
