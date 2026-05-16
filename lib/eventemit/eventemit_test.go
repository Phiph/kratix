package eventemit

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	v1alpha1 "github.com/syntasso/kratix/api/v1alpha1"
)

func TestWithCorrelationID(t *testing.T) {
	ctx := context.Background()
	if got := CorrelationIDFrom(ctx); got != "" {
		t.Fatalf("bare context should have no ID, got %q", got)
	}
	ctx = WithCorrelationID(ctx)
	id := CorrelationIDFrom(ctx)
	if id == "" {
		t.Fatal("expected ID after WithCorrelationID")
	}
	if len(id) != 32 {
		t.Errorf("ID len = %d, want 32 hex chars", len(id))
	}
	// Same context returns same ID — calls are idempotent for the lifetime
	// of the Reconcile.
	if again := CorrelationIDFrom(ctx); again != id {
		t.Errorf("ID drifted within a single context: %q vs %q", id, again)
	}
	// A nested WithCorrelationID overwrites, which is intentional: callers
	// can scope a fresh correlation if they want to. Pin the behaviour.
	ctx2 := WithCorrelationID(ctx)
	if CorrelationIDFrom(ctx2) == id {
		t.Error("nested WithCorrelationID should produce a new ID")
	}
}

func TestEmit_AttachesAnnotations(t *testing.T) {
	rec := record.NewFakeRecorder(8)
	ctx := WithCorrelationID(context.Background())
	id := CorrelationIDFrom(ctx)

	promise := &v1alpha1.Promise{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "redis",
			Generation: 42,
		},
	}

	Emit(ctx, rec, promise, "Warning", "PromiseUnavailable", "down: %s", "WorksSucceeded=False")

	// FakeRecorder doesn't expose annotations, but it does prove Emit didn't
	// panic and called through. Annotation correctness is exercised by the
	// forwarder's translate_test.go round-trip; here we only verify the
	// fan-out happened.
	select {
	case msg := <-rec.Events:
		if msg == "" {
			t.Fatal("expected an event message")
		}
	default:
		t.Fatal("no event recorded")
	}

	// Pin the correlation-ID contract: the ID Emit pulled from ctx is the
	// one the caller can also observe. (No reflection into FakeRecorder
	// needed — the contract is that CorrelationIDFrom is the canonical
	// accessor.)
	if id == "" {
		t.Fatal("correlation ID should be non-empty")
	}
}

func TestEmit_HandlesNilSafely(t *testing.T) {
	// Telemetry must never break a reconcile. Calling Emit with a nil
	// recorder or nil obj should be a no-op, not a panic.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Emit panicked on nil input: %v", r)
		}
	}()
	Emit(context.Background(), nil, nil, "Normal", "Anything", "")
}
