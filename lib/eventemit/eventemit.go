// Package eventemit centralises the producer-side helpers Kratix controllers
// use to emit CloudEvent-bearing Kubernetes Events. It implements the
// kratix.io/ce-* annotation contract from eventing/WIRE-FORMAT.md without
// importing the eventing subsystem's CRD types — controllers only need the
// annotation keys and a correlation-ID generator.
//
// Usage from a Reconcile function:
//
//	ctx = eventemit.WithCorrelationID(ctx)
//	// ... later, on a transition ...
//	eventemit.Emit(ctx, recorder, obj, corev1.EventTypeWarning,
//	    "PromiseUnavailable", "Promise %s is unavailable: %s", name, reason)
package eventemit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
)

// Annotation keys mirror eventing/pkg/schema. Duplicated here on purpose:
// controllers must not import the eventing subsystem to use this package
// (see eventing/README.md "Boundaries").
const (
	annotationCorrelationID = "kratix.io/ce-correlation-id"
	annotationGeneration    = "kratix.io/ce-generation"
)

type correlationIDKey struct{}

// WithCorrelationID returns a child context carrying a freshly-generated
// correlation ID. Call this once at the top of Reconcile; every Emit using
// that context shares the same ID, which is what makes a reconciliation loop
// debuggable end-to-end on the consumer side.
func WithCorrelationID(ctx context.Context) context.Context {
	return context.WithValue(ctx, correlationIDKey{}, newID())
}

// CorrelationIDFrom returns the correlation ID carried by ctx, or "" if none
// is set. Callers normally do not need this — Emit reads it internally.
func CorrelationIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(correlationIDKey{}).(string)
	return v
}

// HasGeneration is the minimum interface an object must satisfy for Emit to
// attribute its kratix.io/ce-generation. All standard Kubernetes objects
// satisfy this via metav1.ObjectMeta.
type HasGeneration interface {
	runtime.Object
	GetGeneration() int64
}

// Emit publishes an annotated Kubernetes Event on obj. It is a thin wrapper
// around EventRecorder.AnnotatedEventf that attaches the kratix.io/ce-*
// annotations required by the wire format.
//
// The Event.reason MUST follow the PascalCase, kratix-namespaced convention
// (e.g. "PromiseUnavailable") — see WIRE-FORMAT.md §2. The eventType is one
// of corev1.EventTypeNormal or corev1.EventTypeWarning; it maps to the
// kratixseverity extension at the forwarder.
//
// If the context has no correlation ID (caller forgot WithCorrelationID),
// Emit generates a one-off ID rather than failing — best-effort, never break
// the reconcile loop on telemetry.
func Emit(
	ctx context.Context,
	recorder record.EventRecorder,
	obj HasGeneration,
	eventType, reason, messageFmt string,
	args ...interface{},
) {
	if recorder == nil || obj == nil {
		return
	}
	corrID := CorrelationIDFrom(ctx)
	if corrID == "" {
		corrID = newID()
	}
	annotations := map[string]string{
		annotationCorrelationID: corrID,
		annotationGeneration:    strconv.FormatInt(obj.GetGeneration(), 10),
	}
	recorder.AnnotatedEventf(obj, annotations, eventType, reason, messageFmt, args...)
}

// newID returns a fresh random hex ID. Matches the forwarder's ID-generation
// scheme — both are CE-extension-safe and uniqueness-only (no ordering).
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand.Read on Linux essentially never fails; if it does, the
		// caller is in deep trouble and ID quality is not the priority.
		return fmt.Sprintf("fallback-%d", b[0])
	}
	return hex.EncodeToString(b[:])
}

// Ensure corev1 stays referenced so a refactor that drops Emit's eventType
// parameter doesn't silently lose the constant-checker.
var _ = corev1.EventTypeWarning
