package main

import (
	"sort"
	"sync"
	"time"
)

// window is the agent's in-memory rolling event store. Events older than
// the horizon are dropped on every observation and on every snapshot —
// there is no background expiry goroutine because the cost of incremental
// pruning at each Observe call is negligible at typical event volumes.
//
// All methods are safe for concurrent use.
type window struct {
	mu        sync.RWMutex
	horizon   time.Duration
	now       func() time.Time // injected for tests
	bySubject map[string][]eventEntry
}

type eventEntry struct {
	at       time.Time
	ceType   string
	severity string // "info" | "warning"
}

// digestEntry is the per-subject record that ends up in the emitted CE.
type digestEntry struct {
	Subject  string         `json:"subject"`
	Counts   map[string]int `json:"counts"` // severity -> count
	TopTypes []string       `json:"topTypes"`
	Total    int            `json:"total"`
}

// digest is the agent's output payload — a snapshot of the window.
type digest struct {
	WindowStart time.Time     `json:"windowStart"`
	WindowEnd   time.Time     `json:"windowEnd"`
	Totals      map[string]int `json:"totals"`
	BySubject   []digestEntry `json:"bySubject"`
}

func newWindow(horizon time.Duration) *window {
	return &window{
		horizon:   horizon,
		now:       time.Now,
		bySubject: map[string][]eventEntry{},
	}
}

// Observe records one event. Subject is the CloudEvent's `subject` field,
// ceType its `type`, severity its `kratixseverity` extension.
func (w *window) Observe(subject, ceType, severity string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	w.bySubject[subject] = append(w.bySubject[subject], eventEntry{
		at:       now,
		ceType:   ceType,
		severity: severity,
	})
	// Opportunistic prune of this subject's list so memory stays bounded
	// even for chatty subjects. Other subjects' lists are pruned the next
	// time they receive an event (or on Snapshot).
	w.bySubject[subject] = pruneOlderThan(w.bySubject[subject], now.Add(-w.horizon))
}

// Snapshot returns the current digest. Pruning happens here so the snapshot
// reflects the *current* window even for subjects that haven't seen recent
// events.
func (w *window) Snapshot() digest {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	cutoff := now.Add(-w.horizon)

	d := digest{
		WindowStart: cutoff,
		WindowEnd:   now,
		Totals:      map[string]int{},
		BySubject:   nil,
	}

	for subject, entries := range w.bySubject {
		entries = pruneOlderThan(entries, cutoff)
		w.bySubject[subject] = entries
		if len(entries) == 0 {
			delete(w.bySubject, subject)
			continue
		}
		entry := digestEntry{
			Subject: subject,
			Counts:  map[string]int{},
			Total:   len(entries),
		}
		typeCounts := map[string]int{}
		for _, e := range entries {
			entry.Counts[e.severity]++
			d.Totals[e.severity]++
			typeCounts[e.ceType]++
		}
		entry.TopTypes = topN(typeCounts, 3)
		d.BySubject = append(d.BySubject, entry)
	}

	// Stable ordering for deterministic output. Total descending, subject
	// ascending as a tiebreaker.
	sort.Slice(d.BySubject, func(i, j int) bool {
		if d.BySubject[i].Total != d.BySubject[j].Total {
			return d.BySubject[i].Total > d.BySubject[j].Total
		}
		return d.BySubject[i].Subject < d.BySubject[j].Subject
	})
	return d
}

// pruneOlderThan returns the suffix of entries whose timestamp is >= cutoff.
// Entries are assumed to be appended in roughly chronological order; we use
// a binary search for correctness even if Observe is called out-of-order.
func pruneOlderThan(entries []eventEntry, cutoff time.Time) []eventEntry {
	i := sort.Search(len(entries), func(i int) bool {
		return !entries[i].at.Before(cutoff)
	})
	if i == 0 {
		return entries
	}
	return append([]eventEntry(nil), entries[i:]...)
}

// topN returns the top-N keys of counts, ordered by count descending.
// Ties broken alphabetically for deterministic output.
func topN(counts map[string]int, n int) []string {
	if len(counts) == 0 {
		return nil
	}
	type kv struct {
		k string
		v int
	}
	sorted := make([]kv, 0, len(counts))
	for k, v := range counts {
		sorted = append(sorted, kv{k, v})
	}
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].v != sorted[j].v {
			return sorted[i].v > sorted[j].v
		}
		return sorted[i].k < sorted[j].k
	})
	if len(sorted) > n {
		sorted = sorted[:n]
	}
	out := make([]string, len(sorted))
	for i, kv := range sorted {
		out[i] = kv.k
	}
	return out
}
