package dispatch

import (
	"context"
	"sync"

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

		pending := []pendingIntent{first}

		timer := w.clock.NewTimer(w.cfg.BatchWindow)

	drain:
		for len(pending) < w.cfg.BatchMaxSize {
			select {
			case p := <-w.inbound:
				pending = append(pending, p)
			case <-timer.C():
				break drain
			case <-w.stopCh:
				timer.Stop()
				break drain
			}
		}
		timer.Stop()

		w.fireBatch(pending)
	}
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

		writes, err := p.intent.Decide(intentReads)
		if err != nil {
			p.resultCh <- SubmitResult{Err: err}
			continue
		}

		key := p.intent.WorkPlacement + "|" + p.intent.SubDir
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
