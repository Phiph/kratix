package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	eventingv1alpha1 "github.com/syntasso/kratix/eventing/api/v1alpha1"
	"github.com/syntasso/kratix/eventing/pkg/schema"
)

// Drives the full loop:
//   POST /proposals (proposed) -> AgentProposal CR materialised
//   annotate CR with approved-by -> watcher emits .approved + resolves
//
// This is the integration check the user explicitly asked for. If this
// passes, every piece of the gate primitive composes correctly.
func TestGateIntegration_ProposedToApproved(t *testing.T) {
	const ns = "observability"

	cli, kc := setupFakes(t)
	now := time.Date(2026, 5, 15, 19, 0, 0, 0, time.UTC)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	rcv := newReceiver(cli, ns, log)
	wch := newWatcher(cli, kc, ns, time.Hour, log)
	wch.now = func() time.Time { return now }

	srv := httptest.NewServer(rcv.Handler())
	defer srv.Close()

	// Step 1: agent emits .proposed via HTTP.
	body := makeProposalCE(t, "01HZ9A-failover", "agent.redis.failover.proposed",
		now.Add(30*time.Minute), "agent/redis-flake-detector/v1.2.0",
		"default/promise/redis", "failover", "3 lag spikes > 30s")
	resp := postProposal(t, srv.URL, body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first POST status = %d, want 202", resp.StatusCode)
	}

	// CR exists with the expected spec.
	prop := &eventingv1alpha1.AgentProposal{}
	if err := cli.Get(context.Background(),
		client.ObjectKey{Namespace: ns, Name: "01HZ9A-failover"}, prop); err != nil {
		t.Fatalf("get proposal: %v", err)
	}
	if prop.Spec.Action != "failover" {
		t.Errorf("action = %q", prop.Spec.Action)
	}

	// Step 2: idempotency — same POST again returns 200 (dedup).
	resp = postProposal(t, srv.URL, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("dedup POST status = %d, want 200", resp.StatusCode)
	}

	// Step 3: approver annotates the CR.
	if prop.Annotations == nil {
		prop.Annotations = map[string]string{}
	}
	prop.Annotations[eventingv1alpha1.ApprovalAnnotation] = "phill@example.com"
	if err := cli.Update(context.Background(), prop); err != nil {
		t.Fatalf("annotate proposal: %v", err)
	}

	// Step 4: tick the watcher. It should see the annotation and resolve.
	if err := wch.tick(context.Background()); err != nil {
		t.Fatalf("watcher tick: %v", err)
	}

	// Step 5: proposal resolved as approved.
	if err := cli.Get(context.Background(),
		client.ObjectKey{Namespace: ns, Name: "01HZ9A-failover"}, prop); err != nil {
		t.Fatalf("re-get proposal: %v", err)
	}
	if prop.Status.Resolution != eventingv1alpha1.AgentProposalResolutionApproved {
		t.Errorf("resolution = %q, want approved", prop.Status.Resolution)
	}
	if prop.Status.ApprovedBy != "phill@example.com" {
		t.Errorf("approvedBy = %q", prop.Status.ApprovedBy)
	}
	if prop.Status.ApprovedAt == nil {
		t.Error("approvedAt unset")
	}

	// Step 6: matching .approved CE Event was emitted by the watcher.
	events, err := kc.CoreV1().Events(ns).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events.Items) != 1 {
		t.Fatalf("Events emitted = %d, want 1", len(events.Items))
	}
	ev := events.Items[0]
	if ev.Annotations[schema.AnnotationType] != "agent.redis.failover.approved" {
		t.Errorf("emitted ce-type = %q", ev.Annotations[schema.AnnotationType])
	}
	if ev.InvolvedObject.Kind != "AgentProposal" || ev.InvolvedObject.Name != "01HZ9A-failover" {
		t.Errorf("involvedObject = %+v", ev.InvolvedObject)
	}
	// Payload carries the proposalId for downstream consumers to correlate.
	if ev.Annotations["kratix.io/ce-data"] == "" {
		t.Errorf("missing ce-data payload")
	}
	// Real apiserver validation (caught by envtest_test.go) requires both
	// reportingInstance and action on EventTime-based Events. Pin them
	// here so a regression fails the cheap fake-client test first.
	if ev.ReportingInstance == "" {
		t.Errorf("reportingInstance unset; real apiserver will reject")
	}
	if ev.Action == "" {
		t.Errorf("action unset; real apiserver will reject")
	}

	// Step 7: a second tick must be a no-op (already resolved).
	if err := wch.tick(context.Background()); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	events, err = kc.CoreV1().Events(ns).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events.Items) != 1 {
		t.Errorf("re-tick emitted duplicate event; total Events = %d", len(events.Items))
	}
}

// Pins expiry: a proposal whose expiresAt has passed resolves to .expired
// when the watcher ticks, even without an approval annotation.
func TestGateIntegration_ProposedToExpired(t *testing.T) {
	const ns = "observability"
	cli, kc := setupFakes(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// expiresAt is in the past relative to the watcher's "now".
	past := time.Date(2026, 5, 15, 18, 0, 0, 0, time.UTC)
	now := time.Date(2026, 5, 15, 19, 0, 0, 0, time.UTC)

	rcv := newReceiver(cli, ns, log)
	wch := newWatcher(cli, kc, ns, time.Hour, log)
	wch.now = func() time.Time { return now }

	srv := httptest.NewServer(rcv.Handler())
	defer srv.Close()

	body := makeProposalCE(t, "01HZ9A-expire", "agent.example.do.thing.proposed",
		past, "agent/example/v1", "ns/kind/name", "do-thing", "no longer relevant")
	resp := postProposal(t, srv.URL, body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST = %d", resp.StatusCode)
	}

	if err := wch.tick(context.Background()); err != nil {
		t.Fatal(err)
	}

	prop := &eventingv1alpha1.AgentProposal{}
	if err := cli.Get(context.Background(),
		client.ObjectKey{Namespace: ns, Name: "01HZ9A-expire"}, prop); err != nil {
		t.Fatal(err)
	}
	if prop.Status.Resolution != eventingv1alpha1.AgentProposalResolutionExpired {
		t.Errorf("resolution = %q, want expired", prop.Status.Resolution)
	}

	events, err := kc.CoreV1().Events(ns).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events.Items) != 1 || events.Items[0].Annotations[schema.AnnotationType] != "agent.example.do.thing.expired" {
		t.Errorf("did not emit .expired CE; events = %+v", events.Items)
	}
}

// Approval after expiry: per design doc §6, the annotation is ignored and
// the proposal still resolves as expired.
func TestGateIntegration_ApprovalAfterExpiryIsIgnored(t *testing.T) {
	const ns = "observability"
	cli, kc := setupFakes(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	past := time.Date(2026, 5, 15, 18, 0, 0, 0, time.UTC)
	now := time.Date(2026, 5, 15, 19, 0, 0, 0, time.UTC)

	rcv := newReceiver(cli, ns, log)
	wch := newWatcher(cli, kc, ns, time.Hour, log)
	wch.now = func() time.Time { return now }

	srv := httptest.NewServer(rcv.Handler())
	defer srv.Close()

	body := makeProposalCE(t, "01HZ9A-late", "agent.example.do.thing.proposed",
		past, "agent/example/v1", "ns/kind/name", "do-thing", "noop")
	postProposal(t, srv.URL, body)

	// First tick — expires.
	if err := wch.tick(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Then someone annotates the expired CR.
	prop := &eventingv1alpha1.AgentProposal{}
	if err := cli.Get(context.Background(),
		client.ObjectKey{Namespace: ns, Name: "01HZ9A-late"}, prop); err != nil {
		t.Fatal(err)
	}
	if prop.Annotations == nil {
		prop.Annotations = map[string]string{}
	}
	prop.Annotations[eventingv1alpha1.ApprovalAnnotation] = "late@example.com"
	if err := cli.Update(context.Background(), prop); err != nil {
		t.Fatal(err)
	}

	// Second tick — must NOT re-resolve as approved.
	if err := wch.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := cli.Get(context.Background(),
		client.ObjectKey{Namespace: ns, Name: "01HZ9A-late"}, prop); err != nil {
		t.Fatal(err)
	}
	if prop.Status.Resolution != eventingv1alpha1.AgentProposalResolutionExpired {
		t.Errorf("late approval flipped resolution to %q (expected sticky expired)", prop.Status.Resolution)
	}
}

// ---------- helpers ----------

func setupFakes(t *testing.T) (client.Client, *kubefake.Clientset) {
	t.Helper()
	sch := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(sch))
	utilruntime.Must(eventingv1alpha1.AddToScheme(sch))
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithStatusSubresource(&eventingv1alpha1.AgentProposal{}).
		Build()
	kc := kubefake.NewSimpleClientset()
	return cli, kc
}

func makeProposalCE(t *testing.T, proposalID, ceType string, expiresAt time.Time, actor, subject, action, rationale string) []byte {
	t.Helper()
	envelope := map[string]any{
		"specversion":         "1.0",
		"type":                ceType,
		"subject":             subject,
		"time":                "2026-05-15T19:00:00Z",
		"kratixcorrelationid": "01HZ8W-CORR",
		"data": map[string]any{
			"action":     action,
			"actor":      actor,
			"subject":    subject,
			"rationale":  rationale,
			"proposalId": proposalID,
			"expiresAt":  expiresAt.UTC().Format(time.RFC3339),
		},
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func postProposal(t *testing.T, url string, body []byte) *http.Response {
	t.Helper()
	resp, err := http.Post(url+"/proposals", "application/cloudevents+json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// Compile-time check that corev1.Event is wired in via the kubernetes fake.
var _ = corev1.Event{}
