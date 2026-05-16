# Kratix Eventing — Wire Format (v0.1, MVP)

> **Status:** alpha. The format will change before v1alpha1. Pin everything.
>
> **Scope:** the contract between Kratix (producer of K8s `Event` objects with `kratix.io/ce-*` annotations) and the forwarder (consumer that translates them into CloudEvents). Agents downstream of the forwarder consume the resulting CloudEvent, not the K8s Event.

---

## 1. Canonical record

Kratix emits a standard Kubernetes `Event` object. The Event *is* the canonical record — CloudEvents are a derived representation. If the Event and the CE disagree, the Event wins.

Producers (Kratix controllers) interact only with the K8s `EventRecorder` API. They never construct CloudEvent envelopes directly.

## 2. Reason naming

Producers populate `Event.reason` using **PascalCase, kratix-namespaced** identifiers. The forwarder strips the implicit `Kratix` prefix where present and lower-cases segments to derive the CloudEvent `type`.

| Event.reason            | CloudEvent type                |
| ----------------------- | ------------------------------ |
| `PromiseUnavailable`    | `kratix.promise.unavailable`   |
| `PromiseAvailable`      | `kratix.promise.available`     |
| `PromiseReady`          | `kratix.promise.ready`         |
| *(future entries TBD)*  |                                |

Rules:

1. PascalCase. No underscores, no hyphens.
2. First segment names the subject domain (`Promise`, `Work`, `Pipeline`, …).
3. Last segment names the transition verb in past or adjective form (`Unavailable`, `Started`, `Failed`).
4. Reasons not matching the producer convention are **ignored** by the forwarder. Non-Kratix Events on the cluster are unaffected.

## 3. Required annotations (v0.1)

**The annotation set is the producer signal.** Kubernetes itself emits many PascalCase reasons (`FailedScheduling`, `BackOff`, `Pulling`, …); reason naming alone cannot distinguish Kratix Events from upstream traffic. The forwarder treats an Event as Kratix-origin only if *both* required annotations are present.

Producers MUST set these `metadata.annotations` on the Event object. The forwarder rejects (with a log line and a dropped-counter increment) any Event missing a required annotation.

| Annotation key                        | Type   | Meaning                                                                  |
| ------------------------------------- | ------ | ------------------------------------------------------------------------ |
| `kratix.io/ce-correlation-id`         | ULID   | Per-Reconcile correlation. All Events emitted from one Reconcile share it. |
| `kratix.io/ce-generation`             | int    | `metadata.generation` of the involved object at emission time.           |

That is the entire required set for v0.1. Optional annotations may be added; consumers MUST ignore unknown `kratix.io/ce-*` annotations.

## 4. Severity mapping

The forwarder derives `kratixseverity` from `Event.type`:

| Event.type | kratixseverity |
| ---------- | -------------- |
| `Normal`   | `info`         |
| `Warning`  | `warning`      |

There is no `critical` severity in v0.1. When a need for it surfaces, we will introduce it as an explicit annotation override (`kratix.io/ce-severity`) rather than overloading `Event.type`.

## 5. Subject derivation

The forwarder derives the CloudEvent `subject` from `involvedObject`:

```
subject = "<kind>/<name>"        # if namespace empty
subject = "<namespace>/<kind>/<name>"   # otherwise
```

Kind is lower-cased. Example: `default/promise/redis`.

## 6. Correlation ID semantics

- One ULID per Reconcile call.
- Generated at the top of `Reconcile` and passed via `context.Context` (key TBD; pragmatic for v0.1: a struct field on the per-reconcile logger context).
- Every Event emitted during that Reconcile carries the same ID.
- Consumers MAY use `(kratixcorrelationid, subject)` as a dedupe key.
- The ID is **not** propagated across Reconcile calls. Two Reconciles of the same object produce two different correlation IDs.

## 7. The CloudEvent envelope (forwarder output)

This is what the forwarder emits over HTTP. Structured mode, JSON, `Content-Type: application/cloudevents+json`.

```jsonc
{
  "specversion": "1.0",
  "id":          "01HZ8X...ULID",                       // forwarder-generated, unique per CE
  "source":      "/kratix/<install-id>/event-forwarder", // forwarder identity
  "type":        "kratix.promise.unavailable",          // derived per §2
  "subject":     "default/promise/redis",               // derived per §5
  "time":        "2026-05-15T19:22:14.501Z",            // Event.eventTime or Event.lastTimestamp
  "datacontenttype": "application/json",

  "kratixinstallid":     "prod-eu",                     // from forwarder config
  "kratixcorrelationid": "01HZ8W...",                   // from annotation
  "kratixgeneration":    42,                            // from annotation, parsed to int
  "kratixseverity":      "warning",                     // derived per §4

  "data": {
    "namespace":  "default",
    "name":       "redis",
    "kind":       "Promise",
    "apiversion": "platform.kratix.io/v1alpha1",
    "reason":     "PromiseUnavailable",
    "message":    "Promise is unavailable: WorksSucceeded=False"
  }
}
```

Notes on the v0.1 envelope:

- **No `previous`/`current` condition diff in the `data` block yet.** The vision doc proposed it; we are deliberately omitting it from v0.1. K8s Event annotations have a 256KB practical ceiling per object but typical individual values should stay small. Once we have a real consumer asking for the diff, we add it as an optional annotation (`kratix.io/ce-data` carrying a JSON blob) and parse it through.
- **`id` is forwarder-generated, not from the Event UID.** This keeps re-emission semantics clear: if the forwarder restarts and re-processes an Event, the CE gets a new `id` but the same `kratixcorrelationid`. Consumers dedupe on the latter.

## 8. Worked example — `kratix.promise.unavailable`

Producer side (Kratix controller, hypothetical wiring at `internal/controller/promise_controller.go:236`):

```go
corrID := ulid.MustNew(ulid.Now(), nil).String()
r.EventRecorder.AnnotatedEventf(
    promise,
    map[string]string{
        "kratix.io/ce-correlation-id": corrID,
        "kratix.io/ce-generation":     strconv.FormatInt(promise.Generation, 10),
    },
    "Warning",
    "PromiseUnavailable",
    "Promise is unavailable: %s", reason,
)
```

> `AnnotatedEventf` is a real method on `record.EventRecorder`. Reuses the existing emission path; no new client.

Forwarder side:

1. Informer fires on the new Event.
2. Reason has no `kratix.` prefix and is PascalCase → derive type `kratix.promise.unavailable`.
3. Read required annotations; reject + count if missing.
4. Build CE envelope (§7).
5. Generate fresh CE `id` (ULID).
6. POST to all matching `CloudEventSink`s (filter on `typeFilter`).
7. On non-2xx: log + increment a per-sink failure counter. Do **not** retry in v0.1.

## 9. Versioning

- This document is **v0.1**.
- Breaking changes (rename annotation keys, change severity mapping, change ID semantics) bump to v0.2. We do not promise compatibility before v1alpha1.
- The first stable contract version will be tagged `v1alpha1` once a non-trivial agent is consuming the stream in production.

## 11. User-pipeline emission (Pattern 1 only)

Pipeline containers — the user-authored ones that run *inside* a Kratix workflow — may emit CloudEvents about their own work. These are **telemetry signals**, not workflow triggers. Per §6 of the vision doc, agents are responsible for causality; the producer just publishes.

### Mechanism

Producers call the `kratix-emit` CLI (`eventing/cmd/kratix-emit`), shipped as a separate image. The CLI:

1. Reads the parent identity from `KRATIX_OBJECT_*` env vars Kratix already injects on every pipeline container (see `api/v1alpha1/pipeline_factory.go:defaultEnvVars`). No new env vars.
2. Constructs a `corev1.Event` whose `involvedObject` is the parent Promise/RR.
3. Annotates with `kratix.io/ce-correlation-id` (auto-generated unless `--correlation-id` is passed), `kratix.io/ce-generation=0` (the sentinel for user-pipeline emission), and **`kratix.io/ce-type=<full type>`** (authoritative — see below).
4. POSTs the Event via the pipeline pod's existing ServiceAccount. Emission is best-effort: a failed API call logs to stderr and exits 0, so telemetry can never break a pipeline.

### Type namespace

User-pipeline CloudEvent types **must not** use the reserved `kratix.*` prefix. Pipelines emit under their own type namespace, scoped under the promise that owns them. Recommended shape:

```
pipeline.<promise-name>.<verb>
e.g. pipeline.redis.upstream.fetch.failed
     pipeline.cloudflare.api.throttled
```

`kratix.*` is reserved for events emitted by Kratix controllers themselves. The CLI rejects `--type` values beginning with `kratix.`.

### Type annotation (authoritative for user-emitted events)

Because reason naming (§2) implicitly resolves to `kratix.*`, user-emitted events MUST set `kratix.io/ce-type` to the full type string (`pipeline.<promise>.<verb>`). The forwarder reads this annotation first; if present, it is the CloudEvent `type` verbatim. Reason becomes a human-readable label only.

Kratix controllers MAY omit the annotation — their reason names are kratix-shaped by construction, and the derivation in §2 works correctly. `kratix-emit` sets it on every Event it creates.

### Generation sentinel

K8s `metadata.generation` is owned by the K8s API server and only meaningful for the parent object's own state. User pipelines do not own the parent's generation, so v0.1 sets `kratix.io/ce-generation=0` for all pipeline-emitted events. Forwarders MAY treat a generation of 0 as "user-emitted" for filtering purposes; future revisions may introduce a richer attribution mechanism (e.g. a separate `kratix.io/ce-producer=pipeline` annotation).

### Source attribution

For pipeline-emitted events, the forwarder MUST set `source` to:

```
/kratix/<install-id>/pipeline/<parent-namespace>/<parent-name>
```

This is derived at forwarder time from `involvedObject` plus the `kratix-emit` `ReportingController` field, so producers don't have to think about it.

## 12. RBAC trade-off (v0.1)

Pipeline pods need permission to create K8s `Event` objects for `kratix-emit` to work. The chosen approach inlines `events: create, patch` into the **existing** per-pipeline RBAC objects:

- **Resource workflows**: rule added to the existing namespaced `Role` (already grants `works`).
- **Promise workflows**: rule added to the existing per-pipeline `ClusterRole`.

This keeps the per-pipeline RBAC object count flat at fleet scale — zero new objects, zero new Apply calls per reconcile. At 10,000 pipelines this is the difference between ~5MB of etcd footprint and 20,000 extra writes per reconcile cycle.

**Trade-off**: for promise workflows the grant is implicitly cluster-wide (ClusterRoles do not scope by namespace). A compromised pipeline could create Events about objects outside its own namespace. The mitigation in v0.2 is forwarder-side provenance verification — checking that the emitting pod's owner chain matches the `involvedObject` it claims. Until then, audit logs are the line of defence.

This choice is documented here rather than buried in code because it is a security-relevant default. Operators who want stricter scoping can override the per-pipeline ClusterRole with a per-pipeline namespaced Role, accepting the per-reconcile write cost.

## 13. What's deliberately not in v0.1

- Optional `kratix.io/ce-data` payload annotation (condition diff, context blob).
- A `kratix.io/ce-severity` override.
- Retry / backpressure semantics beyond "drop and count."
- Replay from anywhere other than the live informer cache.
- Cross-cluster federation.
- Schemas for `Work`, `WorkPlacement`, `Pipeline`, circuit breaker transitions. (The naming convention covers them; the explicit type table will grow as producers wire emission.)
- **Pattern 2** (events as workflow triggers). v0.1 treats CloudEvents as telemetry only. Causality lives in agents, not in Kratix.
- Forwarder-side provenance verification for user-emitted events. The pipeline-pod owner chain is not checked against `involvedObject`; see §12.
- Richer attribution than the generation=0 sentinel for user-emitted events.
