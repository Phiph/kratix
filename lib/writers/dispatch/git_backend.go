package dispatch

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-logr/logr"

	"github.com/syntasso/kratix/api/v1alpha1"
	"github.com/syntasso/kratix/lib/writers"
)

// GitBackend implements Backend by wrapping writers.GitWriter. Each GitBackend
// owns one cached git worktree (created at construction via the underlying
// GitWriter.Init) that lives for the backend's lifetime.
//
// GitBackend is not safe for concurrent use. Callers must serialise all method
// calls; the dispatcher's per-destination worker model does this.
type GitBackend struct {
	logger logr.Logger
	dest   DestinationKey
	writer *writers.GitWriter
	branch string
}

// NewGitBackend constructs a GitBackend. Clones the destination once.
func NewGitBackend(logger logr.Logger, dest DestinationKey, spec v1alpha1.GitStateStoreSpec, creds map[string][]byte) (Backend, error) {
	w, err := writers.NewGitWriter(logger, spec, dest.Path, creds)
	if err != nil {
		return nil, fmt.Errorf("git backend: create writer: %w", err)
	}
	gw, ok := w.(*writers.GitWriter)
	if !ok {
		return nil, errors.New("git backend: writer is not *GitWriter")
	}
	if _, err := gw.Init(dest.Branch); err != nil {
		return nil, fmt.Errorf("git backend: clone: %w", err)
	}
	return &GitBackend{
		logger: logger.WithValues("backend", "git", "destination", dest),
		dest:   dest,
		writer: gw,
		branch: dest.Branch,
	}, nil
}

// Read reads each path against the cached worktree. Missing paths are absent
// from the returned map.
func (g *GitBackend) Read(_ context.Context, paths []string) (map[string][]byte, error) {
	out := make(map[string][]byte, len(paths))
	for _, p := range paths {
		data, err := g.writer.ReadFile(p)
		if err != nil {
			if errors.Is(err, writers.ErrFileNotFound) {
				continue
			}
			return nil, fmt.Errorf("git backend: read %s: %w", p, err)
		}
		out[p] = data
	}
	return out, nil
}

// Validate delegates to the underlying writer's permission check.
func (g *GitBackend) Validate(_ context.Context) error {
	return g.writer.ValidatePermissions()
}

// Close releases the worktree directory.
func (g *GitBackend) Close() error {
	// GitWriter does not currently expose a Close method; the worktree lives
	// under /tmp and will be cleaned by the OS or pod-restart. No-op for now;
	// a follow-up could add gitWriter.Close().
	return nil
}

// ApplyBatch resets the worktree, then for each intent calls
// GitWriter.UpdateFiles (which produces one commit per intent + a push).
// One Reset per batch cleans any leftover state. Note: under the current
// GitWriter, each UpdateFiles call performs its own push, so an N-intent
// batch causes N pushes — still a meaningful reduction over the prior
// per-reconcile push storm. A future GitWriter "stage-without-push" mode
// would collapse this to one push per batch (see spec §5.1).
//
// Failure attribution:
//   - ErrPathOutsideRepo (bad workload path): per-intent quarantine; the
//     rest of the batch keeps applying because the path was rejected
//     before any git state was mutated for this intent.
//   - Any other UpdateFiles error: shared-state failure (the local repo
//     is now in an indeterminate state). Mark this intent AND all
//     remaining intents with ErrBatchFailed; controllers requeue from
//     CR state. Intents processed BEFORE the failure may already have
//     pushed; their PerIntent entries remain nil. This matches what
//     actually happened on the remote.
func (g *GitBackend) ApplyBatch(_ context.Context, batch []ResolvedIntent) BatchResult {
	res := BatchResult{PerIntent: make(map[string]error, len(batch))}

	if err := g.writer.Reset(); err != nil {
		wrapped := fmt.Errorf("%w: reset: %w", ErrBatchFailed, err)
		for _, ri := range batch {
			res.PerIntent[ri.Key] = wrapped
		}
		return res
	}

	var lastSHA string
	for i, ri := range batch {
		sha, err := g.writer.UpdateFiles(ri.SubDir, ri.WorkPlacement, ri.Writes.ToCreate, ri.Writes.ToDelete)
		if err != nil {
			if errors.Is(err, writers.ErrPathOutsideRepo) {
				res.PerIntent[ri.Key] = err
				continue
			}
			shared := fmt.Errorf("%w: %w", ErrBatchFailed, err)
			res.PerIntent[ri.Key] = shared
			for j := i + 1; j < len(batch); j++ {
				res.PerIntent[batch[j].Key] = shared
			}
			break
		}
		res.PerIntent[ri.Key] = nil
		if sha != "" {
			lastSHA = sha
		}
	}

	res.VersionID = lastSHA
	return res
}
