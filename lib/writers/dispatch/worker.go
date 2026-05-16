package dispatch

import (
	"context"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/utils/clock"
)

// SubmitResult is what the worker sends back to a Submit caller via the
// caller-provided result channel.
type SubmitResult struct {
	// Result is the outcome of the batch for this intent on success.
	Result Result
	// Err is non-nil if the intent failed.
	Err error
}

// pendingIntent ties an Intent to the channel that should receive its result.
type pendingIntent struct {
	intent   Intent
	resultCh chan<- SubmitResult
}

// Worker drains intents for a single destination, batches them, and applies
// them via a Backend. One Worker per DestinationKey.
type Worker struct {
	dest    DestinationKey
	backend Backend
	cfg     DispatcherConfig
	clock   clock.Clock
	logger  logr.Logger

	inbound chan pendingIntent
	stopCh  chan struct{}
	doneCh  chan struct{}

	mu      sync.Mutex
	stopped bool
}

// NewWorker creates a Worker and starts its goroutine. Callers must call
// Stop to release the goroutine.
func NewWorker(dest DestinationKey, backend Backend, cfg DispatcherConfig) *Worker {
	if cfg.Clock == nil {
		cfg.Clock = clock.RealClock{}
	}
	if cfg.InboundBufferSize == 0 {
		cfg.InboundBufferSize = 1000
	}
	if cfg.DecideTimeout == 0 {
		cfg.DecideTimeout = 5 * time.Second
	}
	w := &Worker{
		dest:    dest,
		backend: backend,
		cfg:     cfg,
		clock:   cfg.Clock,
		logger:  cfg.Logger.WithValues("destination", dest),
		inbound: make(chan pendingIntent, cfg.InboundBufferSize),
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
	}
	go w.run()
	return w
}

// Submit enqueues an intent. The worker will signal resultCh exactly once
// with the outcome. Returns an error if the worker has been stopped or the
// context is done before the intent is accepted.
func (w *Worker) Submit(ctx context.Context, intent Intent, resultCh chan<- SubmitResult) error {
	w.mu.Lock()
	if w.stopped {
		w.mu.Unlock()
		return ErrShuttingDown
	}
	w.mu.Unlock()

	select {
	case w.inbound <- pendingIntent{intent: intent, resultCh: resultCh}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-w.stopCh:
		return ErrShuttingDown
	}
}

// Stop signals the worker to drain and exit. Blocks until the worker
// goroutine has returned. Safe to call multiple times.
func (w *Worker) Stop() {
	w.mu.Lock()
	if w.stopped {
		w.mu.Unlock()
		<-w.doneCh
		return
	}
	w.stopped = true
	close(w.stopCh)
	w.mu.Unlock()
	<-w.doneCh
}

// run is the worker's main loop.
func (w *Worker) run() {
	defer close(w.doneCh)

	for {
		var first pendingIntent
		select {
		case first = <-w.inbound:
		case <-w.stopCh:
			return
		}

		pending := map[string]pendingIntent{
			dedupKey(first.intent): first,
		}

		timer := w.clock.NewTimer(w.cfg.BatchWindow)

	drain:
		for len(pending) < w.cfg.BatchMaxSize {
			select {
			case p := <-w.inbound:
				key := dedupKey(p.intent)
				if old, exists := pending[key]; exists {
					old.resultCh <- SubmitResult{Err: ErrSuperseded}
				}
				pending[key] = p
			case <-timer.C():
				break drain
			case <-w.stopCh:
				timer.Stop()
				break drain
			}
		}
		timer.Stop()

		// Snapshot to a slice in iteration order for the batch.
		batch := make([]pendingIntent, 0, len(pending))
		for _, p := range pending {
			batch = append(batch, p)
		}
		w.fireBatch(batch)
	}
}

// dedupKey is the canonical (WorkPlacement, SubDir) key for batching/dedup.
func dedupKey(intent Intent) string {
	return intent.WorkPlacement + "|" + intent.SubDir
}

// fireBatch resolves the pending intents via their Decide callbacks and
// applies the resulting batch via the backend, distributing per-intent
// results back to callers.
func (w *Worker) fireBatch(pending []pendingIntent) {
	ctx := context.Background()

	// Collect unique read paths across the batch.
	readSet := map[string]struct{}{}
	for _, p := range pending {
		for _, path := range p.intent.Reads {
			readSet[path] = struct{}{}
		}
	}
	allPaths := make([]string, 0, len(readSet))
	for path := range readSet {
		allPaths = append(allPaths, path)
	}

	var reads map[string][]byte
	if len(allPaths) > 0 {
		var err error
		reads, err = w.backend.Read(ctx, allPaths)
		if err != nil {
			for _, p := range pending {
				p.resultCh <- SubmitResult{Err: err}
			}
			return
		}
	}

	// Resolve each intent: slice its reads, run Decide, quarantine on error.
	resolved := make([]ResolvedIntent, 0, len(pending))
	owners := make([]pendingIntent, 0, len(pending))
	for _, p := range pending {
		intentReads := map[string][]byte{}
		for _, path := range p.intent.Reads {
			if v, ok := reads[path]; ok {
				intentReads[path] = v
			}
		}

		decideCtx, cancel := context.WithTimeout(ctx, w.cfg.DecideTimeout)
		writes, err := runDecide(decideCtx, p.intent.Decide, intentReads)
		cancel()
		if err != nil {
			p.resultCh <- SubmitResult{Err: err}
			continue
		}

		key := dedupKey(p.intent)
		resolved = append(resolved, ResolvedIntent{
			Key:           key,
			WorkPlacement: p.intent.WorkPlacement,
			SubDir:        p.intent.SubDir,
			Writes:        writes,
		})
		owners = append(owners, p)
	}

	if len(resolved) == 0 {
		return
	}

	res := w.backend.ApplyBatch(ctx, resolved)

	for i, ri := range resolved {
		owner := owners[i]
		if errVal, ok := res.PerIntent[ri.Key]; ok && errVal != nil {
			owner.resultCh <- SubmitResult{Err: errVal}
		} else {
			owner.resultCh <- SubmitResult{Result: Result{VersionID: res.VersionID}}
		}
	}
}

// runDecide invokes the user-supplied Decide on a separate goroutine so the
// caller can enforce a timeout via context. If the context fires first,
// runDecide returns ctx.Err(); the Decide goroutine continues until it
// finishes on its own. Decide is expected to be CPU-only by convention; the
// runtime cost of an orphaned goroutine is the user's problem.
func runDecide(ctx context.Context, fn func(map[string][]byte) (Writes, error), reads map[string][]byte) (Writes, error) {
	type result struct {
		writes Writes
		err    error
	}
	done := make(chan result, 1)
	go func() {
		w, e := fn(reads)
		done <- result{writes: w, err: e}
	}()
	select {
	case r := <-done:
		return r.writes, r.err
	case <-ctx.Done():
		return Writes{}, ctx.Err()
	}
}
