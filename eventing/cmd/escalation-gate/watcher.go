package main

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	eventingv1alpha1 "github.com/syntasso/kratix/eventing/api/v1alpha1"
	"github.com/syntasso/kratix/eventing/pkg/schema"
)

// watcher periodically scans AgentProposals and drives them through their
// state machine:
//
//	(materialised) -> .approved annotation appears -> emit .approved CE,
//	                                                  set status.resolution=approved
//	(materialised) -> spec.expiresAt passes        -> emit .expired CE,
//	                                                  set status.resolution=expired
//
// v0.1 uses a polling sweep rather than a watch — the gate is the only
// writer of status, and the expected proposal rate is low (human-paced).
// A watch-based loop would be a v0.2 optimisation.
//
// The watcher is deliberately *not* the receiver: receiver creates CRs;
// watcher resolves them. Two small loops are easier to reason about than
// one big informer-based controller for v0.1.
type watcher struct {
	cli       client.Client
	kc        kubernetes.Interface // for emitting CE Events
	namespace string
	interval  time.Duration
	now       func() time.Time
	log       *slog.Logger
}

func newWatcher(cli client.Client, kc kubernetes.Interface, namespace string, interval time.Duration, log *slog.Logger) *watcher {
	return &watcher{
		cli:       cli,
		kc:        kc,
		namespace: namespace,
		interval:  interval,
		now:       time.Now,
		log:       log,
	}
}

func (w *watcher) Run(ctx context.Context) {
	t := time.NewTicker(w.interval)
	defer t.Stop()
	w.log.Info("watcher started", "interval", w.interval.String())
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := w.tick(ctx); err != nil {
				w.log.Warn("watcher tick failed", "err", err)
			}
		}
	}
}

// tick scans every unresolved AgentProposal in the watcher's namespace
// and advances it where possible. Side-effects (CE emissions, status
// updates) are best-effort — failures log and continue.
func (w *watcher) tick(ctx context.Context) error {
	var list eventingv1alpha1.AgentProposalList
	if err := w.cli.List(ctx, &list, client.InNamespace(w.namespace)); err != nil {
		return fmt.Errorf("list proposals: %w", err)
	}
	for i := range list.Items {
		prop := &list.Items[i]
		if prop.Status.Resolution != "" {
			continue // already resolved
		}
		if err := w.advance(ctx, prop); err != nil {
			w.log.Warn("advance proposal", "name", prop.Name, "err", err)
		}
	}
	return nil
}

// advance moves a single proposal forward by one step if anything changed.
// Approval beats expiry: if both conditions are true in the same tick, the
// proposal resolves as approved. This matches the design doc's "first
// observation wins" rule.
func (w *watcher) advance(ctx context.Context, prop *eventingv1alpha1.AgentProposal) error {
	approver := prop.GetAnnotations()[eventingv1alpha1.ApprovalAnnotation]
	if approver != "" {
		return w.resolveApproved(ctx, prop, approver)
	}
	if w.now().After(prop.Spec.ExpiresAt.Time) {
		return w.resolveExpired(ctx, prop)
	}
	return nil
}

func (w *watcher) resolveApproved(ctx context.Context, prop *eventingv1alpha1.AgentProposal, approver string) error {
	if err := w.emitCE(ctx, prop, approvedTypeForProposed(prop.Spec.ProposedEventType), corev1.EventTypeNormal, "AgentProposalApproved",
		fmt.Sprintf("Proposal %s approved by %s", prop.Name, approver)); err != nil {
		return fmt.Errorf("emit approved: %w", err)
	}
	now := metav1.NewTime(w.now())
	prop.Status.ApprovedBy = approver
	prop.Status.ApprovedAt = &now
	prop.Status.Resolution = eventingv1alpha1.AgentProposalResolutionApproved
	prop.Status.ResolvedAt = &now
	prop.Status.Conditions = upsertCondition(prop.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             "Approved",
		Message:            fmt.Sprintf("Approved by %s", approver),
		LastTransitionTime: now,
	})
	if err := w.cli.Status().Update(ctx, prop); err != nil {
		return fmt.Errorf("status update: %w", err)
	}
	w.log.Info("proposal approved", "name", prop.Name, "by", approver)
	return nil
}

func (w *watcher) resolveExpired(ctx context.Context, prop *eventingv1alpha1.AgentProposal) error {
	if err := w.emitCE(ctx, prop, expiredTypeForProposed(prop.Spec.ProposedEventType), corev1.EventTypeNormal, "AgentProposalExpired",
		fmt.Sprintf("Proposal %s expired without approval", prop.Name)); err != nil {
		return fmt.Errorf("emit expired: %w", err)
	}
	now := metav1.NewTime(w.now())
	prop.Status.Resolution = eventingv1alpha1.AgentProposalResolutionExpired
	prop.Status.ResolvedAt = &now
	prop.Status.Conditions = upsertCondition(prop.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             "Expired",
		Message:            "Proposal expired without approval",
		LastTransitionTime: now,
	})
	if err := w.cli.Status().Update(ctx, prop); err != nil {
		return fmt.Errorf("status update: %w", err)
	}
	w.log.Info("proposal expired", "name", prop.Name)
	return nil
}

// emitCE writes a Kubernetes Event with the kratix.io/ce-* annotation set
// that the forwarder needs to fan it out. We don't import lib/eventemit
// because the gate is non-Kratix-controller: it has no per-Reconcile
// context, and its correlation IDs link a *proposal*, not a reconcile loop.
func (w *watcher) emitCE(ctx context.Context, prop *eventingv1alpha1.AgentProposal, ceType, eventType, reason, message string) error {
	if w.kc == nil {
		// Test mode: nil client means caller wants emission disabled.
		return nil
	}
	now := metav1.NewTime(w.now())
	ev := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "escalation-gate-",
			Namespace:    prop.Namespace,
			Annotations: map[string]string{
				schema.AnnotationCorrelationID: prop.Spec.CorrelationID,
				schema.AnnotationGeneration:    strconv.FormatInt(prop.GetGeneration(), 10),
				schema.AnnotationType:          ceType,
				// Carry the proposalId on the data annotation so consumers
				// can correlate the .approved/.expired event back to the
				// original .proposed event by ID, not just type+subject.
				"kratix.io/ce-data": fmt.Sprintf(`{"proposalId":%q}`, prop.Name),
			},
		},
		InvolvedObject: corev1.ObjectReference{
			Kind:       "AgentProposal",
			APIVersion: eventingv1alpha1.GroupVersion.String(),
			Name:       prop.Name,
			Namespace:  prop.Namespace,
		},
		Reason:              reason,
		Message:             message,
		Type:                eventType,
		Source:              corev1.EventSource{Component: "escalation-gate"},
		FirstTimestamp:      now,
		LastTimestamp:       now,
		EventTime:           metav1.NewMicroTime(now.Time),
		ReportingController: "escalation-gate",
		// The real apiserver requires both reportingInstance and action to
		// be non-empty for EventTime-based Events. The fake client doesn't
		// enforce this — without envtest the gap stays silent until prod.
		ReportingInstance: prop.Name,
		Action:            reason,
	}
	_, err := w.kc.CoreV1().Events(prop.Namespace).Create(ctx, ev, metav1.CreateOptions{})
	return err
}

// upsertCondition is a tiny replacement for meta.SetStatusCondition that
// avoids importing the apimachinery util just for one helper.
func upsertCondition(existing []metav1.Condition, c metav1.Condition) []metav1.Condition {
	for i := range existing {
		if existing[i].Type == c.Type {
			existing[i] = c
			return existing
		}
	}
	return append(existing, c)
}
