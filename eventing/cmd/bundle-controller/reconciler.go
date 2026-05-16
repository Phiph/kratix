package main

import (
	"context"
	"fmt"
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kratixv1alpha1 "github.com/syntasso/kratix/api/v1alpha1"
	eventingv1alpha1 "github.com/syntasso/kratix/eventing/api/v1alpha1"
)

// Reconciler drives PromiseBundles toward their declared state:
//
//  1. Look up the referenced Promise.
//  2. If the Promise is not Available, set Ready=False/PromiseNotReady and
//     requeue with a backoff.
//  3. Otherwise, decode each Companion and server-side-apply it with the
//     PromiseBundle as owner.
//  4. Aggregate per-companion outcomes into status.companions, then set
//     Ready=True/Applied or Ready=False/PartiallyApplied.
//
// Deletion is handled by Kubernetes garbage collection via owner refs —
// no finalizer needed in v0.1.
type Reconciler struct {
	client.Client
	FieldManager string
	Now          func() time.Time
}

const (
	conditionReady             = "Ready"
	reasonPromiseMissing       = "PromiseNotFound"
	reasonPromiseNotReady      = "PromiseNotReady"
	reasonApplied              = "Applied"
	reasonPartiallyApplied     = "PartiallyApplied"
	defaultFieldManager        = "kratix-bundle-controller"
	notReadyRequeueAfter       = 30 * time.Second
	partiallyAppliedRequeueAfter = 1 * time.Minute
)

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("bundle", req.Name)
	if r.FieldManager == "" {
		r.FieldManager = defaultFieldManager
	}
	if r.Now == nil {
		r.Now = time.Now
	}

	var bundle eventingv1alpha1.PromiseBundle
	if err := r.Get(ctx, req.NamespacedName, &bundle); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	promise := &kratixv1alpha1.Promise{}
	if err := r.Get(ctx, client.ObjectKey{Name: bundle.Spec.PromiseRef.Name}, promise); err != nil {
		if apierrors.IsNotFound(err) {
			r.setReady(&bundle, metav1.ConditionFalse, reasonPromiseMissing,
				fmt.Sprintf("Promise %q not found", bundle.Spec.PromiseRef.Name))
			return r.persistStatus(ctx, &bundle, ctrl.Result{RequeueAfter: notReadyRequeueAfter}, logger)
		}
		return ctrl.Result{}, err
	}

	if promise.Status.Status != kratixv1alpha1.PromiseStatusAvailable {
		r.setReady(&bundle, metav1.ConditionFalse, reasonPromiseNotReady,
			fmt.Sprintf("Promise %q is not Available (status=%q); waiting", bundle.Spec.PromiseRef.Name, promise.Status.Status))
		bundle.Status.ObservedGeneration = bundle.Generation
		return r.persistStatus(ctx, &bundle, ctrl.Result{RequeueAfter: notReadyRequeueAfter}, logger)
	}

	// Apply every companion. We do not short-circuit on a single failure;
	// the goal is to report per-companion status so a producer can see
	// exactly which entry broke.
	statuses := make([]eventingv1alpha1.CompanionStatus, len(bundle.Spec.Companions))
	failedCount := 0
	for i, c := range bundle.Spec.Companions {
		statuses[i] = r.applyOne(ctx, &bundle, c, logger)
		if statuses[i].Result != eventingv1alpha1.CompanionApplied {
			failedCount++
		}
	}
	bundle.Status.Companions = statuses
	bundle.Status.ObservedGeneration = bundle.Generation

	if failedCount == 0 {
		r.setReady(&bundle, metav1.ConditionTrue, reasonApplied,
			fmt.Sprintf("All %d companions applied", len(bundle.Spec.Companions)))
		return r.persistStatus(ctx, &bundle, ctrl.Result{}, logger)
	}
	r.setReady(&bundle, metav1.ConditionFalse, reasonPartiallyApplied,
		fmt.Sprintf("%d of %d companions failed to apply", failedCount, len(bundle.Spec.Companions)))
	return r.persistStatus(ctx, &bundle, ctrl.Result{RequeueAfter: partiallyAppliedRequeueAfter}, logger)
}

func (r *Reconciler) applyOne(ctx context.Context, bundle *eventingv1alpha1.PromiseBundle, c eventingv1alpha1.Companion, logger logger) eventingv1alpha1.CompanionStatus {
	st := eventingv1alpha1.CompanionStatus{Name: c.Name, Result: eventingv1alpha1.CompanionPending}

	u, err := decodeCompanion(ctx, r.Client, bundle, c)
	if err != nil {
		st.Result = eventingv1alpha1.CompanionFailed
		st.Message = err.Error()
		return st
	}
	st.APIVersion = u.GetAPIVersion()
	st.Kind = u.GetKind()
	st.Namespace = u.GetNamespace()
	st.ResourceName = u.GetName()

	if err := r.serverSideApply(ctx, u); err != nil {
		st.Result = eventingv1alpha1.CompanionFailed
		st.Message = err.Error()
		return st
	}
	now := metav1.NewTime(r.Now())
	st.Result = eventingv1alpha1.CompanionApplied
	st.LastAppliedAt = &now
	return st
}

// serverSideApply uses Patch with ApplyPatchType so the controller is a
// declarative co-owner of the field set it writes. This plays nicely with
// other field managers (e.g. an operator writing status, the producer
// editing the manifest, etc.).
func (r *Reconciler) serverSideApply(ctx context.Context, u *unstructured.Unstructured) error {
	gvk := u.GroupVersionKind()
	if gvk.Empty() {
		return fmt.Errorf("decoded object missing GVK")
	}
	// Patch with apply semantics: name + namespace + GVK are the target;
	// the full body is the desired state.
	patch := client.Apply
	return r.Patch(ctx, u, patch, client.FieldOwner(r.FieldManager), client.ForceOwnership)
}

func (r *Reconciler) setReady(b *eventingv1alpha1.PromiseBundle, status metav1.ConditionStatus, reason, msg string) {
	now := metav1.NewTime(r.Now())
	cond := metav1.Condition{
		Type:               conditionReady,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		LastTransitionTime: now,
		ObservedGeneration: b.Generation,
	}
	b.Status.Conditions = upsertCondition(b.Status.Conditions, cond)
	// Keep conditions in a deterministic order (alphabetical by type) so
	// status diffs are stable.
	sort.Slice(b.Status.Conditions, func(i, j int) bool {
		return b.Status.Conditions[i].Type < b.Status.Conditions[j].Type
	})
}

func (r *Reconciler) persistStatus(ctx context.Context, b *eventingv1alpha1.PromiseBundle, result ctrl.Result, logger logger) (ctrl.Result, error) {
	if err := r.Status().Update(ctx, b); err != nil {
		logger.Error(err, "status update failed", "bundle", b.Name)
		return ctrl.Result{}, err
	}
	return result, nil
}

func upsertCondition(existing []metav1.Condition, c metav1.Condition) []metav1.Condition {
	for i := range existing {
		if existing[i].Type == c.Type {
			// Preserve LastTransitionTime when the status hasn't actually changed.
			if existing[i].Status == c.Status {
				c.LastTransitionTime = existing[i].LastTransitionTime
			}
			existing[i] = c
			return existing
		}
	}
	return append(existing, c)
}

// logger is a tiny interface to keep applyOne / persistStatus free of a
// hard logr import; tests pass a no-op.
type logger interface {
	Error(err error, msg string, kv ...any)
}

