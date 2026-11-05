package retry

import (
	"testing"
	"time"
)

func TestDelayGrowsExponentiallyUpToTheCap(t *testing.T) {
	b := Backoff{Base: time.Second, Max: 8 * time.Second}
	wantCaps := []time.Duration{1, 2, 4, 8, 8, 8}
	for attempt, want := range wantCaps {
		// With no randomness, a delay is at its lowest, half the cap.
		b.Rand = func() float64 { return 0 }
		if got := b.Delay(attempt); got != want*time.Second/2 {
			t.Errorf("attempt %d low = %v, want %v", attempt, got, want*time.Second/2)
		}
		b.Rand = func() float64 { return 0.999999 }
		if got := b.Delay(attempt); got > want*time.Second || got < want*time.Second*9/10 {
			t.Errorf("attempt %d high = %v, want close to %v", attempt, got, want*time.Second)
		}
	}
}

func TestDelayNeverOverflowsOnLargeAttempts(t *testing.T) {
	b := Backoff{Base: time.Second, Max: time.Minute, Rand: func() float64 { return 0.5 }}
	for _, attempt := range []int{31, 62, 63, 64, 1000, 1 << 30} {
		if got := b.Delay(attempt); got <= 0 || got > time.Minute {
			t.Errorf("attempt %d = %v", attempt, got)
		}
	}
}

func TestDelaySpreadsClientsApart(t *testing.T) {
	values := []float64{0, 0.25, 0.5, 0.75}
	i := 0
	b := Backoff{Base: time.Second, Max: time.Minute, Rand: func() float64 {
		v := values[i%len(values)]
		i++
		return v
	}}
	seen := map[time.Duration]bool{}
	for range values {
		seen[b.Delay(3)] = true
	}
	if len(seen) != len(values) {
		t.Fatalf("distinct delays = %d, want %d: the same attempt must not synchronize clients", len(seen), len(values))
	}
}

func TestJitterStaysWithinItsBounds(t *testing.T) {
	for _, r := range []float64{0, 0.5, 0.999} {
		got := Jitter(time.Minute, 0.25, func() float64 { return r })
		if got < 45*time.Second || got > 75*time.Second {
			t.Errorf("Jitter with %v = %v", r, got)
		}
	}
}
