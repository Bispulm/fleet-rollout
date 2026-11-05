package rollout

import (
	"encoding/hex"
	"fmt"
	"slices"
	"testing"
)

const (
	testRolloutA = "11111111-1111-4111-8111-111111111111"
	testRolloutB = "22222222-2222-4222-8222-222222222222"
)

func deviceIDs(n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("sim-%05d", i)
	}
	return ids
}

func orderOf(ranked []RankedDevice) []string {
	out := make([]string, len(ranked))
	for i, r := range ranked {
		out[i] = r.DeviceID
	}
	return out
}

// TestRankKeyIsFrozen pins the algorithm. If this fails, every rollout's order
// would change between versions, so the fix is a new domain tag, not a new value.
func TestRankKeyIsFrozen(t *testing.T) {
	key := RankKey(testRolloutA, "sim-00042")
	const want = "5899249431e6280e750cebf4b50d69f82b65e091786f5257d11e14ae8853e41a"
	if got := hex.EncodeToString(key[:]); got != want {
		t.Fatalf("RankKey = %s, want %s", got, want)
	}
}

func TestRankKeyInputsAreUnambiguous(t *testing.T) {
	if RankKey("ab", "c") == RankKey("a", "bc") {
		t.Fatal("different rollout and device splits produced the same key")
	}
}

func TestRankDevicesIsIndependentOfInputOrder(t *testing.T) {
	ids := deviceIDs(500)
	want := orderOf(RankDevices(testRolloutA, ids))

	reversed := slices.Clone(ids)
	slices.Reverse(reversed)
	if got := orderOf(RankDevices(testRolloutA, reversed)); !slices.Equal(got, want) {
		t.Fatal("rank order depends on input order")
	}
}

func TestRankDevicesDoesNotModifyInput(t *testing.T) {
	ids := deviceIDs(50)
	before := slices.Clone(ids)
	RankDevices(testRolloutA, ids)
	if !slices.Equal(ids, before) {
		t.Fatal("RankDevices reordered the caller's slice")
	}
}

func TestRankDevicesAssignsSequentialPositions(t *testing.T) {
	for i, r := range RankDevices(testRolloutA, deviceIDs(20)) {
		if r.Position != i+1 {
			t.Fatalf("device %d has position %d", i, r.Position)
		}
	}
}

func TestAddingDevicesKeepsRelativeOrderOfOthers(t *testing.T) {
	ids := deviceIDs(1000)
	base := orderOf(RankDevices(testRolloutA, ids[:900]))
	grown := orderOf(RankDevices(testRolloutA, ids))

	inBase := make(map[string]bool, len(base))
	for _, id := range base {
		inBase[id] = true
	}
	filtered := slices.DeleteFunc(grown, func(id string) bool { return !inBase[id] })
	if !slices.Equal(filtered, base) {
		t.Fatal("adding devices changed the relative order of existing devices")
	}
}

func TestDifferentRolloutsGetIndependentOrders(t *testing.T) {
	const n, firstTenPercent = 10000, 1000
	ids := deviceIDs(n)
	a := orderOf(RankDevices(testRolloutA, ids))[:firstTenPercent]
	b := orderOf(RankDevices(testRolloutB, ids))[:firstTenPercent]

	inA := make(map[string]bool, len(a))
	for _, id := range a {
		inA[id] = true
	}
	overlap := 0
	for _, id := range b {
		if inA[id] {
			overlap++
		}
	}
	// Independent orders overlap by about 10% of 1000, so 100, with a standard
	// deviation near 9.5. This range is roughly five standard deviations wide.
	if overlap < 50 || overlap > 150 {
		t.Fatalf("first-stage overlap = %d, want about 100 for independent orders", overlap)
	}
}

func TestRankDistributesSequentialNamesEvenly(t *testing.T) {
	const n, buckets = 10000, 10
	ranked := RankDevices(testRolloutA, deviceIDs(n))
	// Devices with sequential names should spread evenly across the first stage,
	// rather than the lowest-numbered devices always going first.
	counts := make([]int, buckets)
	for _, r := range ranked[:n/10] {
		var index int
		if _, err := fmt.Sscanf(r.DeviceID, "sim-%d", &index); err != nil {
			t.Fatalf("parse %q: %v", r.DeviceID, err)
		}
		counts[index*buckets/n]++
	}
	for bucket, count := range counts {
		if count < 50 || count > 150 {
			t.Errorf("bucket %d has %d of the first 1000 devices, want about 100", bucket, count)
		}
	}
}

func TestStageTargetsRoundUpAndAreNested(t *testing.T) {
	cases := []struct {
		name     string
		n        int
		percents []int
		want     []int
	}{
		{"typical", 1000, []int{1, 10, 50, 100}, []int{10, 100, 500, 1000}},
		{"rounds up", 150, []int{1, 10, 50, 100}, []int{2, 15, 75, 150}},
		{"exact boundary does not round up", 200, []int{1, 50, 100}, []int{2, 100, 200}},
		{"tiny fleet repeats targets", 3, []int{1, 10, 50, 100}, []int{1, 1, 2, 3}},
		{"single device", 1, []int{1, 50, 100}, []int{1, 1, 1}},
		{"one stage", 7, []int{100}, []int{7}},
		{"large fleet does not overflow", 2_000_000_000, []int{99, 100}, []int{1_980_000_000, 2_000_000_000}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := StageTargets(tc.n, tc.percents)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("StageTargets(%d, %v) = %v, want %v", tc.n, tc.percents, got, tc.want)
			}
		})
	}
}

func TestStageOfPositionMapsPositionsToCohorts(t *testing.T) {
	targets := []int{1, 1, 2, 3}
	want := map[int]int{1: 1, 2: 3, 3: 4}
	for position, stage := range want {
		if got := StageOfPosition(position, targets); got != stage {
			t.Errorf("StageOfPosition(%d) = %d, want %d", position, got, stage)
		}
	}
}
