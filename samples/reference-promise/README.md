# Reference Promise: ScheduledJob

> A complete worked example of a Kratix Promise that ships with everything a real platform producer would build: the Promise CRD, the configure pipeline, a signal taxonomy declaration, a producer-side event emitter, a read-only observability agent, and an action-taking agent with a full escalation gate loop.
>
> **What this is for.** When you are designing your own Promise and want to know "what does a finished one look like?", this is the answer. Copy it, replace `ScheduledJob` with your domain, and you have a starting skeleton.

---

## What this Promise contracts to its consumers

> "Submit a ScheduledJob. I'll give you a managed CronJob with retry policy, an audit trail of every spec applied, default observability via the CloudEvent bus, and a gated remediation agent that proposes pausing the schedule if it begins flaking."

That's the producer's *promise*. The rest of this README is how it's implemented.

---

## What a producer ships

Eight artefacts. Three are declarative; four are runtimes; one is the bundle manifest that ties them together.

```
samples/reference-promise/
├── promise.yaml                          # 1. the Promise CRD + workflow
├── pipeline/                             # 2. configure pipeline (runtime)
│   ├── main.go
│   ├── main_test.go
│   └── Dockerfile
├── signals.yaml                          # 3. PromiseSignals taxonomy (declarative)
├── bundle.yaml                           # 8. PromiseBundle — "what ships with my Promise"
├── job-watcher/                          # 4. signal *producer* (runtime)
│   ├── observer.go
│   ├── emitter.go
│   ├── main.go
│   └── Dockerfile
├── agents/
│   ├── health-summary/                   # 5. read-only agent (config only — reuses generic)
│   │   └── health-summary-agent.yaml
│   └── pause-flaking-job/                # 6. action-taking agent (runtime)
│       ├── window.go
│       ├── proposer.go
│       ├── executor.go
│       ├── main.go
│       └── Dockerfile
├── examples/
│   └── resource-request.yaml             # 7. a sample RR
└── README.md
```

The headline lesson: the read-only agent is *not a runtime*. It's a one-file configuration of the generic `health-summary-agent` we already ship. **Most Promises will not author their own agent runtime.** They'll ship configurations.

---

## Architecture at a glance

```
                          ┌─────────────────────────┐
       (operator applies) │ ScheduledJob Promise    │
                          └────────────┬────────────┘
                                       │ registers CRD + workflow
                                       ▼
       ┌─────────────────────────────────────────────────────────────┐
       │ Consumer applies an RR (samples/.../examples/...)            │
       └────────────┬─────────────────────────────────────────────────┘
                    │ Kratix runs the configure pipeline
                    ▼
                ┌─────────────────────┐
                │ pipeline/ (one-shot)│ → /kratix/output: CronJob + audit-config
                └─────────────────────┘
                    │
                    ▼ Kratix work-creator applies them
                ┌─────────────────────┐
                │ CronJob in cluster  │
                └──────────┬──────────┘
                           │ runs Jobs every tick
                           ▼
            ┌──────────────────────────┐
            │ job-watcher (long-run)   │ observes Jobs + CronJob suspend state
            └──────────────┬───────────┘
                           │ emits kratix.scheduledjob.* CEs as K8s Events
                           ▼
            ┌──────────────────────────┐
            │ kratix-event-forwarder   │ fan-out
            └────────┬────────┬────────┘
                     │        │
        ┌────────────┘        └────────────────┐
        ▼                                      ▼
┌────────────────────────────┐    ┌──────────────────────────┐
│ health-summary-agent       │    │ pause-flaking-job        │
│  (read-only digest)        │    │  (action-taking)         │
└────────────────────────────┘    └─────────┬────────────────┘
                                            │ 3+ failures in 30m
                                            ▼ emits .proposed CE
                                  ┌─────────────────────────┐
                                  │ escalation-gate         │
                                  │ materialises Proposal CR │
                                  └─────────┬───────────────┘
                                            │ kratix-approve <id>
                                            ▼ emits .approved CE
                                  ┌─────────────────────────┐
                                  │ pause-flaking-job       │
                                  │  patches spec.suspended │
                                  └─────────────────────────┘
                                            │ emits .executed CE
                                            ▼
                                       (audit trail)
```

---

## The seven artefacts, in order

### 1. `promise.yaml` — the public API

This is the *only* surface your consumers see. They submit ScheduledJob RRs against the CRD; they don't know or care about the pipeline, the job-watcher, or the agents.

The CRD's schema is your contract: which fields are required, which have defaults, which can be tuned. Keep it small. Consumers will use whatever you put here for years.

Why namespaced? Because each ScheduledJob is owned by a team and runs in their namespace.

Why `suspended` as a spec field? So the action-taking agent can set it via a standard `kubectl patch` rather than a side-channel. **Make remediation actions writable through the API surface, not via labels or annotations.** Agents become normal Kubernetes citizens that way.

### 2. `pipeline/` — the configure workflow

A single binary, one shot per reconcile. Reads `/kratix/input/object.yaml`, writes manifests to `/kratix/output/`. Kratix's work-creator handles the actual `kubectl apply`.

Key shape decisions:
- **No K8s client imports.** The pipeline only reads and writes YAML files. Kratix is the apply mechanism, not the pipeline.
- **Default-applying happens in `readInput`**, not silently inside `build`. Future operators reading the code can see exactly which fields are defaulted and which are required.
- **An audit ConfigMap accompanies every CronJob.** The agent's rationale ("this schedule has changed five times this week") needs a stable record of what the spec looked like at each reconcile.

### 3. `signals.yaml` — the signal taxonomy

A `PromiseSignals` CR that declares every CloudEvent type this Promise emits. Producer-side documentation expressed as a Kubernetes resource so it's versioned, validated, and queryable alongside the Promise.

Two families:
- **`kratix.scheduledjob.*`** — substrate events emitted by the job-watcher.
- **`agent.scheduledjob.pause.*`** — the gate vocabulary for the action-taking agent (reserved suffixes per `docs/escalation-contract.md`).

Operators who read your `signals.yaml` know exactly what to subscribe to without spelunking through agent source code. **Treat this like the API surface for your event stream.**

### 4. `job-watcher/` — the *producer* of `kratix.scheduledjob.*`

The forwarder fans events out, but *something has to put events on the bus first*. Kratix's own controllers emit events about Promises and Resource Requests — but they don't know about your domain. The job-watcher is *your* contribution to the producer side.

It's a long-running Deployment that watches Jobs (and CronJobs for the `skipped` transition) labelled `platform.kratix.io/owned-by=scheduled-job-promise`. It emits annotated K8s Events when a Job starts, completes, fails, or its parent CronJob flips to suspended.

Why this is the most underrated piece: **without it, the agents you ship have nothing to subscribe to.** A Promise without a producer is a half-built ecosystem.

The observer logic is pure (testable in isolation). The wiring is informer-based. The emitter writes annotated K8s Events that the forwarder consumes — no HTTP endpoint here.

### 5. `agents/health-summary/` — the read-only agent

**A single YAML file. No code, no Dockerfile, no image.**

The generic `HealthSummaryAgent` we ship at `eventing/agents/health-summary/` is fully sufficient as a runtime. The producer's contribution is just *pinning the subscription* to this Promise's signal namespace:

```yaml
spec:
  subscribe:
    - "kratix.scheduledjob.*"
    - "agent.scheduledjob.*"
```

That's the entire delta. Hourly digests now scope to ScheduledJob health.

This is what scales. A 50-Promise platform doesn't have 50 health-summary runtimes — it has 50 one-file configurations of one runtime.

### 6. `agents/pause-flaking-job/` — the action-taking agent

The one runtime you genuinely need to write per Promise: the agent that knows how to *do something* in your domain.

It subscribes to:
- `kratix.scheduledjob.job.failed` — to count failures per ScheduledJob
- `kratix.scheduledjob.job.completed` — to reset the count on a green run
- `agent.scheduledjob.pause.approved` — to know when humans have authorised it
- `agent.scheduledjob.pause.expired` — to clean up pending state

Decision logic:
- 3 failures within 30 minutes for the same ScheduledJob → emit `.proposed`.
- On `.approved`, patch the ScheduledJob's `spec.suspended=true` via the dynamic client.
- Emit `.executed` with outcome=succeeded|failed regardless.

The blast-radius framing matters. Pausing a flaking schedule is **reversible** (one `kubectl edit` away) and **visible** (a paused CronJob is obvious). That's the right shape for a v0.1 action. Anything more dangerous (deleting state, failing over a primary) needs more sophisticated gating — see `docs/escalation-gate-patterns.md` for quorum, separation-of-duty, time-of-day, and chained-expiry recipes.

### 8. `bundle.yaml` — "how my Promise tells the platform what it ships"

The piece that ties the other seven together.

A `PromiseBundle` is the producer's manifest of *companions* — the agents, configs, supporting Deployments, and signal taxonomies that ship alongside the Promise. Without it, an operator installing your Promise has to know to apply `signals.yaml`, the health-summary-agent CR, the job-watcher Deployment + RBAC, and any other artefacts you care about. With it, they apply two files: `promise.yaml` and `bundle.yaml`.

The bundle-controller (`eventing/cmd/bundle-controller`) watches `PromiseBundle` resources; when the referenced Promise reaches `Available`, it server-side-applies every companion with the bundle set as the owner reference. Deleting the bundle cascades to its companions via standard Kubernetes garbage collection — there's no separate uninstall ceremony.

Each companion is one of:

- **Inline** — the full resource manifest, embedded in the bundle. Best for small things like CRs (the `HealthSummaryAgent` config, the `PromiseSignals` declaration, ServiceAccounts).
- **Ref** — a reference to a `ConfigMap` carrying the YAML. Useful for larger manifests or when you want the manifest editable separately.

The example bundles three things: the `PromiseSignals`, the `HealthSummaryAgent` config, and the `job-watcher` Deployment + its RBAC. Notice what's *not* in the bundle: the action-taking `pause-flaking-job` agent. It's deliberately a separate install — operators opt in to having an agent that mutates their resources. **The bundle ships the substrate; remediation is an explicit decision.**

Producer payoff: one declarative manifest of everything that travels with your Promise, applied with one kubectl command, garbage-collected as one unit. Operators see a single source of truth for "what does installing this Promise actually mean?"

### 7. `examples/resource-request.yaml` — a sample RR

For consumers. A working ScheduledJob they can submit to convince themselves it does what the Promise promises.

The example uses `busybox` so it runs anywhere. To exercise the action-taking agent end-to-end, edit the args to include `exit 1` — the failures will trip the threshold in three minutes, the agent will propose, and you can run `kratix-approve` to authorise the pause.

---

## End-to-end smoke test

**Prerequisites:** a running Kratix install with the kratix-event-forwarder + escalation-gate already deployed (`eventing/cmd/escalation-gate`, `eventing/cmd/forwarder`). The generic `HealthSummaryAgent` Promise should also be installed.

```sh
# 1. Install the Promise.
kubectl apply -f samples/reference-promise/promise.yaml

# 2. Apply the bundle — this single command brings in:
#       - the PromiseSignals taxonomy
#       - the HealthSummaryAgent configuration (read-only observability)
#       - the job-watcher Deployment + ServiceAccount + RBAC
#    The bundle-controller waits for the Promise to be Available, then
#    server-side-applies every companion with the bundle as owner.
kubectl apply -f samples/reference-promise/bundle.yaml

# 3. Verify the bundle resolved.
kubectl get promisebundle scheduled-job -o jsonpath='{.status.conditions}'
kubectl get promisebundle scheduled-job -o jsonpath='{.status.companions}' | jq

# 4. Submit an RR. (Edit the namespace/args to taste.)
kubectl create namespace team-a
kubectl apply -f samples/reference-promise/examples/resource-request.yaml

# 5. Watch Kratix run the configure pipeline.
kubectl -n team-a get cronjobs
kubectl -n team-a get configmaps -l app.kubernetes.io/name=scheduled-job

# 6. For the optional action-taking agent (NOT in the bundle by design):
#    Build and deploy the pause-flaking-job runtime separately.
docker build -t reference-pause-flaking:dev -f samples/reference-promise/agents/pause-flaking-job/Dockerfile .
kind load docker-image reference-pause-flaking:dev --name platform
# Then write a Deployment manifest (out of v0.1 scope) or run locally with --kubeconfig.

# 7. Watch events flow.
kubectl -n team-a get events --watch \
    --field-selector involvedObject.kind=Job

# 8. To force the action-taking path, edit the RR to make the job fail:
#    Set spec.args[0] to "echo failing; exit 1" and reapply.
#    After ~3 minutes you should see:
kubectl -n kratix-platform-system get agentproposals

# 9. Approve the proposal.
go run ./eventing/cmd/kratix-approve \
    --namespace=kratix-platform-system \
    --approver=you@example.com \
    <proposalId>

# 10. Verify the ScheduledJob was suspended.
kubectl -n team-a get scheduledjob nightly-cleanup -o jsonpath='{.spec.suspended}'
# Output: true
```

---

## The producer-checklist version

For your own Promise:

| Step | Artefact | Required? | Notes |
|---|---|---|---|
| 1 | `promise.yaml` — Promise CRD + workflow | **yes** | The public API. Keep the schema small. |
| 2 | `pipeline/` — configure pipeline | **yes** | Reads /kratix/input, writes /kratix/output. No K8s client. |
| 3 | `signals.yaml` — PromiseSignals taxonomy | strongly recommended | Producer documentation. Consumers read it instead of your source. |
| 4 | A signal *producer* (the analogue of `job-watcher/`) | only if you have non-Kratix transitions to surface | If everything you emit comes from existing Kratix controllers, you can skip this. |
| 5 | Read-only agent configuration | **strongly recommended** | Usually just a `HealthSummaryAgent` CR pinned to your signal namespace. |
| 6 | Action-taking agent runtime | only if a remediation is genuinely safe and worth automating | Use the escalation gate. Pick a low-blast-radius first action. |
| 7 | Sample RR | **yes** | Consumers need a working example to convince themselves it works. |
| 8 | `bundle.yaml` — PromiseBundle | **yes** | One manifest, one apply, automatic cascade on delete. Without it, your Promise is a half-assembled kit. |

---

## What this reference deliberately does not show

- **In-cluster Deployment manifests** for the runtimes. v0.1 of this reference is hybrid — runtimes can run locally (`go run`) against the kind cluster's apiserver. Writing the Deployment + ServiceAccount + RBAC manifests is mechanical and would double the file count without adding teaching value.
- **Multi-pattern escalation** (quorum, separation-of-duty, chained-expiry). The single-approver gate is the default; richer patterns live in `docs/escalation-gate-patterns.md` and are written as policy controllers stacked on top of the substrate.
- **Cross-cluster federation.** This Promise runs on one platform cluster. Worker scheduling via destinationSelectors is supported by Kratix itself.
- **Production hardening.** The runtimes here are reference-quality: single-replica, in-memory state, no leader election, no Prometheus metrics, no envtest. The substrate (forwarder, gate, kratix-emit) is more thoroughly hardened; the producer-side examples are deliberately minimal so the pattern is visible.

---

## See also

- [`docs/vision-cloudevents-signal-layer.md`](../../docs/vision-cloudevents-signal-layer.md) — the framing for why any of this exists
- [`eventing/WIRE-FORMAT.md`](../../eventing/WIRE-FORMAT.md) — the wire-format contract every producer agrees to
- [`docs/escalation-contract.md`](../../docs/escalation-contract.md) — the gate protocol the action-taking agent implements
- [`docs/escalation-gate-patterns.md`](../../docs/escalation-gate-patterns.md) — patterns for designing your own gate policy beyond v0.1 single-approver
- [`eventing/agents/health-summary/`](../../eventing/agents/health-summary/) — the generic agent the read-only configuration reuses
