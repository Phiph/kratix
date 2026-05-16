// Package dispatch provides a per-destination write queue that batches
// concurrent state-store writes into one push (git) or one parallel apply
// (S3) per destination. See docs/superpowers/specs/2026-05-16-write-queue-design.md.
package dispatch

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"k8s.io/utils/clock"

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
	// RegisterGitDestination binds a Git state-store spec and credentials to
	// a DestinationKey so subsequent Submit and Validate calls can lazy-
	// construct a backend. Called by the GitStateStore controller during its
	// reconcile (where the spec and creds are already in hand). Re-registering
	// the same key overwrites the previous spec and credentials.
	RegisterGitDestination(key DestinationKey, spec v1alpha1.GitStateStoreSpec, creds map[string][]byte) error

	// RegisterS3Destination is the S3 counterpart of RegisterGitDestination.
	RegisterS3Destination(key DestinationKey, spec v1alpha1.BucketStateStoreSpec, creds map[string][]byte) error

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

//counterfeiter:generate . Dispatcher

// dispatcher is the concrete Dispatcher.
type dispatcher struct {
	cfg DispatcherConfig

	mu       sync.Mutex
	workers  map[DestinationKey]*Worker
	specs    map[DestinationKey]registeredSpec
	shutdown bool
}

// registeredSpec holds the state-store spec and credentials supplied via
// RegisterGitDestination / RegisterS3Destination. Exactly one of gitSpec
// or s3Spec is non-nil.
type registeredSpec struct {
	gitSpec *v1alpha1.GitStateStoreSpec
	s3Spec  *v1alpha1.BucketStateStoreSpec
	creds   map[string][]byte
}

// NewDispatcher constructs a Dispatcher with the supplied configuration.
// Zero-valued config fields are filled with package defaults.
func NewDispatcher(cfg DispatcherConfig) Dispatcher {
	if cfg.BatchWindow == 0 {
		cfg.BatchWindow = 500 * time.Millisecond
	}
	if cfg.BatchMaxSize == 0 {
		cfg.BatchMaxSize = 100
	}
	if cfg.SubmitTimeout == 0 {
		cfg.SubmitTimeout = 30 * time.Second
	}
	if cfg.DecideTimeout == 0 {
		cfg.DecideTimeout = 5 * time.Second
	}
	if cfg.InboundBufferSize == 0 {
		cfg.InboundBufferSize = 1000
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.RealClock{}
	}
	return &dispatcher{
		cfg:     cfg,
		workers: map[DestinationKey]*Worker{},
		specs:   map[DestinationKey]registeredSpec{},
	}
}

// RegisterGitDestination binds a Git state-store spec and credentials to dest.
func (d *dispatcher) RegisterGitDestination(key DestinationKey, spec v1alpha1.GitStateStoreSpec, creds map[string][]byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.shutdown {
		return ErrShuttingDown
	}
	d.specs[key] = registeredSpec{gitSpec: &spec, creds: creds}
	return nil
}

// RegisterS3Destination binds an S3 state-store spec and credentials to dest.
func (d *dispatcher) RegisterS3Destination(key DestinationKey, spec v1alpha1.BucketStateStoreSpec, creds map[string][]byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.shutdown {
		return ErrShuttingDown
	}
	d.specs[key] = registeredSpec{s3Spec: &spec, creds: creds}
	return nil
}

// Submit enqueues an intent and blocks until its batch completes.
func (d *dispatcher) Submit(ctx context.Context, dest DestinationKey, intent Intent) (Result, error) {
	w, err := d.getOrCreateWorker(dest)
	if err != nil {
		return Result{}, err
	}
	resultCh := make(chan SubmitResult, 1)
	if err := w.Submit(ctx, intent, resultCh); err != nil {
		return Result{}, err
	}
	select {
	case r := <-resultCh:
		return r.Result, r.Err
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}

// Validate checks credentials and write permissions against dest using a
// throwaway backend.
func (d *dispatcher) Validate(ctx context.Context, dest DestinationKey) error {
	b, err := d.constructBackend(dest)
	if err != nil {
		return err
	}
	defer func() { _ = b.Close() }()
	return b.Validate(ctx)
}

// Cleanup tears down the worker for dest. Pending intents fail with
// ErrDestinationGone via the worker's stop path.
func (d *dispatcher) Cleanup(dest DestinationKey) error {
	d.mu.Lock()
	w, ok := d.workers[dest]
	if ok {
		delete(d.workers, dest)
	}
	delete(d.specs, dest)
	d.mu.Unlock()
	if w != nil {
		w.StopWithReason(ErrDestinationGone)
	}
	return nil
}

// Shutdown drains all workers and stops the dispatcher.
func (d *dispatcher) Shutdown(ctx context.Context) error {
	d.mu.Lock()
	d.shutdown = true
	workers := make([]*Worker, 0, len(d.workers))
	for _, w := range d.workers {
		workers = append(workers, w)
	}
	d.workers = map[DestinationKey]*Worker{}
	d.mu.Unlock()

	done := make(chan struct{})
	go func() {
		for _, w := range workers {
			w.Stop()
		}
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *dispatcher) getOrCreateWorker(dest DestinationKey) (*Worker, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.shutdown {
		return nil, ErrShuttingDown
	}
	if w, ok := d.workers[dest]; ok {
		return w, nil
	}
	b, err := d.constructBackendLocked(dest)
	if err != nil {
		return nil, err
	}
	w := NewWorker(dest, b, d.cfg)
	d.workers[dest] = w
	return w, nil
}

func (d *dispatcher) constructBackend(dest DestinationKey) (Backend, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.constructBackendLocked(dest)
}

func (d *dispatcher) constructBackendLocked(dest DestinationKey) (Backend, error) {
	spec, ok := d.specs[dest]
	if !ok {
		return nil, fmt.Errorf("destination not registered: %+v", dest)
	}
	switch {
	case spec.gitSpec != nil:
		if d.cfg.NewGitBackend == nil {
			return nil, errors.New("NewGitBackend not configured")
		}
		return d.cfg.NewGitBackend(d.cfg.Logger, dest, *spec.gitSpec, spec.creds)
	case spec.s3Spec != nil:
		if d.cfg.NewS3Backend == nil {
			return nil, errors.New("NewS3Backend not configured")
		}
		return d.cfg.NewS3Backend(d.cfg.Logger, dest, *spec.s3Spec, spec.creds)
	default:
		return nil, fmt.Errorf("destination %+v has no spec", dest)
	}
}
