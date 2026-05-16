# Escalation gate patterns

> **Companion to [`escalation-contract.md`](./escalation-contract.md).** The contract document tells you what the substrate guarantees. This document is the *user-facing* side: how to design the policy that runs on top.
>
> **Status:** v0.1 reference. Patterns marked **substrate-native** work today against the built-in escalation gate. Patterns marked **needs policy controller** describe shapes the substrate does not enforce — you write a small controller on top of `AgentProposal`.
>
> **Reader.** A platform engineer designing a gate for an action-taking agent. You've read the contract doc and understand `.proposed → .approved → .executed`. You now need to decide *who approves, on what evidence, with what fallback*.

---

## 1. What "designing an escalation gate" actually means

The substrate is fixed. You don't design the protocol; you design the **policy** sitting on top of it. Concretely:

| Decision | Where it's encoded |
|---|---|
| **Who can approve** | Kubernetes RBAC on `update agentproposals` |
| **How a human is notified** | A notifier (see `eventing/cmd/proposal-notifier`) outside Kratix |
| **What "approval" requires** | Either single-annotation (default) or a policy controller |
| **How long they have** | `spec.expiresAt` on each proposal |
| **What happens on expiry** | Either drop (default) or a chained-proposal pattern |

Most of these are K8s-native decisions. The substrate stays out of opinions because organisations have different ones.

---

## 2. The patterns

Each pattern below answers a different "what if" — "what if a single approval isn't enough?", "what if no human responds?", "what if the proposing agent's owner shouldn't be the approver?" Pick the ones that match your risk model. They compose.

---

### Pattern A — Single approver (substrate-native)

**Intent.** Any one authorised human can approve a proposed action. No quorum, no chain.

**When to use.** Default. Most actions, most environments. The simplest defensible gate.

**Mechanism.**
The substrate watches for `agents.kratix.io/approved-by` appearing on the `AgentProposal`. First write wins. The gate controller emits `.approved` and resolves the CR.

**RBAC.**

```yaml
# Give the oncall group permission to approve any AgentProposal in the
# platform namespace.
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: agent-proposal-approver
  namespace: kratix-platform-system
rules:
  - apiGroups: ["eventing.kratix.io"]
    resources: ["agentproposals"]
    verbs: ["get", "list", "watch", "update", "patch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: oncall-approves-proposals
  namespace: kratix-platform-system
subjects:
  - kind: Group
    name: oncall-sre
    apiGroup: rbac.authorization.k8s.io
roleRef:
  kind: Role
  name: agent-proposal-approver
  apiGroup: rbac.authorization.k8s.io
```

**Trade-offs.** Smallest blast radius for the *gate itself* — failures are bounded to "one wrong human approved." Worst-case for *action* blast radius if the action is destructive. Pair with conservative `expiresAt` and a meaningful rationale convention.

---

### Pattern B — Two-of-N quorum (**needs policy controller**)

**Intent.** Require N distinct approvers before the action is allowed.

**When to use.** Highest-blast-radius actions: prod database failover, cross-region traffic shifts, deletion of customer-owned data. Anything where a single rogue approver's mistake would be catastrophic.

**Mechanism.**

The substrate watches for the *single* `approved-by` annotation. To require quorum, run a small policy controller that:

1. Watches `AgentProposal` resources with a label `gate-policy=quorum`.
2. Looks at the *family* of annotations `agents.kratix.io/approved-by/<approver>` (one per human).
3. Emits the substrate-shape `agents.kratix.io/approved-by` annotation *only* once `|distinct approvers| >= N`.
4. The substrate sees that annotation and resolves the proposal normally.

The CR's `spec` can carry an extension:

```yaml
metadata:
  labels:
    gate-policy: quorum
  annotations:
    quorum.gate.kratix.io/required: "2"
```

**Pseudocode for the policy controller.**

```go
for prop := range watchProposals(labels={gate-policy: quorum}):
    if prop.Status.Resolution != "":
        continue
    required := atoi(prop.Annotations["quorum.gate.kratix.io/required"])
    approvers := distinctApprovers(prop)   // scan approved-by/<x> annotations
    if len(approvers) >= required:
        // Triggers the substrate's existing path.
        prop.Annotations["agents.kratix.io/approved-by"] = joinSorted(approvers)
        update(prop)
```

**RBAC.** Approvers each have permission to update *their own* annotation key — typically via an admission webhook that restricts which annotation keys a user can modify based on their identity. Simpler alternative: trust the approver tool to write the right key per user and audit via K8s audit logs.

**Trade-offs.** Doubles the policy surface. The policy controller becomes a critical-path component — its failure mode is "no proposal ever resolves." Make it idempotent and small. Test the recovery story.

---

### Pattern C — Author-cannot-self-approve (**needs policy controller**)

**Intent.** The human responsible for the proposing agent should not be able to approve its own proposals. Separation of duty.

**When to use.** Wherever the proposing agent is operated by a single team. The agent's owner is the most likely to rubber-stamp; the gate exists to bring in a second perspective.

**Mechanism.**

The policy controller knows the owner of each agent (e.g. from a label `agent-owner=team-a` on the proposing agent's Deployment, or a fixed mapping in config). On each `agentproposal` update, it:

1. Reads `metadata.annotations[agents.kratix.io/approved-by]`.
2. Looks up the proposer (`spec.actor`) and its owning team.
3. If the approver's identity is in that team, *strips* the annotation and emits an audit Event explaining why.

**Pseudocode.**

```go
ownerOf := config.AgentOwnerMap  // "agent/redis-flake-detector/v1" -> "team-a"
for prop := range watchProposals():
    approver := prop.Annotations["agents.kratix.io/approved-by"]
    if approver == "" { continue }
    owner := ownerOf[prop.Spec.Actor]
    if isMember(approver, owner):
        delete(prop.Annotations, "agents.kratix.io/approved-by")
        update(prop)
        emitEvent(prop, "Warning", "ApprovalRejected",
                  "%s cannot approve a proposal from their own team", approver)
```

**Trade-offs.** Has a TOCTTOU edge: between the policy controller observing and stripping, the substrate gate controller may have already emitted `.approved`. Mitigation: make the policy controller observe via a *validating admission webhook* on `AgentProposal` updates, so the strip happens at write time, before the substrate sees it. Heavier infrastructure but correct.

---

### Pattern D — Chained expiry escalation (**partial substrate + policy**)

**Intent.** "On-call doesn't respond in 15 min → escalate to on-call manager → escalate to director." A staircase of broader and broader audiences with shorter and shorter windows.

**When to use.** Actions that *must* happen if humans don't engage — emergency rollback, incident response. The default `expired` resolution means "do nothing"; this pattern says "escalate instead."

**Mechanism.**

The proposing agent emits the *first* proposal targeted at on-call, with `expiresAt = +15m`. The substrate handles approval / expiry as usual.

On `.expired`, a policy controller (or the proposing agent itself) emits a *new* proposal with:

- Same `action`, `subject`, `rationale`.
- New `proposalId` (it's a new proposal).
- Broader audience signalled by a label (`gate-audience=oncall-manager`).
- Shorter `expiresAt` (`+10m`).

The audience is enforced by RBAC: the new proposal's namespace, labels, or naming pattern is configured so a different RoleBinding applies.

**Pseudocode (in the proposing agent).**

```go
// Subscribed to agent.<my>.<action>.expired:
for ce := range expiredEvents:
    chain := chainPolicyFor(ce.Action)  // [oncall, oncall-manager, director]
    nextIdx := nextRung(ce, chain)
    if nextIdx >= len(chain):
        emitGivingUpAuditEvent(ce)
        return
    nextProposal := basedOn(ce,
        newId(), shorter(ce.ExpiresAt),
        labels={"gate-audience": chain[nextIdx]})
    emitProposed(nextProposal)
```

**Trade-offs.** Easy to get wrong in a way that creates infinite loops or notification storms. Cap the chain depth in config, not just in the agent. Add an absolute-deadline (`maximumChainDuration`) regardless of how many rungs are left.

---

### Pattern E — Time-of-day gate (**needs policy controller**)

**Intent.** "No prod changes between 18:00 and 08:00, except from the on-call rotation."

**When to use.** Change-management policies; environments with formal freeze windows; teams that want sleep.

**Mechanism.**

A policy controller watches `AgentProposal` creation. For each proposal targeting a labelled "production" subject:

1. Compute current time in the configured time zone.
2. If outside business hours, check whether the approver (when one appears) is in the `oncall-now` group. If not, strip the approval annotation and emit an audit Event.

Or, more aggressively: the policy controller can mark the proposal as *non-approvable* by setting a status condition `Approvable=False/OutsideBusinessHours`. A UI / notifier can then refuse to ping the wrong audience.

**Trade-offs.** Time-zone logic is a foot-gun. Test daylight-saving transitions. Make the schedule itself a CRD or ConfigMap so changing it doesn't require redeploying the policy controller.

---

### Pattern F — Read-only proposals (no human in the loop) (substrate-native)

**Intent.** Some "proposals" are informational: an agent reporting an observation it could not have acted on anyway. No gating required.

**When to use.** Sparingly. The whole point of the proposal mechanism is the gate; bypassing it is suspicious. Legitimate cases: an audit agent that exists to *record* recommendations a human will revisit in a quarterly review, not to act on them.

**Mechanism.**

The proposing agent emits `.proposed` with `expiresAt = +1y` (or whatever your retention horizon is) and the proposal is *never approved*. The CR remains in `AwaitingApproval` until manually deleted or expired. A separate reporting tool reads the queue.

**Better alternative.** Don't use the gate. Emit a non-gated event type (`agent.audit.recommended` instead of `agent.audit.proposed`) and let it flow through the regular forwarder. The gate is for *gating*; if there is no gate, there is no gate.

**Trade-offs.** Mostly anti-pattern. Listed here because users *will* reach for it, and we should name it so they can resist it.

---

## 3. RBAC patterns

Two RBAC shapes recur. Worth pinning so you don't reinvent them.

### Pin approver permissions to a namespace

The gate controller materialises CRs in a single namespace (default: `kratix-platform-system`). Granting approval rights namespace-wide is appropriate for most teams:

```yaml
# Anyone in the oncall group can approve any proposal in the gate namespace.
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: oncall-approves
  namespace: kratix-platform-system
subjects:
  - kind: Group
    name: oncall-sre
roleRef:
  kind: Role
  name: agent-proposal-approver
```

### Pin approver permissions to a *label selector*

For multi-tenant gates, scope approval rights by the proposing agent or subject:

```yaml
# RBAC at the resource level is limited; you can't natively select on
# labels in RBAC. Workaround: a label-based namespace, or an admission
# webhook that enforces "user X can only approve proposals labelled team=X."
```

K8s RBAC is coarse on this. Most teams reach for an admission webhook (Kyverno, Gatekeeper, OPA) once they need fine-grained scoping. That's fine and expected — RBAC is the floor, not the ceiling.

---

## 4. Notifier patterns

The substrate emits `.proposed` events on the CloudEvent bus. *How a human learns there's something to approve* is a separate concern. The reference notifier (`eventing/cmd/proposal-notifier`) takes the generic approach: it translates `.proposed` CEs into HTTP POSTs to a configured target URL.

**Common notifier shapes:**

| Shape | When to use |
|---|---|
| Webhook → Slack incoming webhook | Default for most teams. Cheap, easy. |
| Webhook → Microsoft Teams workflow | Same shape, different target. |
| Webhook → PagerDuty Events API | Where a missed approval has on-call consequences. |
| Webhook → custom UI | A purpose-built approval dashboard. The most polished but most build cost. |
| Webhook → email digest | Lowest-urgency audiences (e.g. weekly capacity reviews). |

All of these are the *same* notifier with different target URLs. The reference implementation deliberately doesn't embed Slack-specific concerns — it relays a generic envelope and lets the user choose what receives it.

If you need Slack Block Kit formatting (richer than a plain text webhook), the reference notifier ships with a `--format=slack` mode that does the translation in the relay rather than expecting a richer message format upstream.

---

## 5. Audit patterns

The full CE chain is the audit trail by design. Each proposal generates 2–3 events:

```
agent.<X>.proposed   (the agent — "I think we should...")
agent.<X>.approved   (the gate — "human authorised at T+...")
agent.<X>.executed   (the agent — "done, here's the outcome")
```

Or, the alternate path:

```
agent.<X>.proposed
agent.<X>.expired
```

**Common audit shapes:**

- **Cluster-local retention.** K8s Events default to a 1h TTL. Sufficient for debugging, not for compliance. Increase via apiserver config or copy events to an external store.
- **Loki / Elasticsearch sink.** Most teams will fan all `agent.*` CEs into their existing log pipeline. Index on `agents.kratix.io/correlation-id` so the chain is queryable.
- **S3 cold storage.** For long-term retention. Object key by `{date}/{namespace}/{proposalId}.json`.
- **SIEM integration.** Most enterprise SIEMs accept CloudEvents over HTTPS directly.

The `AgentProposal` CR itself is *not* the long-term audit object. It's deleted (or left to be garbage-collected) after resolution. The CE chain is what survives.

---

## 6. Worked example: putting it together

**Scenario.** A bank's platform team wants to ship a `redis-failover-agent` that proposes primary failovers when replication lag is sustained. The action — flipping the primary — is high-blast-radius. The team wants:

1. Only on-call SREs can approve.
2. Two approvers required (separation of duty).
3. The proposing agent's owning team (the Redis platform team) cannot approve their own agent's proposals.
4. If approval doesn't land in 15 min, escalate to the on-call manager.
5. No approvals between 02:00 and 06:00 UTC except from the dedicated overnight rotation.
6. Every step in the chain logs to the bank's SIEM.

That's five patterns layered onto one substrate.

**Architecture.**

```
┌────────────────────────────────────────────────────────────────────┐
│                    redis-failover-agent (proposer)                  │
└──────────────────────────────┬─────────────────────────────────────┘
                               │ emits .proposed (CE)
                               ▼
┌────────────────────────────────────────────────────────────────────┐
│              escalation-gate controller (substrate)                 │
│        materialises AgentProposal CR; watches for approval          │
└──────────────────────────────┬─────────────────────────────────────┘
                               │ creates AgentProposal
                               ▼
┌────────────────────────────────────────────────────────────────────┐
│  policy controller stack (this team's design)                       │
│    1. quorum controller       — needs 2 distinct approvers          │
│    2. separation-of-duty      — strips approvals from owning team   │
│    3. time-of-day controller  — strips approvals outside windows    │
│    4. chained-expiry handler  — re-proposes on .expired             │
└──────────────────────────────┬─────────────────────────────────────┘
                               │ resolved AgentProposal
                               ▼
┌────────────────────────────────────────────────────────────────────┐
│       notifier → Slack #oncall-alerts (per audience routing)        │
└────────────────────────────────────────────────────────────────────┘
                               │ all CEs fan-out
                               ▼
┌────────────────────────────────────────────────────────────────────┐
│        forwarder → SIEM sink (audit retention)                      │
└────────────────────────────────────────────────────────────────────┘
```

**The substrate carries the protocol.** Everything in the policy controller stack is the bank's own controller code, run as Deployments. Each one is small (under 200 lines) because it operates on a single concern: read CRs, apply a rule, mutate or emit.

**The notifier is the same generic one.** A second instance targets a different Slack channel (`#oncall-manager-alerts`) for the chained-expiry rung. A third instance routes audit-only events to a logging endpoint. One image, three Deployments.

**The SIEM is just another CloudEventSink.** No bank-specific code in the eventing subsystem — the bank's SIEM team configures the sink URL in their existing infrastructure.

**Total custom code.** Four small policy controllers + the proposing agent. Everything else is the substrate.

This is what "designing an escalation gate" looks like: composing patterns over a fixed protocol. The substrate doesn't grow as policies get richer — *the policy controllers do*. That's by design.

---

## 7. Anti-patterns

The shapes that look tempting but you'll regret:

| Anti-pattern | Why it bites you |
|---|---|
| **Bake policy into the proposing agent.** | "The agent itself decides who can approve." Couples policy to the agent's code, breaks the substrate's promise of generic gating. Now every agent reimplements RBAC. |
| **Use the substrate for non-gated workflows.** | If there's no human in the loop, you don't need `.proposed` — just emit a normal CE. The gate exists to gate; bypassing it makes the audit trail noisier and the operational story confusing. |
| **Make `expiresAt` huge.** | "Give them a week to decide." A proposal that lingers is a proposal someone has stopped thinking about. Long `expiresAt` is correlated with proposals that should have been read-only events instead. |
| **Manual approval via `kubectl annotate`.** | Works for ops; doesn't scale. You'll forget who approved what. Always route through a notifier + an approver tool that records the approver's identity from SSO, not from the operator's terminal. |
| **Approve before notifying.** | Auto-approval by a Slack bot "for low-risk cases" defeats the gate. If the case is low-risk, it shouldn't go through the gate. Patterns A and F are the right tools. |

---

## 8. Where to go next

- The substrate: [`escalation-contract.md`](./escalation-contract.md) for the protocol, [`vision-cloudevents-signal-layer.md`](./vision-cloudevents-signal-layer.md) for the wider context.
- The reference notifier: `eventing/cmd/proposal-notifier/README.md`.
- The approver CLI: `eventing/cmd/kratix-approve/`.
- Build your own policy controller: start from the patterns above, the controller is usually <200 lines of `client-go` plus your rule.
