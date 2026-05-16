# proposal-notifier

Reference notifier for the escalation gate. Receives `agent.*.proposed` CloudEvents from `kratix-event-forwarder` (via a `CloudEventSink`) and POSTs a notification to a configured external target — typically a Slack/Teams/n8n incoming webhook.

The notifier is deliberately tool-agnostic. Pick a format:

| Flag | Outgoing payload | Targets |
|---|---|---|
| `--format=generic` (default) | A flat JSON envelope (`proposalId`, `action`, `actor`, `subject`, `rationale`, `expiresAt`, `expiresIn`, `approveCmd`, `kubectlCmd`, …) | Anything that accepts JSON POSTs — webhook.site, n8n, custom UIs, email-via-webhook, your internal portal. |
| `--format=slack` | Slack Block Kit JSON (`text` fallback + structured `blocks`) | Slack incoming webhooks. |

To plumb in a different format (Teams Adaptive Cards, PagerDuty Events v2, etc.) drop in a tiny `<format>.go` that takes `genericEnvelope` and writes the target-native shape, then add the case to `relay.encode`. The wire receiver is shared.

## Quick start (local)

```sh
# 1. Run the notifier locally, pointed at a webhook.site URL:
go run ./eventing/cmd/proposal-notifier \
    --target=https://webhook.site/<your-uuid> \
    --format=generic \
    --listen=127.0.0.1:8091

# 2. In the cluster, create a CloudEventSink routing proposals to it.
#    (For local testing, port-forwarding into the cluster or running the
#    forwarder locally too is easier than a real sink.)
```

For an end-to-end test against a real kind cluster, see the smoke-test recipe in `docs/escalation-gate-patterns.md`.

## Slack incoming webhook example

```sh
go run ./eventing/cmd/proposal-notifier \
    --target=https://hooks.slack.com/services/T000/B000/<token> \
    --format=slack
```

Renders proposals as a Slack message with action / subject / rationale / expiry fields, plus a copy-paste `kratix-approve ...` block.

## What gets sent (generic format)

```json
{
  "event":         "agent.redis.failover.proposed",
  "action":        "failover",
  "actor":         "agent/redis-flake-detector/v1.2.0",
  "subject":       "default/promise/redis",
  "rationale":     "3 lag spikes > 30s in last 10m",
  "proposalId":    "01HZ9A0000000000000000000",
  "namespace":     "kratix-platform-system",
  "expiresAt":     "2026-05-15T19:30:00Z",
  "expiresIn":     "12m30s",
  "correlationId": "01HZ8W000000000000000000",
  "approveCmd":    "kratix-approve 01HZ9A... --namespace=kratix-platform-system --approver=<you>",
  "kubectlCmd":    "kubectl -n kratix-platform-system get agentproposal 01HZ9A... -o yaml"
}
```

## What the notifier does *not* do

- **No state.** Restart loses nothing — every event is processed independently.
- **No auth on /events.** The expectation is the forwarder runs inside the cluster and the notifier sits behind a `ClusterIP` Service. If you expose `/events` publicly, terminate auth at a sidecar.
- **No retries beyond per-request timeout.** A failed POST to the target is logged and the event is `202`-acked back to the forwarder. The forwarder is at-least-once; if you need exactly-once notification semantics, the target needs to be idempotent on `proposalId`.
- **No threading / mute / acknowledgement back into the gate.** A Slack thread reply does not approve the proposal. Approval is always via `kratix-approve` or `kubectl annotate`. This is deliberate — the approver tool's job is to be the *only* path that writes the annotation.
- **No per-proposal routing.** All `agent.*.proposed` events go to the same target. To route by audience (e.g. on-call vs on-call-manager for chained escalation), run multiple notifier Deployments with distinct `CloudEventSink` filters.

## Plumbing into the cluster

Once the kratix-event-forwarder is running, create a `CloudEventSink`:

```yaml
apiVersion: eventing.kratix.io/v1alpha1
kind: CloudEventSink
metadata:
  name: notifier-oncall
spec:
  url: http://proposal-notifier.kratix-platform-system.svc:8080/events
  typeFilter:
    - "agent.*.proposed"
```

The notifier is one HTTP target; subscription routing is done at the forwarder, not in the notifier. That's the substrate's promise: the gate emits, the forwarder fans out, the notifier just plays the receiving end of one fan-out lane.
