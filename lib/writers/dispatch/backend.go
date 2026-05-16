package dispatch

import "context"

//counterfeiter:generate . Backend

// Backend is implemented by GitBackend and S3Backend.
// Workers hold one per destination.
type Backend interface {
	// Read fetches the contents of the given paths from the destination's
	// current state. Missing paths return nil values (no entry in the map
	// for missing paths). Worker uses this to satisfy Intent.Reads before
	// calling Decide.
	Read(ctx context.Context, paths []string) (map[string][]byte, error)

	// ApplyBatch applies the given writes as atomically as the backend allows
	// and reports per-intent results.
	//
	// For git: either all PerIntent entries are nil (commit + push succeeded)
	// or all carry the same wrapped error (push failed).
	// For S3: PerIntent entries may differ per intent.
	ApplyBatch(ctx context.Context, batch []ResolvedIntent) BatchResult

	// Validate is the implementation behind Dispatcher.Validate.
	Validate(ctx context.Context) error

	// Close releases backend-held resources (close git worktree, etc).
	Close() error
}

// ResolvedIntent is an Intent after Decide has run.
// The worker constructs these and passes them to ApplyBatch.
type ResolvedIntent struct {
	// Key is the dedup key (stringified WorkPlacement+SubDir), opaque to backend.
	Key string

	// WorkPlacement is the source WP name, used for git commit messages.
	WorkPlacement string

	// SubDir scopes the write within the destination, as accepted by
	// GitWriter.UpdateFiles / S3Writer.UpdateFiles.
	SubDir string

	// Writes is what Decide returned.
	Writes Writes
}

// BatchResult is what Backend.ApplyBatch returns.
type BatchResult struct {
	// VersionID for the batch (git HEAD SHA, S3 composite). Empty on shared
	// failure.
	VersionID string

	// PerIntent maps the ResolvedIntent.Key to its outcome. nil = success.
	PerIntent map[string]error
}
