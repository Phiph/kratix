# Per-destination write queue (Dispatcher) — design

**Status:** Draft for review
**Date:** 2026-05-16
**Owner:** Phill Morton
**Related:** kratix#688 (test refactor — predecessor)

## Problem

Today, a `WorkPlacement` reconcile that targets a `GitStateStore` writes through `GitWriter` against a long-lived, per-state-store cached worktree. Concurrency is controlled by a single `sync.Mutex` per `Repository` (see `internal/controller/repository_cache.go`). When many WorkPlacements target the same destination — the canonical "2,000 resource requests writing to one destination" case — every reconcile serialises through:

1. The per-state-store mutex
2. `Reset` (checkout + clean)
3. Write workloads to the local worktree
4. Commit
5. Push to the remote

Each push is a separate network round trip and contends with every other push to the same branch. Throughput is capped at roughly one push per round-trip latency.

A secondary effect is that `BucketStateStore` writes are also serialised through the same per-`Repository` mutex even though S3 does not need this — different objects can be written concurrently with no contention against the bucket.

## Goal

**Reduce the number of git pushes under fan-in.** Turn N concurrent reconciles to the same destination into one push that covers all of them. Everything else (S3 efficiency, cleaner writer architecture, finer-grained locking) is in scope only as far as it serves this goal.

## Non-goals

- Rewriting `lib/writers/git.go` or `util/git/*`. Existing writers remain as building blocks.
- Cross-pod coordination. The dispatcher is in-memory; another pod will reconcile if this one dies.
- Persistent retry queues. Failures propagate to controllers; controller-runtime backoff handles requeueing.
- Bare-repo + per-operation worktree caching (Argo CD style). Not needed at our scale; one cached worktree per destination is sufficient because the worker serialises writes against it.

## Architectural decisions (locked)

| Decision | Choice |
|---|---|
| Scope | Touches `lib/writers` and all controller call sites |
| Controller-facing API | New `Dispatcher` interface; replaces `repositoryCache` |
| Backend coverage | Both git and S3, behind one interface, with backend-specific batching |
| Read/write atomicity | Callback intents (`Intent{Reads, Decide(reads) Writes}`) executed in one critical section |
| Failure semantics | No queue-level retries; batch failures propagate to all callers; controller-runtime backoff handles the herd |
| Batching trigger | Hybrid: fire when `max(BatchWindow elapsed, BatchMaxSize reached)` |
| Dedup policy | Last-write-wins by `(WorkPlacement, SubDir)` — older intent gets `ErrSuperseded` |
| Git batching mode | N commits per batch, 1 push at the end |
| Path-traversal handling | Quarantine bad intents at the backend boundary; batch continues |
| Idle eviction of workers | Deferred (see Future Enhancements) |

## Architecture

```
                       ┌────────────────────────────┐
   Controllers ───────▶│        Dispatcher          │
   (workplacement,     │  (one per controller pod)  │
    destination,       │                            │
    state_store)       │  ┌──────────────────────┐  │
                       │  │ workers map          │  │
                       │  │   key: DestinationKey│  │
                       │  │   val: *worker       │  │◀──── lazy create on first Submit
                       │  └──────────────────────┘  │
                       └──────────────┬─────────────┘
                                      │ one channel
                                      ▼  per worker
                       ┌────────────────────────────┐
                       │       destination worker   │
                       │   goroutine:               │
                       │     - drain intents        │
                       │     - batch (time/size)    │
                       │     - dedup by (WP, Sub)   │
                       │     - execute Decide()     │
                       │     - call backend         │
                       └─────────────┬──────────────┘
                                     │
                                     ▼
                       ┌────────────────────────────┐
                       │     Backend (interface)    │
                       │   Read / ApplyBatch /      │
                       │   Validate / Close         │
                       └────────────┬───────────────┘
                                    │
                       ┌────────────┴────────────┐
                       ▼                         ▼
            ┌──────────────────┐      ┌──────────────────┐
            │  GitBackend      │      │  S3Backend       │
            │  wraps GitWriter │      │  wraps S3Writer  │
            └──────────────────┘      └──────────────────┘
```

### Ownership

- **One `Dispatcher`** per controller pod, constructed at startup, shared by all reconcilers. Replaces `repositoryCache`.
- **One worker goroutine** per `DestinationKey`, lazy-created on first `Submit`, owns its `Backend` instance for its lifetime. The full `DestinationKey` (state-store kind, name, branch, path) is what determines worker identity — two reconciles targeting different branches of the same repo, or different paths of the same bucket, get different workers and can batch independently.
- **Backends** wrap the existing `GitWriter` / `S3Writer`. No changes to those writers' public APIs.

### What goes away

- `internal/controller/repository_cache.go` — replaced by `Dispatcher` in `lib/writers/dispatch`.
- `Repository.Lock()/Unlock()` at controller call sites — workers serialise per destination by construction.
- The `StateStoreWriter` interface as a controller-facing API. It may remain as an internal contract the backends use to talk to the underlying writers, or be folded into the backend implementations.

## Components & interfaces

### Public API

```go
// Package: lib/writers/dispatch

// Dispatcher is the entry point for all state-store writes.
// One instance per controller pod, shared by all reconcilers.
type Dispatcher interface {
    // Submit enqueues an intent for the given destination and blocks until
    // the batch it joined has been applied (or failed).
    //
    // The Decide callback runs inside the worker after reads complete but
    // before writes are applied — atomicity is guaranteed within the batch.
    Submit(ctx context.Context, dest DestinationKey, intent Intent) (Result, error)

    // Validate checks credentials and basic write permissions against the
    // destination. Does NOT enqueue — runs immediately on the calling
    // goroutine against a throwaway backend instance.
    Validate(ctx context.Context, dest DestinationKey) error

    // Cleanup tears down the worker for a destination (e.g. on StateStore
    // deletion). Pending intents fail with ErrDestinationGone.
    Cleanup(dest DestinationKey) error

    // Shutdown gracefully drains all workers and stops.
    // Called on pod shutdown.
    Shutdown(ctx context.Context) error
}
```

### Identity

```go
type DestinationKey struct {
    StateStoreKind string // "GitStateStore" | "BucketStateStore"
    StateStoreName string
    Branch         string // empty for S3
    Path           string // destinationPath
}
```

### Intents and results

```go
type Intent struct {
    // (WorkPlacement, SubDir) form the dedup key within a worker.
    WorkPlacement string
    SubDir        string

    // Reads: paths the Decide callback needs to read before computing writes.
    // Resolved by the worker against live state at batch-execute time.
    Reads []string

    // Decide computes writes given the read results.
    // Runs on the worker goroutine inside the batch critical section.
    // If Decide returns an error, this intent is quarantined out of the
    // batch and its caller receives the error. Other intents continue.
    Decide func(reads map[string][]byte) (Writes, error)
}

type Writes struct {
    ToCreate []v1alpha1.Workload
    ToDelete []string
}

type Result struct {
    // VersionID is backend-defined: git commit SHA, S3 version composite, etc.
    // Empty if the batch made no changes.
    VersionID string
}
```

### Backend interface

```go
type Backend interface {
    Read(ctx context.Context, paths []string) (map[string][]byte, error)
    ApplyBatch(ctx context.Context, batch []ResolvedIntent) BatchResult
    Validate(ctx context.Context) error
    Close() error
}

type ResolvedIntent struct {
    Key    string // dedup key, opaque to backend
    Writes Writes
}

type BatchResult struct {
    VersionID string
    PerIntent map[string]error // nil = success
}
```

### Configuration

```go
type DispatcherConfig struct {
    BatchWindow   time.Duration // default 500ms
    BatchMaxSize  int           // default 100
    SubmitTimeout time.Duration // default 30s
    DecideTimeout time.Duration // default 5s

    NewGitBackend func(spec v1alpha1.GitStateStoreSpec, creds map[string][]byte) (Backend, error)
    NewS3Backend  func(spec v1alpha1.BucketStateStoreSpec, creds map[string][]byte) (Backend, error)

    // Clock is injected for deterministic testing.
    Clock clock.Clock
}
```

### Sentinel errors

```go
var (
    // Intent superseded by newer intent for same (WP, SubDir).
    // Transient; controller-runtime requeues.
    ErrSuperseded = errors.New("intent superseded by newer intent")

    // Destination deleted while intent was queued.
    // Terminal.
    ErrDestinationGone = errors.New("destination no longer exists")

    // Dispatcher shutting down. Transient — another pod picks up.
    ErrShuttingDown = errors.New("dispatcher shutting down")

    // The batch this intent joined failed at the backend layer.
    // Wraps the backend error. Transient by default.
    ErrBatchFailed = errors.New("batch apply failed")
)
```

Errors returned by user-supplied `Decide` callbacks pass through unwrapped so callers can match on their own error types.

## Data flow

### Submit (hot path)

```
Submit(ctx, dest, intent)
  1. Lookup or lazy-create worker for dest (workersMu).
  2. Send (intent, resultCh) to worker's inbound channel.
     resultCh is a 1-buffered channel created per Submit.
  3. Block on <-resultCh or <-ctx.Done().
     - resultCh: return Result, err.
     - ctx.Done(): tell worker to drop our resultCh, return ctx.Err().
```

### Worker loop

```
loop:
  1. Wait for first intent on inbound channel.
  2. Start batchTimer (BatchWindow).
  3. Insert intent into pending map keyed by (WP, SubDir):
     - If key exists: signal old resultCh with ErrSuperseded, replace.
  4. If len(pending) >= BatchMaxSize: fire immediately.
  5. Otherwise, drain channel non-blockingly until timer fires or size reached.
  6. Fire batch (see below).
  7. Reset timer, return to step 1.
```

### Batch fire

```
1. Snapshot pending map → []Intent. Clear pending.
2. Collect unique Reads across all intents.
3. backend.Read(ctx, allPaths).
   - On error: every intent's resultCh receives ErrBatchFailed-wrapped read err.
4. For each intent (stable order):
   - Call intent.Decide(readsForThisIntent) under DecideTimeout.
   - If Decide errors / times out: signal that intent's resultCh, quarantine.
   - Else: collect as ResolvedIntent.
5. If 0 ResolvedIntents remain: done.
6. backend.ApplyBatch(ctx, resolvedIntents) → BatchResult.
7. For each intent: signal resultCh with Result{VersionID} or PerIntent[key] error.
```

### Validate

Runs on the caller's goroutine against a throwaway backend constructed just for this call. Does not touch (or create) the destination's worker. This avoids two problems: (1) Validate would otherwise have to serialise behind any pending batch, adding unrelated latency to a credential check; (2) Validate constructing a worker as a side effect would create persistent state for destinations that may not even have writes yet. State-store controller uses this at reconcile time for credential/permission checks.

### Cleanup

```
1. workersMu.Lock; remove worker from map; Unlock.
2. Close worker's inbound channel.
3. Worker discards remaining pending intents (does NOT apply them);
   signals each resultCh with ErrDestinationGone.
4. Worker exits goroutine; backend.Close().
```

Cleanup is for destination deletion. Anything queued is moot — the destination is gone.

### Shutdown

```
1. Snapshot all workers under workersMu.
2. For each (parallel): send drain signal; worker fires one final batch
   covering any pending intents, then exits.
3. Respect ctx.Done(): on expiry, force-close inbound channels;
   any still-pending resultChs receive ErrShuttingDown.
4. backend.Close() for each.
```

Shutdown is best-effort flushing on a graceful pod stop. Anything that can't be flushed within `ctx` deadline propagates `ErrShuttingDown` to callers; another pod (or this pod after restart) will reconcile from CR state.

## Failure & lifecycle

### Error matrix

| Failure | Who sees it | Caller treats as |
|---|---|---|
| Decide returns error | Just this intent | Per-controller decision (typically terminal) |
| backend.Read fails | All intents in batch | Transient |
| ApplyBatch shared failure | All intents | Transient |
| ApplyBatch per-intent failure (S3 only) | Just affected intents | Transient |
| ctx.Done() on caller | Just that caller | ctx.Err() |
| Worker panic | All pending intents | ErrBatchFailed |
| ErrSuperseded | Older intent | Transient (newer wins) |
| ErrDestinationGone | All pending at Cleanup | Terminal |
| ErrShuttingDown | All pending at Shutdown | Transient |

### Backpressure

- **Worker inbound channel**: bounded buffer (default 1000). Full channel causes `Submit` to block until room or `SubmitTimeout` expires.
- **Pending batch**: hard cap at `BatchMaxSize`. When reached, batch fires immediately regardless of timer. Ceiling on per-destination memory.

### Panic safety

The worker goroutine wraps its main loop in `defer recover()`. On panic:
- All pending `resultCh` receive `ErrBatchFailed`.
- Worker removes itself from dispatcher map.
- Panic is logged.
- Next `Submit` for that destination lazy-creates a fresh worker.

### Submit cancellation

When caller's `ctx` cancels mid-wait:
- Caller returns `ctx.Err()`.
- Worker is signalled to drop that resultCh from its tracking (so it doesn't send to a dead channel).
- The intent itself remains in the batch — the work has been queued; abandoning it would leak partial state. Only the result delivery is cancelled.

### Slow Decide callbacks

- `Decide` runs under a per-call context with `DecideTimeout` (default 5s).
- On timeout, the intent is quarantined out of the batch with `ctx.Err()`.
- **Convention:** `Decide` should be CPU-only, no I/O. `Reads` is for I/O. This is documented but not enforced.

### Pod restarts

In-memory dispatcher. Pod restart kills queued intents. K8s reconciliation re-runs from CR state. No persistence added.

## Backend strategies

### GitBackend

Wraps existing `GitWriter`. On construction, calls `gitWriter.Init(branch)` once (clones the destination). Worktree lives for the worker's lifetime.

**Read:** per-path `gitWriter.ReadFile(path)`. Local filesystem read against cached worktree. Cheap.

**ApplyBatch (3 phases):**

```
1. Reset
   - gitWriter.Reset() — checkout --force + clean -ffdx.
   - One per batch; cleans any leftover state.

2. Stage and commit each intent
   - For each ResolvedIntent (stable order):
       gitWriter.update(intent.SubDir, intent.Key,
                        intent.Writes.ToCreate,
                        intent.Writes.ToDelete)
   - This produces one commit per intent (existing behaviour).
   - Path-traversal check inside update should report failure rather than
     silently no-op; quarantine such intents and continue with the rest.

3. Push
   - One git push at the end of the batch covering all commits.
   - On success: every intent receives the post-push HEAD SHA.
     (Per-commit SHAs are available; we use the final SHA as VersionID
     for the batch result. Consumers don't need per-intent SHAs today.)
   - On failure: every intent receives ErrBatchFailed-wrapped push err.
```

**Note on path traversal:** Current `GitWriter.update` silently returns `("", nil)` when a workload's path escapes the worktree (see `git.go:140-146`). This must be modified to surface the rejection so GitBackend can quarantine the offending intent. Alternative: GitBackend pre-validates paths before calling `update`. Either is acceptable; implementation decides.

**Close:** removes the temp directory; releases auth state.

### S3Backend

Wraps existing `S3Writer`. No cached state. Construction sets up the minio client.

**Read:** per-path `s3Writer.ReadFile(path)`. Optionally parallel via errgroup if Reads list is large.

**ApplyBatch:**

```
1. Compute combined delete set
   - For each unique SubDir in batch: one ListObjects to enumerate.
   - For intents with explicit ToDelete: include those paths.
   - Subtract any paths the batch is about to PUT — never delete-then-recreate.

2. Issue writes in parallel
   - errgroup with concurrency cap (default 16).
   - PutObject per Workload, with ETag short-circuit to skip unchanged content.
   - Per-intent errors recorded independently.

3. Issue deletes in batches
   - Group into chunks of up to 1000 (S3 DeleteObjects limit).
   - Failures attributed to the intent that owned the path. If multiple
     intents in the batch listed the same path for deletion, the failure
     is attributed to all of them (rare; same-path collisions usually
     indicate a misconfiguration upstream).

4. Aggregate
   - VersionID: composite hash of all upload version IDs (matches existing
     S3Writer.update behaviour).
   - PerIntent: dedup key → error or nil.
```

**Key contrast with git:** S3 batches *deletes* (1000x reduction in API calls) and *parallelises writes*. There is no "one atomic apply" — each intent's outcome is independent.

**BucketExists** is called at most once per batch (not per intent). Removes N-1 wasteful HEAD requests.

### Why the implementations are intentionally different shapes

Git's bottleneck is the remote push. S3's bottleneck is API call count. Same interface (`Backend`), different batching strategies. Worker and dispatcher code is identical regardless of backend.

## Testing strategy

### Dispatcher + worker — fake backend

Counterfeiter-generated `FakeBackend`. Inject a fake clock (`k8s.io/utils/clock`) for deterministic batching tests.

Properties pinned by tests:
- Submit blocks until batch fires; returns when resultCh receives.
- Batch fires on time window expiry (verify with fake clock).
- Batch fires on size threshold.
- Dedup: older intent → `ErrSuperseded`, newer → backend.
- 100 concurrent submits → expected number of `ApplyBatch` calls; no lost intents.
- Decide error quarantines just that intent.
- Read failure fails the whole batch.
- ApplyBatch shared failure → all callers get `ErrBatchFailed`.
- ApplyBatch per-intent failure → individual attribution.
- Submit ctx cancellation does not deadlock the worker.
- Cleanup → pending receive `ErrDestinationGone`.
- Shutdown drains; pending get `ErrShuttingDown` on ctx expiry.
- Panic in Decide recovers; next Submit succeeds.

### GitBackend — local bare-repo integration tests

`t.TempDir()` + `git init --bare`. No network. Properties:

- 1-intent batch → 1 commit, 1 push.
- N-intent batch → N commits, 1 push.
- Per-intent commit messages reflect each WP.
- Path-traversal intent quarantined; other intents succeed.
- Push rejection (pre-populate bare repo with a divergent commit) → all intents get `ErrBatchFailed`.
- ReadFile against existing and missing paths.

The existing `test/git/` against real GitHub remains as auth-integration coverage but does not duplicate the behavioural tests here.

### S3Backend — MinIO integration tests

Reuse the existing MinIO test harness if present. Properties:

- 1-intent batch → expected objects, expected VersionID.
- N-intent batch → all writes happen; per-intent errors attributed correctly.
- Mixed-success batch: bad creds for one intent → only that one fails.
- Batched deletes use `DeleteObjects` (verify behaviourally).
- SubDir replacement: existing objects under SubDir deleted, new ones appear.

### Controller integration

3-5 end-to-end tests using the existing controller test harness:

- WorkPlacement reconcile against a GitStateStore produces the right files via the dispatcher.
- 100 concurrent WP reconciles to one destination produce ≤10 pushes (proves fan-in in a controller context).
- StateStore deletion triggers `Dispatcher.Cleanup`; worker exits cleanly.

### Out of scope for this test work

- `GitWriter` / `S3Writer` internals (covered by issue #688).
- `util/git/*` primitives.
- Specific timing values; those are configuration, varied per test.
- Live GitHub / live S3.

### File layout

```
lib/writers/dispatch/
    dispatcher.go              # public API
    dispatcher_test.go
    worker.go                  # internal: per-destination worker
    worker_test.go             # batching, dedup, quarantine
    backend.go                 # Backend interface + sentinel errors
    git_backend.go
    git_backend_test.go
    s3_backend.go
    s3_backend_test.go
    dispatchfakes/             # counterfeiter-generated mocks
        fake_backend.go
```

## Migration

The dispatcher is additive code. Migration from `repositoryCache` happens in two phases:

1. **Land the dispatcher and backends with no controller wiring.** Pure new code in `lib/writers/dispatch`. Fully tested in isolation. Reviewable on its own.

2. **Migrate controllers to use the dispatcher.** Remove `repositoryCache`, `Repository.Lock/Unlock`, the `StateStoreWriter` interface as a controller-facing type. Each controller (`workplacement_controller`, `destination_controller`, `state_store`) restructures `Reset → ReadFile → compute → UpdateFiles` flows into `Dispatcher.Submit(Intent{Reads, Decide, ...})` calls.

This second phase is where the bulk of the controller-side diff lives. It is deliberately separated so reviewers can evaluate dispatcher correctness independently of controller restructuring.

## Future enhancements (out of scope for this design)

- **Idle eviction of workers.** Auto-terminate workers idle for `IdleTimeout`. Releases backend resources (cached worktree, auth state). Adds a self-eviction timer to the worker loop. Localised change; can be added later if memory/disk telemetry shows the need.
- **Per-intent commit SHA attribution for git.** Currently the batch returns one final HEAD SHA. If a consumer ever needs to know which commit covered which intent, GitBackend would track per-commit SHAs and populate them into `BatchResult` per-intent.
- **Cross-pod coordination.** Multiple controller pods writing to the same destination is currently handled by git's non-fast-forward push rejection. A more sophisticated approach (leader election per destination, shared queue) is conceivable but unnecessary at our scale.
- **Adaptive batch sizing.** Tune `BatchWindow` / `BatchMaxSize` based on observed traffic. Adds telemetry and a control loop. Defer until we have data.

## Open questions

None outstanding at this point — all questions raised during brainstorming have been resolved.

## Observability

Metric emission points (concrete metric names deferred to implementation):
- Per-destination: queue depth, batch size on fire, batch latency, push/apply duration, per-result-code counts.
- Per-dispatcher: worker count, total inbound throughput.

## References

- Existing writer: `lib/writers/git.go`, `lib/writers/s3.go`
- Existing cache: `internal/controller/repository_cache.go`
- Existing controllers using the cache:
  - `internal/controller/workplacement_controller.go`
  - `internal/controller/destination_controller.go`
  - `internal/controller/state_store.go`
- Underlying git primitives: `util/git/*` (forked from Argo CD's `util/git`)
- Predecessor work on testability: kratix#688
