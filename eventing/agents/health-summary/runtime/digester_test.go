package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestParseSchedule(t *testing.T) {
	cases := map[string]time.Duration{
		"":             time.Hour,
		"@hourly":      time.Hour,
		"@daily":       24 * time.Hour,
		"@every 30s":   30 * time.Second,
		"@every 5m":    5 * time.Minute,
		"@every 2h30m": 2*time.Hour + 30*time.Minute,
	}
	for in, want := range cases {
		got, err := parseSchedule(in)
		if err != nil {
			t.Errorf("parseSchedule(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseSchedule(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := parseSchedule("0 * * * *"); err == nil {
		t.Error("expected error for full-cron expression (not v0.1 supported)")
	}
	if _, err := parseSchedule("@every nonsense"); err == nil {
		t.Error("expected error for invalid duration")
	}
}

func TestDigester_EmitWritesAnnotatedEvent(t *testing.T) {
	w := newWindow(time.Hour)
	w.now = func() time.Time { return time.Date(2026, 5, 15, 19, 0, 0, 0, time.UTC) }
	w.Observe("default/promise/redis", "kratix.promise.unavailable", "warning")
	w.Observe("default/promise/kafka", "kratix.work.failed", "warning")

	kc := fake.NewSimpleClientset()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	d, err := newDigester(w, kc, "observability", "primary", "@hourly", log)
	if err != nil {
		t.Fatal(err)
	}
	d.clock = w.now

	if err := d.emit(context.Background()); err != nil {
		t.Fatal(err)
	}

	events, err := kc.CoreV1().Events("observability").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events.Items) != 1 {
		t.Fatalf("expected 1 Event, got %d", len(events.Items))
	}
	ev := events.Items[0]
	if got := ev.Annotations["kratix.io/ce-type"]; got != "agent.health-summary.digest.published" {
		t.Errorf("ce-type = %q", got)
	}
	if ev.Annotations["kratix.io/ce-correlation-id"] == "" {
		t.Error("missing correlation id annotation")
	}
	if ev.Reason != "HealthSummaryDigest" {
		t.Errorf("reason = %q", ev.Reason)
	}
	if ev.Type != corev1.EventTypeNormal {
		t.Errorf("event type = %q", ev.Type)
	}
	if ev.InvolvedObject.Kind != "HealthSummaryAgent" {
		t.Errorf("involvedObject.kind = %q", ev.InvolvedObject.Kind)
	}

	var d2 digest
	if err := json.Unmarshal([]byte(ev.Annotations["kratix.io/ce-data"]), &d2); err != nil {
		t.Fatalf("ce-data not valid JSON: %v", err)
	}
	if d2.Totals["warning"] != 2 {
		t.Errorf("digest warning total = %d", d2.Totals["warning"])
	}
}
