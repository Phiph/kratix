package main

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/syntasso/kratix/eventing/pkg/schema"
)

func newTestEvent() *corev1.Event {
	return &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "redis.123",
			Namespace: "default",
			Annotations: map[string]string{
				schema.AnnotationCorrelationID: "01HZ8W000000000000000000",
				schema.AnnotationGeneration:    "42",
			},
		},
		InvolvedObject: corev1.ObjectReference{
			Kind:       "Promise",
			Name:       "redis",
			Namespace:  "default",
			APIVersion: "platform.kratix.io/v1alpha1",
		},
		Reason:        "PromiseUnavailable",
		Message:       "Promise is unavailable: WorksSucceeded=False",
		Type:          "Warning",
		EventTime:     metav1.NewMicroTime(time.Date(2026, 5, 15, 19, 22, 14, 0, time.UTC)),
		LastTimestamp: metav1.Now(),
	}
}

func TestTranslate_Happy(t *testing.T) {
	ev := newTestEvent()
	ce, drop, ok := translate(ev, "prod-eu")
	if !ok {
		t.Fatalf("expected ok, got drop=%s", drop)
	}
	if ce.Type != "kratix.promise.unavailable" {
		t.Errorf("type = %q", ce.Type)
	}
	if ce.Subject != "default/promise/redis" {
		t.Errorf("subject = %q", ce.Subject)
	}
	if ce.CorrelationID != "01HZ8W000000000000000000" {
		t.Errorf("corrID = %q", ce.CorrelationID)
	}
	if ce.Generation != 42 {
		t.Errorf("gen = %d", ce.Generation)
	}
	if ce.Severity != schema.SeverityWarning {
		t.Errorf("severity = %q", ce.Severity)
	}
	if ce.Source != "/kratix/prod-eu/event-forwarder" {
		t.Errorf("source = %q", ce.Source)
	}
	if ce.SpecVersion != "1.0" {
		t.Errorf("specversion = %q", ce.SpecVersion)
	}
	if ce.ID == "" {
		t.Errorf("ID should be generated")
	}
	if ce.Data.Reason != "PromiseUnavailable" {
		t.Errorf("data.reason = %q", ce.Data.Reason)
	}
}

func TestTranslate_ExplicitTypeAnnotationWins(t *testing.T) {
	ev := newTestEvent()
	// User-emitted: pipeline.* type with a non-kratix reason. The annotation
	// is the source of truth; reason resolution is only the fallback.
	ev.Reason = "UpstreamFetchFailed"
	ev.Annotations[schema.AnnotationType] = "pipeline.redis.upstream.fetch.failed"
	ce, drop, ok := translate(ev, "prod-eu")
	if !ok {
		t.Fatalf("expected ok, got drop=%s", drop)
	}
	if ce.Type != "pipeline.redis.upstream.fetch.failed" {
		t.Errorf("type = %q, want pipeline.* from annotation", ce.Type)
	}
}

// Upstream K8s Events have no kratix.io/ce-* annotations at all, so they drop
// on the annotation check regardless of how their reason resolves. This pins
// that behaviour explicitly.
func TestTranslate_UpstreamEventDropsOnAnnotations(t *testing.T) {
	ev := newTestEvent()
	ev.Reason = "FailedScheduling"
	ev.Annotations = nil
	_, drop, ok := translate(ev, "prod-eu")
	if ok {
		t.Fatalf("expected drop, got ok")
	}
	if drop != "missing-annotation" {
		t.Errorf("drop = %q, want missing-annotation", drop)
	}
}

func TestTranslate_DropPaths(t *testing.T) {
	// The gate for "is this a Kratix event?" is the annotation set, not the
	// reason string. Upstream Kubernetes emits plenty of PascalCase reasons
	// (FailedScheduling, BackOff, Pulling, ...) that we must not translate.
	// Producers signal Kratix-origin by attaching kratix.io/ce-* annotations.
	cases := []struct {
		name string
		mut  func(*corev1.Event)
		want string
	}{
		{
			"upstream reason without kratix annotations",
			func(ev *corev1.Event) {
				ev.Reason = "FailedScheduling"
				ev.Annotations = nil
			},
			"missing-annotation",
		},
		{"empty reason", func(ev *corev1.Event) { ev.Reason = "" }, "not-kratix"},
		{"lowercase reason", func(ev *corev1.Event) { ev.Reason = "promiseUnavailable" }, "not-kratix"},
		{"missing correlation id", func(ev *corev1.Event) { delete(ev.Annotations, schema.AnnotationCorrelationID) }, "missing-annotation"},
		{"missing generation", func(ev *corev1.Event) { delete(ev.Annotations, schema.AnnotationGeneration) }, "missing-annotation"},
		{"bad generation", func(ev *corev1.Event) { ev.Annotations[schema.AnnotationGeneration] = "not-a-number" }, "bad-generation"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := newTestEvent()
			tc.mut(ev)
			ce, drop, ok := translate(ev, "prod-eu")
			if ok || ce != nil {
				t.Fatalf("expected drop, got ce=%+v", ce)
			}
			if drop != tc.want {
				t.Fatalf("drop reason = %q, want %q", drop, tc.want)
			}
		})
	}
}
