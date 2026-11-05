package metrics

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bispulm/fleet-rollout/internal/store"
)

type countingReader struct {
	reads atomic.Int32
	// block, if set, holds each read until it's closed, so that scrapes overlap.
	block chan struct{}
}

func (r *countingReader) RecentRollouts(ctx context.Context, _ time.Duration) ([]store.Rollout, error) {
	r.reads.Add(1)
	if r.block != nil {
		select {
		case <-r.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return []store.Rollout{{ID: "r1", FleetName: "lab-a", Name: "demo", State: "running"}}, nil
}

func scrapeOnce(t *testing.T, m *Metrics) {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), "GET", "/metrics", nil))
	if _, err := io.Copy(io.Discard, rec.Body); err != nil {
		t.Fatal(err)
	}
}

func TestOverlappingScrapesShareOneRead(t *testing.T) {
	reader := &countingReader{block: make(chan struct{})}
	m := New()
	m.ObserveRollouts(reader, slog.New(slog.DiscardHandler))

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { scrapeOnce(t, m) })
	}
	// Give every scrape time to arrive while the first read is held.
	time.Sleep(100 * time.Millisecond)
	close(reader.block)
	wg.Wait()

	if got := reader.reads.Load(); got != 1 {
		t.Fatalf("8 overlapping scrapes read the store %d times, want 1", got)
	}
}

func TestRolloutStateIsReadAgainOnceItIsStale(t *testing.T) {
	reader := &countingReader{}
	m := New()
	m.ObserveRollouts(reader, slog.New(slog.DiscardHandler))
	now := time.Now()
	m.rollouts.now = func() time.Time { return now }

	scrapeOnce(t, m)
	scrapeOnce(t, m)
	if got := reader.reads.Load(); got != 1 {
		t.Fatalf("two scrapes in a row read the store %d times, want 1", got)
	}
	now = now.Add(rolloutCacheTTL)
	scrapeOnce(t, m)
	if got := reader.reads.Load(); got != 2 {
		t.Fatalf("a scrape after the cache expired read the store %d times in all, want 2", got)
	}
}
