package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// digester ticks on the configured schedule, captures a window snapshot,
// and emits it as a Kubernetes Event with kratix.io/ce-* annotations so
// the forwarder fans it out as a CloudEvent to downstream sinks.
//
// We don't use kratix-emit-the-binary here for two reasons:
//  1. We're in a long-running process, not a one-shot CLI — exec'ing for
//     every digest would be silly.
//  2. The "parent object" semantics differ: digests are attributed to the
//     agent instance, not to a Promise/RR. We construct the involvedObject
//     ourselves.
//
// The wire-format contract (eventing/WIRE-FORMAT.md) is satisfied by:
//   - reason = "HealthSummaryDigest" (kratix-shaped fallback)
//   - kratix.io/ce-type = "agent.health-summary.digest.published" (authoritative)
//   - kratix.io/ce-correlation-id = fresh ULID per digest
//   - kratix.io/ce-generation = 0 (agents don't own a generation)
type digester struct {
	window     *window
	client     kubernetes.Interface
	namespace  string
	instance   string
	interval   time.Duration
	log        *slog.Logger
	clock      func() time.Time
	tickerStop chan struct{}
}

// newDigester wires a digester. `schedule` is a v0.1-limited expression:
// "@hourly", "@daily", or "@every <duration>". Everything else returns an
// error so misconfiguration is loud rather than silently never digesting.
func newDigester(w *window, kc kubernetes.Interface, namespace, instance, schedule string, log *slog.Logger) (*digester, error) {
	interval, err := parseSchedule(schedule)
	if err != nil {
		return nil, err
	}
	return &digester{
		window:    w,
		client:    kc,
		namespace: namespace,
		instance:  instance,
		interval:  interval,
		log:       log,
		clock:     time.Now,
	}, nil
}

func parseSchedule(s string) (time.Duration, error) {
	switch s {
	case "", "@hourly":
		return time.Hour, nil
	case "@daily":
		return 24 * time.Hour, nil
	}
	if rest, ok := strings.CutPrefix(s, "@every "); ok {
		return time.ParseDuration(strings.TrimSpace(rest))
	}
	return 0, fmt.Errorf("unsupported schedule %q (v0.1 supports @hourly, @daily, or @every <duration>)", s)
}

// Run blocks until ctx is done. Each tick captures a snapshot and emits.
// Emission failures log and continue — a missed digest is preferable to a
// crashed agent.
func (d *digester) Run(ctx context.Context) {
	t := time.NewTicker(d.interval)
	defer t.Stop()
	d.log.Info("digester started", "interval", d.interval.String())
	for {
		select {
		case <-ctx.Done():
			d.log.Info("digester stopping")
			return
		case <-t.C:
			if err := d.emit(ctx); err != nil {
				d.log.Warn("digest emit failed", "err", err)
			}
		}
	}
}

func (d *digester) emit(ctx context.Context) error {
	snap := d.window.Snapshot()
	payload, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("marshal digest: %w", err)
	}

	now := metav1.NewTime(d.clock())
	corrID := newCorrelationID()

	ev := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "health-summary-digest-",
			Namespace:    d.namespace,
			Annotations: map[string]string{
				"kratix.io/ce-correlation-id": corrID,
				"kratix.io/ce-generation":     strconv.FormatInt(0, 10),
				"kratix.io/ce-type":           "agent.health-summary.digest.published",
				// Bag the digest payload as an annotation too — the
				// forwarder doesn't read this in v0.1, but a future
				// kratix.io/ce-data path would pick it up.
				"kratix.io/ce-data": string(payload),
			},
		},
		InvolvedObject: corev1.ObjectReference{
			Kind:       "HealthSummaryAgent",
			APIVersion: "agents.kratix.io/v1alpha1",
			Name:       d.instance,
			Namespace:  d.namespace,
		},
		Reason:              "HealthSummaryDigest",
		Message:             fmt.Sprintf("digest window %s..%s, totals=%v", snap.WindowStart.Format(time.RFC3339), snap.WindowEnd.Format(time.RFC3339), snap.Totals),
		Type:                corev1.EventTypeNormal,
		Source:              corev1.EventSource{Component: "health-summary-agent"},
		FirstTimestamp:      now,
		LastTimestamp:       now,
		EventTime:           metav1.NewMicroTime(now.Time),
		ReportingController: "health-summary-agent",
		ReportingInstance:   d.instance,
		Action:              "HealthSummaryDigest",
	}

	_, err = d.client.CoreV1().Events(d.namespace).Create(ctx, ev, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create digest event: %w", err)
	}
	d.log.Info("digest emitted", "subjects", len(snap.BySubject), "totals", snap.Totals)
	return nil
}

func newCorrelationID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
