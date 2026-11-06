// Package sim runs fleets of simulated devices. Every device is the real agent: the
// simulator replaces only what's outside it, which is the network, durable storage, the
// installer, and time, and injects faults at those boundaries.
//
// A fault decision is a pure function of the seed, the device, the kind of operation, and
// which occurrence of it this is. One device's behavior can't shift another's faults, and
// in deterministic mode, where nothing else varies, a seed reproduces the whole run.
package sim

import (
	"crypto/sha256"
	"encoding/binary"
	"strconv"
	"time"
)

// Faults are the failures to inject. Rates are probabilities per operation, unless noted.
type Faults struct {
	// Drop fails a request before it reaches the server.
	Drop float64 `json:"drop"`
	// LostResponse lets a request reach the server, then loses the response, so the device
	// can't tell whether it worked. This is what makes retries produce duplicates.
	LostResponse float64 `json:"lost_response"`
	// Corrupt flips a byte in a downloaded chunk.
	Corrupt float64 `json:"corrupt"`
	// Truncate cuts a download off partway through.
	Truncate float64 `json:"truncate"`
	// Break cuts a download off at a random point anywhere in the response, as a connection
	// dropping mid-transfer would. Unlike Truncate, which cuts within the first kilobyte, it
	// can lose most of a chunk.
	Break float64 `json:"break,omitempty"`
	// DuplicateReport delivers a report twice.
	DuplicateReport float64 `json:"duplicate_report"`
	// Crash kills the agent at a durable-state or install boundary. It restarts from what
	// it had saved, after a delay of up to RestartWithin.
	Crash         float64       `json:"crash"`
	RestartWithin time.Duration `json:"restart_within"`
	// Unhealthy is the share of devices on which the release fails its health check.
	Unhealthy float64 `json:"unhealthy"`
	// Offline is the share of devices that drop off the network once, for OfflineFor,
	// starting somewhere in the first OfflineWithin of the run.
	Offline       float64       `json:"offline"`
	OfflineFor    time.Duration `json:"offline_for"`
	OfflineWithin time.Duration `json:"offline_within"`
	// StormAt and StormFor take every device offline at once, from StormAt after the start,
	// so that they all come back together.
	StormAt  time.Duration `json:"storm_at"`
	StormFor time.Duration `json:"storm_for"`
	// Latency adds up to this much to each request. Only live mode uses it, since there's
	// nothing to wait for on a fake clock.
	Latency time.Duration `json:"latency"`
}

// decider turns the seed into fault decisions.
type decider struct {
	seed int64
}

// value returns a number in [0, 1) for one decision.
func (d decider) value(device, kind string, n int) float64 {
	h := sha256.New()
	var seed [8]byte
	binary.BigEndian.PutUint64(seed[:], uint64(d.seed)) //nolint:gosec // Only the bits matter.
	h.Write(seed[:])
	for _, part := range []string{device, kind, strconv.Itoa(n)} {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(part)))
		h.Write(length[:])
		h.Write([]byte(part))
	}
	sum := h.Sum(nil)
	return float64(binary.BigEndian.Uint64(sum[:8])>>11) / float64(1<<53)
}

// hit reports whether a fault with this rate happens.
func (d decider) hit(device, kind string, n int, rate float64) bool {
	return rate > 0 && d.value(device, kind, n) < rate
}
