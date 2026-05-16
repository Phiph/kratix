package main

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	eventingv1alpha1 "github.com/syntasso/kratix/eventing/api/v1alpha1"
)

func newClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	sch := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(sch))
	utilruntime.Must(eventingv1alpha1.AddToScheme(sch))
	return fake.NewClientBuilder().
		WithScheme(sch).
		WithStatusSubresource(&eventingv1alpha1.AgentProposal{}).
		WithObjects(objs...).
		Build()
}

func newProposal() *eventingv1alpha1.AgentProposal {
	return &eventingv1alpha1.AgentProposal{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "01HZ9A",
			Namespace: "ns",
		},
		Spec: eventingv1alpha1.AgentProposalSpec{
			ProposedEventType: "agent.x.y.proposed",
			Actor:             "agent/x/v1",
			Subject:           "ns/kind/name",
			Action:            "do",
			Rationale:         "because",
			ExpiresAt:         metav1.NewTime(time.Now().Add(time.Hour)),
		},
	}
}

func TestApprove_Happy(t *testing.T) {
	cli := newClient(t, newProposal())
	if err := approve(context.Background(), cli, "ns", "01HZ9A", "phill@example.com"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	got := &eventingv1alpha1.AgentProposal{}
	if err := cli.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "01HZ9A"}, got); err != nil {
		t.Fatal(err)
	}
	if got.Annotations[eventingv1alpha1.ApprovalAnnotation] != "phill@example.com" {
		t.Errorf("annotation = %q", got.Annotations[eventingv1alpha1.ApprovalAnnotation])
	}
}

func TestApprove_RefusesAlreadyResolved(t *testing.T) {
	prop := newProposal()
	prop.Status.Resolution = eventingv1alpha1.AgentProposalResolutionExpired
	cli := newClient(t, prop)
	err := approve(context.Background(), cli, "ns", "01HZ9A", "phill@example.com")
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expected refusal mentioning 'expired'; got %v", err)
	}
}

func TestApprove_RefusesIfExpired(t *testing.T) {
	prop := newProposal()
	prop.Spec.ExpiresAt = metav1.NewTime(time.Now().Add(-time.Hour))
	cli := newClient(t, prop)
	err := approve(context.Background(), cli, "ns", "01HZ9A", "phill@example.com")
	if err == nil || !strings.Contains(err.Error(), "expiresAt") {
		t.Fatalf("expected refusal mentioning 'expiresAt'; got %v", err)
	}
}

func TestApprove_RefusesDoubleApproval(t *testing.T) {
	prop := newProposal()
	prop.SetAnnotations(map[string]string{
		eventingv1alpha1.ApprovalAnnotation: "someone-else@example.com",
	})
	cli := newClient(t, prop)
	err := approve(context.Background(), cli, "ns", "01HZ9A", "phill@example.com")
	if err == nil || !strings.Contains(err.Error(), "already carries") {
		t.Fatalf("expected refusal for double-approve; got %v", err)
	}
}

func TestApprove_PropagatesNotFound(t *testing.T) {
	cli := newClient(t)
	err := approve(context.Background(), cli, "ns", "missing", "phill@example.com")
	if err == nil {
		t.Fatal("expected error for missing proposal")
	}
}
