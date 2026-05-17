package dispatch

import (
	"time"

	"github.com/go-logr/logr"
	"k8s.io/utils/clock"

	"github.com/syntasso/kratix/api/v1alpha1"
)

// BackendFactory constructs a Backend from a state-store spec and credentials.
// Injected via DispatcherConfig so tests can stub backends.
type BackendFactory func(logger logr.Logger, dest DestinationKey, spec any, creds map[string][]byte) (Backend, error)

// DispatcherConfig configures a Dispatcher instance. Zero values are filled
// in with package defaults by NewDispatcher.
type DispatcherConfig struct {
	// BatchWindow is the max time a worker waits for more intents after the
	// first arrival before firing the batch. Default: 500ms.
	BatchWindow time.Duration

	// BatchMaxSize is the max number of intents in one batch. When reached,
	// fire immediately regardless of timer. Default: 100.
	BatchMaxSize int

	// SubmitTimeout caps how long Submit blocks waiting for the worker to
	// accept its intent. Default: 30s.
	SubmitTimeout time.Duration

	// DecideTimeout caps how long a single Decide callback may run. Default: 5s.
	DecideTimeout time.Duration

	// InboundBufferSize is the buffer size of each worker's inbound channel.
	// Default: 1000.
	InboundBufferSize int

	// NewGitBackend constructs a GitBackend for a destination. Required at
	// dispatcher construction; tests may stub.
	NewGitBackend func(logger logr.Logger, dest DestinationKey, spec v1alpha1.GitStateStoreSpec, creds map[string][]byte) (Backend, error)

	// NewS3Backend constructs an S3Backend for a destination. Required at
	// dispatcher construction; tests may stub.
	NewS3Backend func(logger logr.Logger, dest DestinationKey, spec v1alpha1.BucketStateStoreSpec, creds map[string][]byte) (Backend, error)

	// Clock is injected so batch timing is testable. Default: real clock.
	Clock clock.Clock

	// Logger is the root logger; workers derive child loggers per destination.
	Logger logr.Logger
}
