package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	eventingv1alpha1 "github.com/syntasso/kratix/eventing/api/v1alpha1"
)

// receiver accepts CloudEvent POSTs on /proposals and creates the matching
// AgentProposal CR. Idempotent on the proposalId: a duplicate post returns
// 200 instead of 202 and does not mutate the existing CR's spec.
type receiver struct {
	cli       client.Client
	namespace string // proposal CR namespace; v0.1: single global namespace per gate instance
	log       *slog.Logger
}

func newReceiver(cli client.Client, namespace string, log *slog.Logger) *receiver {
	return &receiver{cli: cli, namespace: namespace, log: log}
}

func (r *receiver) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/proposals", r.handleProposal)
	return mux
}

func (r *receiver) handleProposal(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer req.Body.Close()
	body, err := io.ReadAll(io.LimitReader(req.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}

	prop, err := proposalFromCE(body, r.namespace)
	if err != nil {
		if isReject(err) {
			r.log.Warn("reject proposal", "err", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		r.log.Error("proposal parse failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	status, err := r.materialise(req.Context(), &prop)
	if err != nil {
		r.log.Error("materialise failed", "err", err, "proposalId", prop.Name)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(status)
}

// materialise creates the AgentProposal CR. Returns:
//   - 202 (Accepted) on create
//   - 200 (OK) on dedup (already exists with same spec)
//   - 409 (Conflict) on dedup with mismatched spec — same proposalId, different
//     content, which usually indicates a bug in the proposing agent
//   - non-nil error on transient failure (the caller maps to 500)
func (r *receiver) materialise(ctx context.Context, prop *eventingv1alpha1.AgentProposal) (int, error) {
	if err := r.cli.Create(ctx, prop); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return 0, err
		}
		// Already there — fetch and check whether it matches what we'd have
		// written. Mismatched spec = the proposing agent is re-using a
		// proposalId for different content, which is a contract violation.
		existing := &eventingv1alpha1.AgentProposal{}
		if err := r.cli.Get(ctx, client.ObjectKey{Namespace: prop.Namespace, Name: prop.Name}, existing); err != nil {
			return 0, err
		}
		if !specsEqual(existing.Spec, prop.Spec) {
			r.log.Warn("proposalId re-used with different spec", "proposalId", prop.Name)
			return http.StatusConflict, nil
		}
		return http.StatusOK, nil
	}
	// Annotate the Ready=Unknown condition so consumers see the proposal
	// transition through a defined state machine. The watcher updates this
	// to Ready=False on resolution.
	prop.Status.Conditions = []metav1.Condition{{
		Type:               "Ready",
		Status:             metav1.ConditionUnknown,
		Reason:             "AwaitingApproval",
		Message:            "Proposal materialised; awaiting approval",
		LastTransitionTime: metav1.Now(),
	}}
	// Best-effort status update — failure here doesn't invalidate the
	// proposal (the spec is the audit object). The next watcher tick will
	// reconcile any drift.
	if err := r.cli.Status().Update(ctx, prop); err != nil && !errors.Is(err, context.Canceled) {
		r.log.Warn("initial status update failed", "err", err, "proposalId", prop.Name)
	}
	return http.StatusAccepted, nil
}

// specsEqual reports whether two AgentProposalSpecs are equivalent for the
// purpose of idempotency. We compare the load-bearing fields only — small
// stylistic differences in the plan blob (e.g. whitespace) should not
// cause a conflict.
func specsEqual(a, b eventingv1alpha1.AgentProposalSpec) bool {
	if a.ProposedEventType != b.ProposedEventType ||
		a.Actor != b.Actor ||
		a.Subject != b.Subject ||
		a.Action != b.Action ||
		!a.ExpiresAt.Equal(&b.ExpiresAt) {
		return false
	}
	// Ignore rationale text and plan body for equivalence — agents may
	// regenerate human-readable strings differently across retries.
	return true
}

// stripLeadingSlash makes route matching forgiving — defensive but cheap.
func stripLeadingSlash(s string) string { return strings.TrimPrefix(s, "/") }
