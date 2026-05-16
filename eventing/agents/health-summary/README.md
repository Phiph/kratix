# health-summary-agent

The first Kratix-native agent. A read-only observer of the Kratix CloudEvent stream that publishes a rolling 1-hour digest as its own CloudEvent.

This agent is built as a **Kratix Promise** — installing it is the same as installing any other Promise, and creating instances is the same as creating any other Resource Request. See [`../../docs/vision-cloudevents-signal-layer.md`](../../../docs/vision-cloudevents-signal-layer.md) §6 for why agents are modelled this way.

## What's here

```
health-summary/
├── promise.yaml              The Promise (CRD + workflow)
├── pipeline/                 Configure-workflow container source
│   ├── main.go               Reads /kratix/input, writes /kratix/output
│   ├── main_test.go
│   └── Dockerfile
├── runtime/                  Long-running agent source
│   ├── main.go               HTTP receiver + digester wiring
│   ├── window.go             In-memory rolling window
│   ├── receiver.go           CloudEvent HTTP handler
│   ├── digester.go           Schedule-driven digest emitter
│   ├── *_test.go
│   └── Dockerfile
└── examples/
    └── health-summary-agent.yaml   Sample Resource Request
```

## How it works

1. **Install** the Promise: `kubectl apply -f promise.yaml`. Kratix registers the `HealthSummaryAgent` CRD and the configure workflow.
2. **Create** an instance: `kubectl apply -f examples/health-summary-agent.yaml`. Kratix runs the pipeline.
3. **The pipeline** reads the RR and writes manifests to `/kratix/output/`:
   - `Deployment` for the runtime
   - `Service` exposing `:8080/events`
   - `ConfigMap` carrying `digestSchedule` + `subscribe`
   - `ServiceAccount` + `Role`/`RoleBinding` (events:create in this namespace)
   - `CloudEventSink` routing matching events to the agent's Service
4. **The runtime** starts, listens on `:8080`, and the forwarder begins POSTing matching CloudEvents.
5. **On schedule** (`@hourly` by default), the runtime emits a digest as a Kubernetes Event with `kratix.io/ce-type=agent.health-summary.digest.published`. The forwarder picks it up and fans it out — downstream agents can subscribe to it like any other event.

## Digest shape

The digest is published as a CloudEvent of type `agent.health-summary.digest.published`. The payload is carried on the originating Event's `kratix.io/ce-data` annotation as JSON:

```jsonc
{
  "windowStart": "2026-05-15T18:00:00Z",
  "windowEnd":   "2026-05-15T19:00:00Z",
  "totals": { "info": 142, "warning": 7 },
  "bySubject": [
    {
      "subject":  "default/promise/redis",
      "counts":   { "warning": 3, "info": 12 },
      "topTypes": ["kratix.promise.unavailable", "kratix.work.failed"],
      "total":    15
    }
  ]
}
```

## Boundaries (v0.1)

- **State is in-memory only.** Agent restart drops the window. Next digest starts fresh. Acceptable because the K8s Event TTL is the same horizon.
- **Single replica.** No leader election. Scaling the Deployment to 2 replicas would double-count events.
- **Schedule is duration-based.** `@hourly`, `@daily`, `@every <duration>`. Full cron is deferred.
- **No agent-side filtering.** The `CloudEventSink` does all filtering before events reach the agent. The `subscribe` field on the RR is the only knob.
- **No alerting, no remediation.** This agent is observation-only. Action-taking agents come later with a human-in-the-loop gate (see vision doc §6).

## Why this is the first agent

It has no human-in-the-loop concerns (it takes no action), so it proves the loop end-to-end without needing the gate primitive. Once it's running, the digest event is itself a CloudEvent — a future flake-detector agent can subscribe to digests and act on patterns, and *that* agent is where the human gate matters.
