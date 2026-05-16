package main

import (
	"testing"
	"time"
)

func TestWindow_ObserveAndSnapshot(t *testing.T) {
	w := newWindow(time.Hour)
	base := time.Date(2026, 5, 15, 19, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return base }

	w.Observe("default/promise/redis", "kratix.promise.unavailable", "warning")
	w.Observe("default/promise/redis", "kratix.promise.available", "info")
	w.Observe("default/promise/kafka", "kratix.promise.unavailable", "warning")

	d := w.Snapshot()
	if d.Totals["warning"] != 2 {
		t.Errorf("total warning = %d, want 2", d.Totals["warning"])
	}
	if d.Totals["info"] != 1 {
		t.Errorf("total info = %d, want 1", d.Totals["info"])
	}
	if len(d.BySubject) != 2 {
		t.Fatalf("subjects = %d, want 2", len(d.BySubject))
	}
	if d.BySubject[0].Subject != "default/promise/redis" {
		t.Errorf("top subject = %q (expected highest-count first)", d.BySubject[0].Subject)
	}
}

func TestWindow_DropsExpiredEntries(t *testing.T) {
	w := newWindow(time.Hour)
	now := time.Date(2026, 5, 15, 19, 0, 0, 0, time.UTC)

	// An event from 90 minutes ago — outside the 1h horizon.
	w.now = func() time.Time { return now.Add(-90 * time.Minute) }
	w.Observe("default/promise/redis", "kratix.promise.unavailable", "warning")

	// A fresh event "now".
	w.now = func() time.Time { return now }
	w.Observe("default/promise/redis", "kratix.promise.available", "info")

	d := w.Snapshot()
	if got := d.Totals["warning"]; got != 0 {
		t.Errorf("expired event still counted: warning=%d", got)
	}
	if got := d.Totals["info"]; got != 1 {
		t.Errorf("fresh event missing: info=%d, want 1", got)
	}
}

func TestWindow_SnapshotPrunesEmptySubjects(t *testing.T) {
	w := newWindow(time.Hour)
	now := time.Date(2026, 5, 15, 19, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return now.Add(-90 * time.Minute) }
	w.Observe("default/promise/redis", "kratix.promise.unavailable", "warning")

	w.now = func() time.Time { return now }
	d := w.Snapshot()
	if len(d.BySubject) != 0 {
		t.Errorf("expected empty subjects after prune, got %d", len(d.BySubject))
	}
	// And the next Snapshot must not still hold the subject internally.
	if _, ok := w.bySubject["default/promise/redis"]; ok {
		t.Errorf("expired subject still in internal map")
	}
}

func TestWindow_TopTypes(t *testing.T) {
	w := newWindow(time.Hour)
	now := time.Date(2026, 5, 15, 19, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return now }

	// One subject, four distinct types. Top-3 wanted.
	w.Observe("default/promise/redis", "kratix.promise.unavailable", "warning")
	w.Observe("default/promise/redis", "kratix.promise.unavailable", "warning")
	w.Observe("default/promise/redis", "kratix.promise.unavailable", "warning")
	w.Observe("default/promise/redis", "kratix.work.failed", "warning")
	w.Observe("default/promise/redis", "kratix.work.failed", "warning")
	w.Observe("default/promise/redis", "kratix.pipeline.completed", "info")
	w.Observe("default/promise/redis", "kratix.promise.available", "info")

	d := w.Snapshot()
	if len(d.BySubject) != 1 {
		t.Fatalf("subjects = %d", len(d.BySubject))
	}
	got := d.BySubject[0].TopTypes
	want := []string{"kratix.promise.unavailable", "kratix.work.failed", "kratix.pipeline.completed"}
	if len(got) != 3 {
		t.Fatalf("topTypes = %v, want 3 entries", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("topTypes[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestWindow_ConcurrentObserve(t *testing.T) {
	// Light smoke check that the mutex actually protects state. Run a
	// dozen goroutines each appending 100 events; the snapshot total
	// should equal goroutines*100.
	w := newWindow(time.Hour)
	now := time.Date(2026, 5, 15, 19, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return now }

	const goroutines = 12
	const each = 100
	done := make(chan struct{})
	for g := 0; g < goroutines; g++ {
		go func() {
			for i := 0; i < each; i++ {
				w.Observe("default/promise/redis", "kratix.promise.unavailable", "warning")
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < goroutines; i++ {
		<-done
	}
	d := w.Snapshot()
	if got := d.Totals["warning"]; got != goroutines*each {
		t.Errorf("warning total = %d, want %d", got, goroutines*each)
	}
}
