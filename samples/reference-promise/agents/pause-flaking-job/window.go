package main

import (
	"sort"
	"sync"
	"time"
)

// failureWindow tracks recent ScheduledJob job.failed observations per
// scheduledJob subject. It's the agent's sole state. In-memory only;
// agent restart resets the window (acceptable because the K8s Event TTL
// is comparable to the horizon).
//
// Safe for concurrent use.
type failureWindow struct {
	mu        sync.Mutex
	horizon   time.Duration
	threshold int
	now       func() time.Time
	bySubject map[string][]time.Time
}

func newFailureWindow(horizon time.Duration, threshold int) *failureWindow {
	return &failureWindow{
		horizon:   horizon,
		threshold: threshold,
		now:       time.Now,
		bySubject: map[string][]time.Time{},
	}
}

// Observe records a failure for subject at the current observed time.
// Older entries beyond the horizon are pruned on every observation so
// memory stays bounded for noisy schedules.
func (w *failureWindow) Observe(subject string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	w.bySubject[subject] = append(w.bySubject[subject], now)
	w.bySubject[subject] = pruneOlderThan(w.bySubject[subject], now.Add(-w.horizon))
}

// Clear drops all observations for subject. Called when we see a successful
// completion — a single green run "resets" the agent's view of flakiness.
func (w *failureWindow) Clear(subject string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.bySubject, subject)
}

// Trips reports whether subject has met or exceeded the threshold inside
// the horizon as of the observed time. The caller decides what to do.
func (w *failureWindow) Trips(subject string) (count int, tripped bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	cutoff := now.Add(-w.horizon)
	entries := pruneOlderThan(w.bySubject[subject], cutoff)
	w.bySubject[subject] = entries
	return len(entries), len(entries) >= w.threshold
}

// Snapshot returns a copy of the per-subject counts, for diagnostics
// (e.g. /healthz/state). Order is stable for deterministic output.
func (w *failureWindow) Snapshot() []SubjectCount {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]SubjectCount, 0, len(w.bySubject))
	for s, entries := range w.bySubject {
		out = append(out, SubjectCount{Subject: s, Count: len(entries)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Subject < out[j].Subject })
	return out
}

// SubjectCount is the diagnostic shape for Snapshot.
type SubjectCount struct {
	Subject string
	Count   int
}

func pruneOlderThan(in []time.Time, cutoff time.Time) []time.Time {
	i := sort.Search(len(in), func(i int) bool { return !in[i].Before(cutoff) })
	if i == 0 {
		return in
	}
	return append([]time.Time(nil), in[i:]...)
}
