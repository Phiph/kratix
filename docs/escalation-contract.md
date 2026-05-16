# Escalation contract for Kratix agents

> **Status:** design sketch, v0.1. Not implemented. The shape proposed here is what code, when written, should target. Decisions are deliberately *opinionated and few* — if a decision can wait, it does.
>
> **Audience:** Kratix platform team and producers building action-taking agents. Read after `eventing/WIRE-FORMAT.md` and the [vision doc](./vision-cloudevents-signal-layer.md).

---

## 1. Why escalation needs a contract

Read-only agents (`health-summary-agent`) are easy: observe, summarise, emit. They take no action; no gate is required.

Action-taking agents are the interesting ones — and the dangerous ones. A flake-detector that triages a failing Redis replica is useful. The same flake-detector failing over a primary at 3am without authorisation is an incident.

The vision doc commits to **human-in-the-loop as a first-class primitive**. This document defines what that primitive *is*. The constraint we are designing for:

> Any agent capable of mutating state MUST hand off to a human before doing so. The handoff is auditable, time-bounded, and itself observable as part of the CloudEvent stream.

The substrate (CloudEvents over the forwarder) does not change. Escalation is a *pattern* over the substrate, not a new mechanism. That's the load-bearing decision.

---

## 2. The event flow

Four CloudEvent types per action, with reserved suffixes:

```
agent.<domain>.<action>.proposed
agent.<domain>.<action>.approved
agent.<domain>.<action>.executed
agent.<domain>.<action>.expired
```

Worked example — a Redis flake-detector proposing a primary failover:

| Step | Event type | Emitted by | Means |
|---|---|---|---|
| 1 | `agent.redis.failover.proposed` | flake-detector agent | "I think we should fail over. Here is why and how. Waiting for approval." |
| 2a | `agent.redis.failover.approved` | the approver (see §4) | "Approved by `phill@example.com` at `T+3m`." |
| 2b | `agent.redis.failover.expired` | flake-detector agent (or a watchdog) | "Proposal timed out without approval. No action taken." |
| 3 | `agent.redis.failover.executed` | flake-detector agent | "Action complete. Outcome attached." |

Either `2a → 3` or `2b → (no further events)`. Never both. The agent that emitted `proposed` is the only one that may emit `executed`.

### Reserved suffixes

The four suffixes (`.proposed`, `.approved`, `.executed`, `.expired`) are reserved within `agent.*` CE types. Producers MUST NOT emit `agent.*.proposed` for telemetry purposes — that namespace is the escalation contract.

This is the equivalent of HTTP reserving methods. The substrate stays generic; the semantic comes from the suffix.

---

## 3. The proposal payload

Every `.proposed` event MUST carry, on its `kratix.io/ce-data` annotation, a JSON object with at least:

```jsonc
{
  // What is being proposed
  "action":      "failover",                // human-readable verb
  "actor":       "agent/redis-flake-detector/v1.2.0",  // proposer identity
  "subject":     "default/promise/redis-primary",       // the affected object (mirrors CE subject)

  // Why it should happen
  "rationale":   "3 lag spikes > 30s in last 10m",
  "evidence":    {
    "correlationIds": ["01HZ8W...", "01HZ8X...", "01HZ8Y..."],
    "since":          "2026-05-15T18:50:00Z"
  },

  // How to execute it — opaque to the substrate, meaningful to the agent
  "plan":        { /* agent-specific */ },

  // Gate parameters
  "proposalId":  "01HZ9A...",               // ULID — used as approval target
  "expiresAt":   "2026-05-15T19:30:00Z"     // approval window
}
```

Notes:

- `proposalId` is the durable identity of the proposal. It survives forwarder restarts, replays, and multiple consumers. Approvers reference *this*, not the CE `id`.
- `expiresAt` is producer-set. There is no global TTL — different actions have different urgency. The producer is responsible for picking a sensible window.
- `plan` is opaque. The substrate does not interpret it; the proposing agent is the only entity that knows how to execute it. This keeps Kratix out of the action-execution business.
- `evidence.correlationIds` link back to the originating reconcile loops. This is how an operator answers "what did the agent see?" — a chain of CE correlation IDs they can trace through the forwarder's audit log.

---

## 4. The approver primitive

The hardest decision. Three options were considered; one is chosen for v0.1.

### Considered: webhook approver

A signed HTTP request to the agent's `/approve` endpoint. Mirrors the GitHub approval model. **Rejected** for v0.1: requires every agent to host an authenticated endpoint, multiplies attack surface, and pushes the auth problem onto agent authors.

### Considered: dedicated `Proposal` CRD

A `Proposal` CR that approvers patch to set `status.approved=true`. **Rejected** for v0.1: adds a third API surface alongside the Promise + agent CRD, requires an admission/RBAC story for the patch, and turns "approve" into "have a kubectl context with the right credentials" — which most operators already do anyway, but now via a custom resource rather than the standard mechanism.

### Chosen: annotation-on-proposal-CE-mirror

The forwarder, when it sees a `.proposed` event, mirrors it as a *new lightweight CR* (`AgentProposal`) into the agent's namespace. The CR's spec carries the proposal payload verbatim. Approvers run:

```sh
kubectl annotate agentproposal/<proposalId> \
    agents.kratix.io/approved-by=phill@example.com \
    --namespace=observability
```

A small controller (the **escalation gate controller**, part of the eventing subsystem) watches `AgentProposal` resources. When it sees the `approved-by` annotation appear, it emits the matching `.approved` CloudEvent. The proposing agent consumes that CE and acts.

When `expiresAt` passes without an approval, the gate controller emits `.expired` and deletes the `AgentProposal`. The CR is garbage-collected automatically; the event chain is the audit trail.

#### Why this option

1. **Approval is standard `kubectl annotate`.** No new auth surface. RBAC on `AgentProposal/approved-by` is the access-control story, and operators already understand it. A team can grant `update:agentproposals` to a Slack bot, a UI, an oncall rotation — whoever has the credentials.
2. **The CR is the audit object.** What was proposed, by whom approved, when, in one place. `kubectl get agentproposals --all-namespaces` is the "what's waiting on a human" view.
3. **The CR has a finite lifetime.** Created on `.proposed`, deleted on `.approved` or `.expired`. Storage stays bounded. The CloudEvent stream is the long-term audit trail; the CR is the short-lived approval surface.
4. **No agent hosts an approval endpoint.** Agents subscribe to `.approved` events like any other CE. The gate controller is the one moving part that needs the K8s API.
5. **The proposal can be inspected before approval.** `kubectl describe agentproposal/<id>` shows the rationale, the evidence, the plan. Approvers have a single command to read before they annotate.

#### Cost

A new controller (the escalation gate) and a new CRD (`AgentProposal`). The CRD is mechanical; the controller is ~200 lines (watch CRs, watch annotations, emit CEs, garbage-collect). Both belong in `eventing/`.

---

## 5. The `AgentProposal` CRD (sketch)

This is the shape; not yet written.

```yaml
apiVersion: eventing.kratix.io/v1alpha1
kind: AgentProposal
metadata:
  name: 01HZ9A0000000000000000000   # proposalId from §3
  namespace: observability
spec:
  proposedEventType: agent.redis.failover.proposed
  actor:             agent/redis-flake-detector/v1.2.0
  subject:           default/promise/redis-primary
  action:            failover
  rationale:         "3 lag spikes > 30s in last 10m"
  evidence:
    correlationIds: [...]
    since:          2026-05-15T18:50:00Z
  plan:              { ... }                  # opaque blob
  expiresAt:         2026-05-15T19:30:00Z
status:
  conditions:
    - type:   Ready
      status: True
      reason: AwaitingApproval
  approvedBy: ""        # populated by the gate controller from the annotation
  approvedAt: null
  resolution: ""        # "approved" | "expired" | "executed"
```

Cluster-scoped or namespaced? **Namespaced.** Proposals attach to a specific agent instance in a specific namespace; the CR lives where the agent lives.

---

## 6. Idempotency and edge cases

The interesting ones, decided so they don't surprise an implementer:

- **Double-approval.** If a human annotates an already-approved proposal, the gate controller is a no-op. The first annotation wins; the CR's `status.approvedBy` is immutable once set.
- **Approval after expiry.** If `kubectl annotate` lands after `expiresAt`, the gate controller rejects it. The proposal is already in resolution `expired`; the annotation is ignored with a warning Event. **Do not** retroactively approve — the agent has already been told to stand down.
- **Agent crash between `.approved` and `.executed`.** On restart, the agent must reconcile its outstanding approvals — query `AgentProposal` for any with `status.resolution=approved` matching its actor identity, and execute. This is the agent's idempotency problem; the gate controller does not retry.
- **Multiple humans approving in parallel.** First write wins on the annotation; the second `kubectl annotate` will see the field already set and the gate controller will ignore the second update. (Standard K8s last-write-wins semantics; the immutability check is in the controller.)
- **Replay.** A `.proposed` event arriving twice for the same `proposalId` is a no-op: the gate controller creates the CR on first sight, dedupes on second. This is what makes `proposalId` worth more than the CE `id`.

---

## 7. What this contract deliberately does *not* address

Out of scope for v0.1; flagged so we don't quietly need them:

- **Multi-approver / quorum approval.** "This requires sign-off from two SREs." Plausibly needed for high-blast-radius actions. Not built. When needed, extend `AgentProposal.spec` with an `approvers` list and gate the controller on it.
- **Approver authentication beyond K8s RBAC.** If your approver is a Slack bot acting on a typed `/approve` command, you have a Slack-side auth problem that lives outside this contract.
- **Rollback / undo.** `.executed` is terminal. If an action turns out to be wrong, a *new* proposal must be made to reverse it. The agent doesn't model undo; the operator does.
- **Cross-cluster proposals.** Approvers and proposers are in the same cluster. Federation is future.
- **UI.** A nice "what's pending approval" dashboard would help. Out of scope; the CRD plus existing K8s tooling is enough for v0.1.

---

## 8. Producer responsibilities

If you ship an action-taking agent:

1. **Declare the action's reserved CE types** in your `PromiseSignals` document:
   ```yaml
   - type: agent.redis.failover.proposed
     severity: warning
     stability: alpha
     description: Agent has proposed a primary failover.
   - type: agent.redis.failover.approved
     severity: info
     ...
   - type: agent.redis.failover.executed
     severity: warning
     ...
   - type: agent.redis.failover.expired
     severity: info
     ...
   ```
2. **Pick a sensible `expiresAt`** for each action. Failover: 15 minutes. Capacity expansion: 24 hours. "Restart this pod": 5 minutes.
3. **Document the action's blast radius** in the rationale text. Approvers read this before annotating.
4. **Be idempotent on `.approved`.** Your agent may restart between approval and execution.
5. **Emit `.executed` regardless of outcome** — success or failure. The audit chain must close.

---

## 9. Implementation order (when we build this)

Roughly four steps, each independently shippable:

1. **`AgentProposal` CRD** (`eventing/api/v1alpha1/agentproposal_types.go`). Same scaffolding as `PromiseSignals` and `CloudEventSink`. No controller yet; just the type.
2. **Escalation gate controller** (`eventing/cmd/escalation-gate/`). Watches `AgentProposal` + Events; emits `.approved` / `.expired`; deletes resolved CRs.
3. **First action-taking agent.** Pick a small one (not Redis failover — something genuinely safe even if it goes wrong, like "reset the failed-jobs counter"). Wire the full proposed→approved→executed loop.
4. **A reference approver.** Even a CLI (`kratix-approve <proposalId>`) that wraps `kubectl annotate`. Lowers the barrier; ensures the contract is usable.

---

## 10. Open questions

The handful of decisions deliberately left to whoever picks this up:

1. **Where does the gate controller's leader election live?** Single-replica is fine for v0.1, but the moment there's a second cluster the question becomes load-bearing. Defer until then.
2. **Should `AgentProposal` carry a digital signature of the proposal payload?** Useful if you don't trust the originating agent's image. v0.1 trusts agents (they're in your cluster); a future version may not.
3. **Do `.approved` events themselves need an approval gate?** Only matters if a malicious agent could forge them. The gate controller is the *only* legitimate emitter of `.approved`, and that's enforced by the gate controller being the only thing with the SA that can post those — which is also where the contract gets enforced by RBAC. Worth documenting more sharply when written.
