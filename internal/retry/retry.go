// Package retry computes how long to wait between attempts. It doesn't sleep or loop, so
// its callers stay in control of time, which is what makes them testable.
package retry

import "time"

// Backoff is exponential backoff with jitter.
type Backoff struct {
	Base time.Duration
	Max  time.Duration
	// Rand returns a value in [0, 1).
	Rand func() float64
}

// Delay returns the wait before retrying after a number of consecutive failures, starting
// from 0. It falls between half of the cap for that attempt and the cap itself. The lower
// bound keeps a client from retrying immediately, and the spread keeps clients that failed
// together from coming back together.
func (b Backoff) Delay(attempt int) time.Duration {
	limit := b.Base
	for range attempt {
		if limit >= b.Max/2 {
			limit = b.Max
			break
		}
		limit *= 2
	}
	limit = min(limit, b.Max)
	return limit/2 + time.Duration(b.Rand()*float64(limit/2))
}

// Jitter spreads a duration by plus or minus a fraction of itself.
func Jitter(d time.Duration, fraction float64, rand func() float64) time.Duration {
	return time.Duration(float64(d) * (1 - fraction + 2*fraction*rand()))
}
