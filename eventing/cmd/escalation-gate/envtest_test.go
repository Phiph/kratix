//go:build envtest

// Real-apiserver smoke test for the escalation gate.
//
// Run with:
//
//	make test-envtest
//
// which installs setup-envtest, fetches the matching kube-apiserver+etcd
// binaries, and runs this file with the 'envtest' build tag and the
// KUBEBUILDER_ASSETS env var pointing at them.
//
// This is intentionally separate from the fake-client integration tests
// in integration_test.go. Those exercise Go-level composition; this one
// exercises the binary against a real apiserver: it catches missing
// scheme registrations, CRD-schema mismatches, status-subresource RBAC
// surprises, and any "works against the fake but not the real one" bugs.
package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	eventingv1alpha1 "github.com/syntasso/kratix/eventing/api/v1alpha1"
	"github.com/syntasso/kratix/eventing/pkg/schema"
)

// findRepoRoot walks up from the test file's directory to the repo root
// (identified by go.mod). Tests run from the package directory; the CRD
// manifest lives several levels up.
func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found walking up from test dir")
		}
		dir = parent
	}
}

func loadCRDsFromYAML(t *testing.T, path string) []*apiextv1.CustomResourceDefinition {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read CRD: %v", err)
	}
	dec := yaml.NewYAMLOrJSONDecoder(strings.NewReader(string(data)), 4096)
	var out []*apiextv1.CustomResourceDefinition
	for {
		crd := &apiextv1.CustomResourceDefinition{}
		if err := dec.Decode(crd); err != nil {
			break
		}
		if crd.Kind == "CustomResourceDefinition" {
			out = append(out, crd)
		}
	}
	if len(out) == 0 {
		t.Fatalf("no CRDs decoded from %s", path)
	}
	return out
}

// TestGate_EnvtestRealApiserver drives the full proposed -> approved
// loop against a real apiserver+etcd. The fake-client tests prove the Go
// logic; this one proves the binary stays correct when wired to real
// Kubernetes semantics.
func TestGate_EnvtestRealApiserver(t *testing.T) {
	root := findRepoRoot(t)
	crdPath := filepath.Join(root, "eventing", "config", "crd", "agentproposal.yaml")
	crds := loadCRDsFromYAML(t, crdPath)

	env := &envtest.Environment{
		CRDs: crds,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest start: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Logf("envtest stop: %v", err)
		}
	})

	sch := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(sch))
	utilruntime.Must(eventingv1alpha1.AddToScheme(sch))

	cli, err := client.New(cfg, client.Options{Scheme: sch})
	if err != nil {
		t.Fatalf("ctrl client: %v", err)
	}
	kc, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("kube client: %v", err)
	}

	const ns = "default"

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// For the smoke test we want to *see* watcher logs if anything goes
	// wrong — silencing them was an artefact of the fake-client tests.
	verboseLog := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	rcv := newReceiver(cli, ns, log)
	wch := newWatcher(cli, kc, ns, 200*time.Millisecond, verboseLog)

	// Run the receiver on a real ephemeral port. We don't bother running
	// the watcher's Run() loop — tick() is plenty for a smoke test and
	// avoids any race between the test asserting and the loop polling.
	srv := &http.Server{Handler: rcv.Handler()}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	go func() { _ = srv.Serve(listener) }()
	baseURL := "http://" + listener.Addr().String()

	// Build a real .proposed CloudEvent.
	expiresAt := time.Now().Add(15 * time.Minute).UTC().Format(time.RFC3339)
	ce := map[string]any{
		"specversion":         "1.0",
		"type":                "agent.example.do.thing.proposed",
		"subject":             "default/promise/example",
		"time":                time.Now().UTC().Format(time.RFC3339),
		"kratixcorrelationid": "01HZ-ENVTEST",
		"data": map[string]any{
			"action":     "do-thing",
			"actor":      "agent/example/v1",
			"subject":    "default/promise/example",
			"rationale":  "real-apiserver smoke",
			"proposalId": "envtest-01",
			"expiresAt":  expiresAt,
		},
	}
	body, err := json.Marshal(ce)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(baseURL+"/proposals", "application/cloudevents+json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST status = %d, want 202", resp.StatusCode)
	}

	// Real apiserver: the CR genuinely exists in etcd now.
	prop := &eventingv1alpha1.AgentProposal{}
	if err := cli.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "envtest-01"}, prop); err != nil {
		t.Fatalf("get proposal: %v", err)
	}
	if prop.Spec.Action != "do-thing" {
		t.Errorf("action = %q", prop.Spec.Action)
	}

	// Approve via kratix-approve's underlying function — the same code path
	// the CLI takes. We can't import the CLI package directly (it's
	// package main), but the operation is one Update.
	if prop.Annotations == nil {
		prop.Annotations = map[string]string{}
	}
	prop.Annotations[eventingv1alpha1.ApprovalAnnotation] = "envtest@example.com"
	if err := cli.Update(context.Background(), prop); err != nil {
		t.Fatalf("annotate: %v", err)
	}

	// Re-fetch and confirm the annotation actually landed in etcd before
	// the watcher tick — sanity check before debugging anywhere deeper.
	if err := cli.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "envtest-01"}, prop); err != nil {
		t.Fatal(err)
	}
	if got := prop.Annotations[eventingv1alpha1.ApprovalAnnotation]; got != "envtest@example.com" {
		t.Fatalf("annotation pre-tick = %q (not persisted to apiserver)", got)
	}

	// Tick the watcher. Real apiserver = real status subresource semantics.
	if err := wch.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	// Confirm status was written (the real apiserver enforces the
	// status subresource — fake clients don't).
	if err := cli.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "envtest-01"}, prop); err != nil {
		t.Fatal(err)
	}
	if prop.Status.Resolution != eventingv1alpha1.AgentProposalResolutionApproved {
		t.Fatalf("resolution = %q, want approved (annotation present = %v)",
			prop.Status.Resolution,
			prop.Annotations[eventingv1alpha1.ApprovalAnnotation] == "envtest@example.com")
	}
	if prop.Status.ApprovedBy != "envtest@example.com" {
		t.Errorf("approvedBy = %q", prop.Status.ApprovedBy)
	}
	if prop.Status.ApprovedAt == nil {
		t.Error("approvedAt unset")
	}

	// And the .approved CE Event landed in etcd.
	events, err := kc.CoreV1().Events(ns).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var found *corev1.Event
	for i := range events.Items {
		if events.Items[i].Annotations[schema.AnnotationType] == "agent.example.do.thing.approved" {
			found = &events.Items[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("no .approved CE Event found in %d Events", len(events.Items))
	}
	if found.InvolvedObject.Kind != "AgentProposal" || found.InvolvedObject.Name != "envtest-01" {
		t.Errorf("involvedObject = %+v", found.InvolvedObject)
	}
	if found.Annotations[schema.AnnotationCorrelationID] != "01HZ-ENVTEST" {
		t.Errorf("correlation propagation: %q", found.Annotations[schema.AnnotationCorrelationID])
	}
}
