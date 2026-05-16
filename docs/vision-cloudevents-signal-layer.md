# Vision: Kratix + CloudEvents as a Platform Signal Layer

> **Kratix promises what the platform will provide.
> CloudEvents promises what the platform will say.**

Status: exploration / RFC. Not a commitment to build.
Author: Phill Morton — 2026-05-15.

---

## 1. Premise

Kratix reconciles continuously. On every loop, each controller computes — implicitly or explicitly — a delta between desired state (the Promise / Resource Request / Work) and actual state (the cluster, the destination, the Job). Today that delta surfaces in three places:

- **Status conditions** on the K8s object (`Available`, `WriteSucceeded`, `ConfigureWorkflowCompleted`, …)
- **Kubernetes `Event` objects** scoped to the owning resource (`PipelineStarted`, `WaitingDestination`, `WorkplacementsFailing`, …)
- **Prometheus metrics** (`kratix_workplacement_writes_total`, `kratix_workplacement_outcomes_total`, …)

All three are *useful*, but none are a stream. Conditions are level-triggered point-in-time state. Events are namespaced and expire in an hour by default. Metrics are aggregates. Nothing downstream of Kratix can subscribe to "tell me when a Promise transitions from Available to Unavailable across the fleet" without scraping or watching individual K8s objects.

**The proposal**: turn the *transitions* Kratix already detects into a structured event stream using the CloudEvents v1.0 envelope, emitted at controller boundaries via a thin abstraction we already have (`StateObserver`, see §5). Treat the stream as the substrate for bounded, single-remit agents that subscribe, reason, and — under human-in-the-loop governance — act.

This is not a new control plane. It is a side-channel.

---

## 2. Where reconciliation state surfaces today

| Surface | Where | What it carries |
|---|---|---|
| Reconcile entry | `internal/controller/promise_controller.go:154`, `work_controller.go:69`, `dynamic_resource_request_controller.go:105`, `workplacement_controller.go:91` | Generation, namespaced name, baseline log context |
| Delta computation | Generation comparison; `originalStatus`/`originalAvailableCondition` snapshot before reconcile, write-back after | Implicit — there is no central diff; each controller decides |
| Status conditions | `api/v1alpha1/workplacement_types.go:97-98`, `api/v1alpha1/promise_types.go:45`, `lib/resourceutil/util.go:20-38` | `Available`, `WriteSucceeded`, `ScheduleSucceeded`, `ConfigureWorkflowCompleted`, `DeleteWorkflowCompleted`, `WorksSucceeded`, `Reconciled`, `Ready`, `PausedReconciliation`, `WorkflowSuspended` |
| K8s Events | `promise_controller.go:236`, `work_controller.go:149-199`, `lib/workflow/reconciler.go:156,190` | `Unavailable`, `WaitingDestination`, `WorkplacementsFailing`, `PipelineSuspended`, `PipelineStarted` |
| Metrics | `internal/telemetry/metrics.go` | `kratix_workplacement_writes_total{result}`, `kratix_workplacement_outcomes_total{outcome}` keyed by `{promise, resource, destination, pipeline}` |
| **State-transition callback** | `internal/circuit/breaker.go` (worktree, commit `43fe050c`) | `StateObserver.OnTransition(key, old, new)` — *the one piece of infrastructure that already does exactly what we want* |

The condition vocabulary is already rich enough to bootstrap an event taxonomy without inventing new state.

---

## 3. Natural emission points

The reconcile loops already know when interesting things happen — we just need to fan them out. Concretely:

| Controller | Transition | Proposed event type |
|---|---|---|
| Promise | `Available: True → False` (line 211, 236) | `kratix.promise.unavailable` |
| Promise | `Available: False → True` | `kratix.promise.available` |
| Promise | First `Ready` reached after install | `kratix.promise.ready` |
| Work | Any `WorkPlacement` transitions `ScheduleSucceeded: True → False` | `kratix.work.unschedulable` |
| Work | `Ready` flips after retry storm | `kratix.work.recovered` |
| WorkPlacement | `WriteSucceeded: False` on a destination | `kratix.workplacement.write_failed` |
| Pipeline | `PipelineStarted` (reconciler.go:190) | `kratix.pipeline.started` |
| Pipeline | Job `Succeeded > 0` (reconciler.go:169) | `kratix.pipeline.completed` |
| Pipeline | Job `Failed > 0` (reconciler.go:173) | `kratix.pipeline.failed` |
| Pipeline | Manual reconcile detected | `kratix.pipeline.manual_intervention` |
| Circuit breaker | `closed → open` (worktree) | `kratix.controller.circuit_open` |
| Circuit breaker | `half-open → closed` | `kratix.controller.circuit_closed` |

These are not new state — they are existing transitions, lifted into a uniform shape.

---

## 4. Proposed CloudEvents schema

Use CloudEvents v1.0. JSON format on the wire. Required fields plus a small Kratix extension set:

```jsonc
{
  // CloudEvents v1.0 core
  "specversion": "1.0",
  "id":          "01HZ8X...ULID",
  "source":      "/kratix/<install-id>/<controller>",   // e.g. /kratix/prod-eu/promise-controller
  "type":        "kratix.promise.unavailable",          // see §3
  "subject":     "promise/<name>",                       // canonical K8s ref
  "time":        "2026-05-15T19:22:14.501Z",
  "datacontenttype": "application/json",

  // Kratix extensions (lower-case per CE spec)
  "kratixinstallid":  "prod-eu",
  "kratixgeneration": 42,                                // .metadata.generation at emission
  "kratixreason":     "WorksSucceeded=False",            // canonical condition reason
  "kratixseverity":   "warning",                         // info | warning | critical
  "kratixcorrelationid": "01HZ8W...",                    // ties events to a reconcile loop

  // Body
  "data": {
    "namespace":  "default",
    "name":       "redis",
    "kind":       "Promise",
    "apiversion": "platform.kratix.io/v1alpha1",
    "previous":   { "conditions": [ /* prior relevant subset */ ] },
    "current":    { "conditions": [ /* current relevant subset */ ] },
    "context": {
      // Optional: failing workplacements, destination, pipeline name, breaker key, etc.
    }
  }
}
```

Design notes:
- **`source`** is per-controller, per-install — lets a subscriber filter by control plane instance.
- **`subject`** is the canonical K8s reference. Agents can resolve back to the live object.
- **`type`** is reverse-DNS, dot-segmented. Reserve `kratix.*` as a namespace.
- **`kratixseverity`** is the only opinionated extension — it gates which agents wake up.
- **`previous` / `current`** carry only the *relevant subset* of conditions, not the whole object. Keeps payloads bounded; agents that need the full object resolve via `subject`.
- **`kratixcorrelationid`** is a per-Reconcile ULID. Multiple events from one loop share it. This is what makes the stream debuggable.

---

## 5. The primitive is already there: `StateObserver`

This is the most important finding. The per-Promise circuit breaker work (worktree branch `per-promise-fairness-phase-1`, commit `43fe050c`) introduced exactly the shape we need:

```go
// internal/circuit/breaker.go
type StateObserver interface {
    OnTransition(key types.NamespacedName, old, new State)
}

type ObservableBreaker interface {
    Breaker
    WithObserver(o StateObserver) ObservableBreaker
}
```

The breaker emits transitions at all four state-machine sites. The Promise controller wires an observer that translates each transition into a metric write, a status condition, and a K8s Event (`b097b224`, later refined in `3d8a871e` to keep API writes out of the observer goroutine).

**This is the emission model.** The circuit breaker is one *instance* of a pattern that generalises:

> *A controller surface that knows about transitions accepts an observer; emission of conditions, events, metrics, and (now) CloudEvents is composed at the observer layer, not hard-coded into the state machine.*

Concretely, a `CloudEventObserver` would:
1. Receive `OnTransition` (or a more general `OnEvent(domainEvent)`) from controllers.
2. Map to the CE envelope in §4.
3. Hand off to a non-blocking sink (channel → goroutine → HTTP/Kafka/NATS).
4. Drop with a counter on backpressure — never block reconcile.

Same pattern, three more controllers. No new abstraction.

---

## 6. Bounded agents — the consumer side

The producer/consumer split mirrors Promises themselves: Kratix promises the *shape*, agents are accountable for the *outcome*.

Each agent has a single remit, a defined event subscription, and an escalation path with a human gate before any non-reversible action.

### Sketch: three starter agents

**A. `catastrophic-degradation-agent`**
- *Subscribes to:* `kratix.promise.unavailable` AND `kratix.controller.circuit_open` where `kratixseverity=critical`, with a temporal join (≥ N events within M minutes across distinct subjects).
- *Not its concern:* single-instance flakes, individual pipeline failures, transient unschedulable Works. Those belong to (B) and (C).
- *Action ladder:*
  1. Open an incident with the correlated event bundle attached.
  2. Post to the platform on-call channel with a one-line synthesis.
  3. **Human gate** before any remediation (e.g. pausing reconciliation, scaling controller, draining a destination).

**B. `health-summary-agent`**
- *Subscribes to:* all `kratix.*` types, severity ≥ info, on a rolling window.
- *Action ladder:* publishes a periodic synthesis (e.g. hourly digest, weekly report). Read-only. No human gate needed because it takes no action.

**C. `pipeline-flake-agent`**
- *Subscribes to:* `kratix.pipeline.failed`.
- *Concern:* a single Resource Request whose pipeline failed once or twice. Decides retry policy, opens a ticket if persistent.
- *Not its concern:* fleet-wide patterns. Hands off to (A) via — crucially — *emitting its own CloudEvent* (`agent.pipeline_flake.escalating`) that (A) can subscribe to. Agents compose over the same substrate.

### Properties

- **Bounded responsibility.** Each agent's subscription filter *is* its contract.
- **Composable.** Agents may emit their own CloudEvents into the stream. The producer/consumer model is recursive.
- **Human-in-the-loop is structural, not bolt-on.** Any agent capable of mutating state has an explicit gate primitive (e.g. an `agent.action.proposed` CE that requires an `agent.action.approved` CE in response before execution). The gate is itself a subscriber + emitter, so it can be tested and audited like any other agent.
- **No special-cased logic in Kratix core.** Agents read events; they do not get controller-level hooks.

---

## 7. Architectural sketch

```
┌────────────────────────────────────────────────────────────┐
│  Kratix controllers (existing)                             │
│  ┌──────────┐ ┌──────────┐ ┌──────────────┐ ┌───────────┐  │
│  │ Promise  │ │ Work     │ │ WorkPlacement│ │ Pipeline  │  │
│  └────┬─────┘ └────┬─────┘ └──────┬───────┘ └─────┬─────┘  │
│       │            │              │                │       │
│       ▼            ▼              ▼                ▼       │
│            ┌─────────────────────────────┐                 │
│            │ StateObserver fan-out       │  (existing      │
│            │  - metric writer            │   pattern,      │
│            │  - condition writer         │   §5)           │
│            │  - event recorder           │                 │
│            │  + CloudEventObserver  ◄── new                │
│            └─────────────┬───────────────┘                 │
└──────────────────────────┼─────────────────────────────────┘
                           │  non-blocking sink
                           ▼
                ┌────────────────────┐
                │ Transport          │   pluggable:
                │ (HTTP / NATS /     │   start with K8s
                │  Kafka / K8s Event │   Events mirror +
                │  forwarder)        │   webhook
                └─────────┬──────────┘
                          │
        ┌─────────────────┼──────────────────┐
        ▼                 ▼                  ▼
  ┌───────────┐    ┌─────────────┐   ┌─────────────────┐
  │ catastr.  │    │ health      │   │ pipeline-flake  │
  │ degrad.   │    │ summary     │   │                 │
  │ agent     │    │ agent       │   │ agent           │
  └─────┬─────┘    └─────────────┘   └────────┬────────┘
        │  proposes action                    │  may re-emit
        ▼                                     ▼
  ┌──────────────────────────────────────────────────────┐
  │ Human-in-the-loop gate (CE-driven approval)          │
  └──────────────────────────────────────────────────────┘
```

The thing to notice: the bottom half (agents + gate) is *entirely* downstream of Kratix. The Kratix change is one new observer implementation and a transport adapter. Everything else lives outside the operator.

---

## 8. Open questions

1. **Transport choice.** Mirroring to K8s Events with a forwarder is the lowest-friction path (no new deps). NATS or a Kafka topic is the right answer at scale but a bigger commitment. Worth a separate decision.
2. **Schema registry.** If `kratix.*` types proliferate, agents will drift. Do we publish the type list as a generated artefact alongside the CRDs? Probably yes — same way conditions are documented.
3. **Backpressure semantics.** When the sink is slow, do we drop, buffer, or block? Default must be *drop with a counter*. Reconcile must never stall on emission. The 3d8a871e refactor already establishes this discipline for the breaker.
4. **Replay & idempotency.** Agents should be designed to tolerate duplicate events (use `id` + `kratixcorrelationid`). Are we ever responsible for exactly-once? My instinct: no. Document at-least-once and move on.
5. **Multi-tenancy / scoping.** `source` carries the install ID; do we also need a tenant extension for shared Kratix installs? Possibly an extension `kratixtenant` — but only if a concrete need shows up.
6. **What's the smallest believable v0?** I'd argue: one observer, one transport (K8s Events with a sidecar that converts to CE-over-HTTP), one agent (health-summary). Ship that. Let the second agent inform whether the schema needs revising.
7. **Relationship to `WatchCircuitOpen` and friends.** The condition vocabulary should remain the K8s-native source of truth. CloudEvents are *derived* — never the canonical state. If a condition and a CE disagree, the condition wins. Worth stating explicitly so agents don't drift into being a control loop.

---

## 9. What I'm explicitly *not* proposing

- A new CRD. The existing condition vocabulary is the contract.
- A Kratix-internal agent runtime. Agents live outside Kratix and are accountable for themselves.
- Replacing Events or conditions. CE is additive — the K8s-native surfaces stay.
- A control plane via events. Agents propose; humans approve; Kratix executes via its existing reconcile path. Events do not mutate state directly.

---

## 10. Next step (if pursued)

Build a 200-line spike: a `CloudEventObserver` that wraps the existing breaker `StateObserver`, emits CE JSON to stdout, and a tiny `health-summary-agent` that consumes it. End-to-end in one afternoon. The point of the spike is to falsify the schema in §4, not to build infrastructure.
