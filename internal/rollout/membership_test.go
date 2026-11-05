package rollout

import (
	"errors"
	"testing"
)

func candidate(id string) Candidate {
	return Candidate{ID: id, Platform: "linux/arm64", Status: DeviceActive, Labels: map[string]string{"site": "lab"}}
}

func membershipPolicy(t *testing.T, edit func(*PolicySpec)) Policy {
	t.Helper()
	spec := PolicySpec{
		Stages: []StageSpec{{Percent: 50}, {Percent: 100}},
		Safety: SafetySpec{MaxFailureBPS: ptr(200)},
	}
	if edit != nil {
		edit(&spec)
	}
	p, err := spec.Normalize()
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	return p
}

var testTarget = ArtifactTarget{Platform: "linux/arm64", Release: 10}

func TestExclusionReasons(t *testing.T) {
	policy := membershipPolicy(t, func(s *PolicySpec) {
		s.Selector = map[string]string{"site": "lab"}
		// Deliberately out of order, which Normalize must sort for lookups to work.
		s.ExcludeDevices = []string{"zulu", "pinned", "alpha"}
	})
	cases := []struct {
		name string
		edit func(*Candidate)
		want ExclusionReason
	}{
		{"eligible", func(*Candidate) {}, ""},
		{"excluded status", func(c *Candidate) { c.Status = DeviceExcluded }, ExclusionDeviceStatus},
		{"decommissioned", func(c *Candidate) { c.Status = DeviceDecommissioned }, ExclusionDeviceStatus},
		{"other platform", func(c *Candidate) { c.Platform = "linux/amd64" }, ExclusionPlatform},
		{"selector value differs", func(c *Candidate) { c.Labels = map[string]string{"site": "field"} }, ExclusionSelector},
		{"selector key missing", func(c *Candidate) { c.Labels = nil }, ExclusionSelector},
		{"listed by the rollout", func(c *Candidate) { c.ID = "pinned" }, ExclusionRollout},
		{"same release", func(c *Candidate) { c.InstalledRelease = 10 }, ExclusionAlreadyCurrent},
		{"newer release", func(c *Candidate) { c.InstalledRelease = 11 }, ExclusionAlreadyCurrent},
		{"older release", func(c *Candidate) { c.InstalledRelease = 9 }, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := candidate("d1")
			tc.edit(&c)
			if got := policy.Exclusion(c, testTarget); got != tc.want {
				t.Fatalf("Exclusion = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFreezeAssignsNestedStagesAndRecordsExclusions(t *testing.T) {
	policy := membershipPolicy(t, func(s *PolicySpec) { s.ExcludeDevices = []string{"pinned"} })
	candidates := []Candidate{candidate("a"), candidate("b"), candidate("c"), candidate("d"), candidate("pinned")}

	m, err := policy.Freeze(testRolloutA, testTarget, candidates)
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	if len(m.Members) != 4 {
		t.Fatalf("members = %d, want 4", len(m.Members))
	}
	if len(m.Exclusions) != 1 || m.Exclusions[0].DeviceID != "pinned" || m.Exclusions[0].Reason != ExclusionRollout {
		t.Fatalf("exclusions = %+v", m.Exclusions)
	}
	wantStage := []int{1, 1, 2, 2}
	for i, member := range m.Members {
		if member.Position != i+1 {
			t.Errorf("member %d position = %d", i, member.Position)
		}
		if member.Stage != wantStage[i] {
			t.Errorf("member %d stage = %d, want %d", i, member.Stage, wantStage[i])
		}
	}
	if m.Plan[0].CohortSize != 2 || m.Plan[1].CohortSize != 2 {
		t.Fatalf("plan = %+v", m.Plan)
	}
}

func TestFreezeIsReproducible(t *testing.T) {
	policy := membershipPolicy(t, nil)
	forward := []Candidate{candidate("a"), candidate("b"), candidate("c")}
	backward := []Candidate{candidate("c"), candidate("b"), candidate("a")}

	first, err := policy.Freeze(testRolloutA, testTarget, forward)
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	second, err := policy.Freeze(testRolloutA, testTarget, backward)
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	for i := range first.Members {
		if first.Members[i] != second.Members[i] {
			t.Fatalf("member %d differs: %+v and %+v", i, first.Members[i], second.Members[i])
		}
	}
}

func TestFreezeRejectsWhenExclusionsLeaveNobody(t *testing.T) {
	c := candidate("a")
	c.Platform = "linux/amd64"
	_, err := membershipPolicy(t, nil).Freeze(testRolloutA, testTarget, []Candidate{c})
	var pe *PolicyError
	if !errors.As(err, &pe) {
		t.Fatalf("Freeze error = %v, want *PolicyError", err)
	}
}

func TestFreezeRejectsWhenExclusionsMakeSampleSizeImpossible(t *testing.T) {
	policy := membershipPolicy(t, func(s *PolicySpec) { s.Stages[0].MinObservations = ptr(2) })
	ok := []Candidate{candidate("a"), candidate("b"), candidate("c")}
	if _, err := policy.Freeze(testRolloutA, testTarget, ok); err != nil {
		t.Fatalf("Freeze with enough devices: %v", err)
	}
	ok[2].Platform = "linux/amd64"
	ok[1].Platform = "linux/amd64"
	if _, err := policy.Freeze(testRolloutA, testTarget, ok); err == nil {
		t.Fatal("Freeze accepted a stage that can't reach its minimum")
	}
}

func TestMembersRecordTheReleaseTheyRanBefore(t *testing.T) {
	p, err := validSpec().Normalize()
	if err != nil {
		t.Fatal(err)
	}
	candidates := []Candidate{
		{ID: "a", Platform: "linux/arm64", Status: DeviceActive, InstalledRelease: 3},
		{ID: "b", Platform: "linux/arm64", Status: DeviceActive, InstalledRelease: 7},
	}
	m, err := p.Freeze("11111111-1111-4111-8111-111111111111", ArtifactTarget{Platform: "linux/arm64", Release: 9}, candidates)
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	prior := map[string]int64{}
	for _, member := range m.Members {
		prior[member.DeviceID] = member.PriorRelease
	}
	if prior["a"] != 3 || prior["b"] != 7 {
		t.Fatalf("prior releases = %v, want each device's installed release", prior)
	}
}
