package main

import (
	"testing"
	"time"
)

func TestFailureWindow_TripsOnThreshold(t *testing.T) {
	w := newFailureWindow(30*time.Minute, 3)
	w.now = func() time.Time { return time.Date(2026, 5, 15, 19, 0, 0, 0, time.UTC) }
	subject := "team-a/scheduledjob/nightly-cleanup"

	w.Observe(subject)
	if _, tripped := w.Trips(subject); tripped {
		t.Errorf("tripped at 1 observation")
	}
	w.Observe(subject)
	w.Observe(subject)
	if count, tripped := w.Trips(subject); !tripped || count != 3 {
		t.Errorf("expected tripped at 3 (got count=%d tripped=%v)", count, tripped)
	}
}

func TestFailureWindow_ClearResets(t *testing.T) {
	w := newFailureWindow(30*time.Minute, 3)
	w.now = func() time.Time { return time.Date(2026, 5, 15, 19, 0, 0, 0, time.UTC) }
	subject := "team-a/scheduledjob/nightly-cleanup"
	w.Observe(subject)
	w.Observe(subject)
	w.Observe(subject)
	w.Clear(subject)
	if count, tripped := w.Trips(subject); tripped || count != 0 {
		t.Errorf("expected reset; got count=%d tripped=%v", count, tripped)
	}
}

func TestFailureWindow_PrunesOutsideHorizon(t *testing.T) {
	w := newFailureWindow(30*time.Minute, 3)
	base := time.Date(2026, 5, 15, 19, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return base.Add(-time.Hour) } // observations 1h ago
	w.Observe("s")
	w.Observe("s")
	w.Observe("s")
	w.now = func() time.Time { return base } // back to present
	if count, tripped := w.Trips("s"); tripped || count != 0 {
		t.Errorf("stale observations should have been pruned; got count=%d tripped=%v", count, tripped)
	}
}

func TestFailureWindow_PerSubjectIndependence(t *testing.T) {
	w := newFailureWindow(30*time.Minute, 3)
	w.now = func() time.Time { return time.Date(2026, 5, 15, 19, 0, 0, 0, time.UTC) }
	for i := 0; i < 3; i++ {
		w.Observe("subject-A")
	}
	w.Observe("subject-B")
	if _, t1 := w.Trips("subject-A"); !t1 {
		t.Errorf("A should have tripped")
	}
	if _, t2 := w.Trips("subject-B"); t2 {
		t.Errorf("B should not have tripped (1 obs)")
	}
}
