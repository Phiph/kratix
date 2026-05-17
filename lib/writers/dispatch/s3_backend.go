package dispatch

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-logr/logr"

	"github.com/syntasso/kratix/api/v1alpha1"
	"github.com/syntasso/kratix/lib/writers"
)

// S3Backend implements Backend by wrapping writers.S3Writer. It does not
// maintain cached state between calls; each operation runs against the
// bucket directly.
//
// Unlike GitBackend, S3Backend's underlying S3Writer is stateless (just
// holds the minio client config), so concurrent calls are safe at the
// writer layer. The dispatcher's worker model still serialises calls per
// destination — that's fine, but does mean we're not exploiting S3's
// natural parallelism today. See Task 28's TODO for the deferred perf
// work (batched DeleteObjects, parallel puts, shared ListObjects).
type S3Backend struct {
	logger logr.Logger
	dest   DestinationKey
	writer *writers.S3Writer
}

// NewS3Backend constructs an S3Backend. Sets up the underlying minio client.
func NewS3Backend(logger logr.Logger, dest DestinationKey, spec v1alpha1.BucketStateStoreSpec, creds map[string][]byte) (Backend, error) {
	w, err := writers.NewS3Writer(logger, spec, dest.Path, creds)
	if err != nil {
		return nil, fmt.Errorf("s3 backend: create writer: %w", err)
	}
	sw, ok := w.(*writers.S3Writer)
	if !ok {
		return nil, errors.New("s3 backend: writer is not *S3Writer")
	}
	return &S3Backend{
		logger: logger.WithValues("backend", "s3", "destination", dest),
		dest:   dest,
		writer: sw,
	}, nil
}

// Read reads each path from the bucket. Missing paths are absent from the
// returned map.
func (s *S3Backend) Read(_ context.Context, paths []string) (map[string][]byte, error) {
	out := make(map[string][]byte, len(paths))
	for _, p := range paths {
		data, err := s.writer.ReadFile(p)
		if err != nil {
			if errors.Is(err, writers.ErrFileNotFound) {
				continue
			}
			return nil, fmt.Errorf("s3 backend: read %s: %w", p, err)
		}
		out[p] = data
	}
	return out, nil
}

// Validate delegates to the underlying writer's permission check.
func (s *S3Backend) Validate(_ context.Context) error {
	return s.writer.ValidatePermissions()
}

// Close is a no-op. The underlying minio client has no resources to release.
func (s *S3Backend) Close() error { return nil }

// ApplyBatch applies the batch by calling UpdateFiles per intent. The
// current implementation is sequential; perf optimisations (batched
// DeleteObjects, parallel puts) are deferred per spec §5.2.
func (s *S3Backend) ApplyBatch(_ context.Context, batch []ResolvedIntent) BatchResult {
	res := BatchResult{PerIntent: make(map[string]error, len(batch))}
	for _, ri := range batch {
		_, err := s.writer.UpdateFiles(ri.SubDir, ri.WorkPlacement, ri.Writes.ToCreate, ri.Writes.ToDelete)
		res.PerIntent[ri.Key] = err
	}
	return res
}
