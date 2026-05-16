// Package dispatch provides a per-destination write queue that batches
// concurrent state-store writes into one push (git) or one parallel apply
// (S3) per destination. See docs/superpowers/specs/2026-05-16-write-queue-design.md.
package dispatch

import (
	"context"
	"errors"

	"github.com/syntasso/kratix/api/v1alpha1"
)

//go:generate go run github.com/maxbrunsfeld/counterfeiter/v6 -generate

// Sentinel errors returned by Dispatcher.Submit. Errors from user-supplied
// Decide callbacks pass through unwrapped.
var (
	// ErrSuperseded means a newer intent for the same (WorkPlacement, SubDir)
	// replaced this one before the batch fired. Transient; caller should requeue.
	ErrSuperseded = errors.New("intent superseded by newer intent")

	// ErrDestinationGone means the destination's worker was removed via
	// Cleanup while this intent was queued. Terminal.
	ErrDestinationGone = errors.New("destination no longer exists")

	// ErrShuttingDown means Dispatcher.Shutdown was called while this intent
	// was queued. Transient; another pod will reconcile.
	ErrShuttingDown = errors.New("dispatcher shutting down")

	// ErrBatchFailed wraps a backend-layer failure that affected the entire batch.
	ErrBatchFailed = errors.New("batch apply failed")
)

// DestinationKey uniquely identifies a write target. Same key → same worker.
type DestinationKey struct {
	StateStoreKind string // "GitStateStore" | "BucketStateStore"
	StateStoreName string
	Branch         string // empty for S3
	Path           string // destinationPath
}

// Intent is a single unit of work submitted to the dispatcher.
type Intent struct {
	// WorkPlacement and SubDir form the dedup key within a worker.
	WorkPlacement string
	SubDir        string

	// Reads is the list of paths the Decide callback needs to read before
	// computing writes. Resolved by the worker against live state at batch
	// execute time.
	Reads []string

	// Decide computes writes given the read results. Called by the worker
	// inside the batch critical section. If Decide returns an error, this
	// intent is quarantined out of the batch and its caller receives the error.
	Decide func(reads map[string][]byte) (Writes, error)
}

// Writes is what Decide produces.
type Writes struct {
	ToCreate []v1alpha1.Workload
	ToDelete []string
}

// Result is what Submit returns to the caller on success.
type Result struct {
	// VersionID is backend-defined: git commit SHA, S3 version composite.
	// Empty if the batch made no changes.
	VersionID string
}

// Dispatcher is the entry point for all state-store writes.
// One instance per controller pod, shared by all reconcilers.
type Dispatcher interface {
	// Submit enqueues an intent and blocks until its batch completes.
	Submit(ctx context.Context, dest DestinationKey, intent Intent) (Result, error)

	// Validate checks credentials and write permissions against the destination
	// using a throwaway backend. Does not enqueue.
	Validate(ctx context.Context, dest DestinationKey) error

	// Cleanup tears down the worker for a destination. Pending intents
	// fail with ErrDestinationGone.
	Cleanup(dest DestinationKey) error

	// Shutdown drains all workers and stops. Called on pod shutdown.
	Shutdown(ctx context.Context) error
}
