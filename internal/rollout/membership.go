package rollout

import (
	"cmp"
	"crypto/sha256"
	"slices"
)

// DeviceStatus is a device's administrative status.
type DeviceStatus string

// The device statuses.
const (
	DeviceActive         DeviceStatus = "active"
	DeviceExcluded       DeviceStatus = "excluded"
	DeviceDecommissioned DeviceStatus = "decommissioned"
)

// ExclusionReason says why a fleet device isn't part of a rollout. The values
// match a check constraint in the database schema.
type ExclusionReason string

// The exclusion reasons.
const (
	ExclusionDeviceStatus   ExclusionReason = "device_status"
	ExclusionPlatform       ExclusionReason = "platform_mismatch"
	ExclusionSelector       ExclusionReason = "selector_mismatch"
	ExclusionRollout        ExclusionReason = "rollout_excluded"
	ExclusionAlreadyCurrent ExclusionReason = "already_current"
)

// Candidate is a fleet device being considered for a rollout.
type Candidate struct {
	ID               string
	Platform         string
	Status           DeviceStatus
	Labels           map[string]string
	InstalledRelease int64
}

// ArtifactTarget is what eligibility needs to know about the artifact.
type ArtifactTarget struct {
	Platform string
	Release  int64
}

// Exclusion returns why a candidate isn't eligible, or "" if it is.
func (p Policy) Exclusion(c Candidate, target ArtifactTarget) ExclusionReason {
	switch {
	case c.Status != DeviceActive:
		return ExclusionDeviceStatus
	case c.Platform != target.Platform:
		return ExclusionPlatform
	case !matchesSelector(c.Labels, p.Selector):
		return ExclusionSelector
	case isExcluded(p.ExcludeDevices, c.ID):
		return ExclusionRollout
	case c.InstalledRelease >= target.Release:
		// The agent's downgrade check would refuse this install, which would then
		// count as a failure of the release.
		return ExclusionAlreadyCurrent
	}
	return ""
}

// isExcluded searches the sorted list that Normalize produces.
func isExcluded(sorted []string, id string) bool {
	_, found := slices.BinarySearch(sorted, id)
	return found
}

func matchesSelector(labels, selector map[string]string) bool {
	for k, want := range selector {
		if got, ok := labels[k]; !ok || got != want {
			return false
		}
	}
	return true
}

// Member is a device in a rollout's frozen membership.
type Member struct {
	DeviceID string
	Position int
	// Stage is the 1-based stage that first exposes this device.
	Stage int
	Key   [sha256.Size]byte
	// PriorRelease is the release the device ran when membership froze, which a rollback
	// restores.
	PriorRelease int64
}

// ExcludedDevice records a fleet device left out of a rollout, and why.
type ExcludedDevice struct {
	DeviceID string
	Reason   ExclusionReason
}

// Membership is the frozen result of evaluating a fleet against a policy.
type Membership struct {
	Members    []Member
	Exclusions []ExcludedDevice
	Plan       []StagePlan
}

// Freeze ranks the eligible candidates and assigns each to its stage. It returns
// a *PolicyError if the resulting membership can't satisfy the policy.
func (p Policy) Freeze(rolloutID string, target ArtifactTarget, candidates []Candidate) (Membership, error) {
	var eligible []string
	var exclusions []ExcludedDevice
	prior := make(map[string]int64, len(candidates))
	for _, c := range candidates {
		if reason := p.Exclusion(c, target); reason != "" {
			exclusions = append(exclusions, ExcludedDevice{DeviceID: c.ID, Reason: reason})
			continue
		}
		eligible = append(eligible, c.ID)
		prior[c.ID] = c.InstalledRelease
	}

	plan, err := p.Plan(len(eligible))
	if err != nil {
		return Membership{}, err
	}
	targets := make([]int, len(plan))
	for i, stage := range plan {
		targets[i] = stage.Target
	}

	ranked := RankDevices(rolloutID, eligible)
	members := make([]Member, len(ranked))
	for i, r := range ranked {
		members[i] = Member{
			DeviceID:     r.DeviceID,
			Position:     r.Position,
			Stage:        StageOfPosition(r.Position, targets),
			Key:          r.Key,
			PriorRelease: prior[r.DeviceID],
		}
	}
	slices.SortFunc(exclusions, func(a, b ExcludedDevice) int { return cmp.Compare(a.DeviceID, b.DeviceID) })
	return Membership{Members: members, Exclusions: exclusions, Plan: plan}, nil
}
