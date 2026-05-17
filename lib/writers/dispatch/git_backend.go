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

// ApplyBatch is implemented in Task 22.
func (g *GitBackend) ApplyBatch(_ context.Context, batch []ResolvedIntent) BatchResult {
	return BatchResult{}
}
