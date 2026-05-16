# Kratix eventing vs Knative, CloudEvents SDK, and friends

> **Status:** design rationale, not a roadmap. Written after enough of the substrate existed to make the comparison honest. Audience: anyone evaluating Kratix who already knows the CloudEvents ecosystem and is wondering "why didn't you just use Knative?"

---

## TL;DR

We built our own substrate on purpose. Kratix's value proposition is being **a self-contained platform-engineering primitive**: install Kratix, get the whole pattern (Promises, Resource Requests, Workflows, and now events + agents + gates). Bolting onto Knative would have meant operators install *two* platforms to get one platform pattern — the opposite of the Kratix promise.

The novel contributions are the **`PromiseSignals` taxonomy CRD**, the **`PromiseBundle` packaging CRD**, and the **`AgentProposal` + escalation gate**. The plumbing underneath (forwarder, wire format, emit helper) is reinvention of things Knative and the CloudEvents SDK already do well. That reinvention is a deliberate cost we pay for cohesion. This document explains *why*, *what* we'd reuse if we changed our mind, and *how* a platform team running Knative anyway can bridge.

---

## 1. The design stance

> **Kratix is its own system.**
>
> Producers shouldn't have to install, reason about, or operate a second event-routing platform to use Kratix's eventing primitives. The cost of building our own substrate is that we maintain it; the benefit is that "install Kratix" is the complete onboarding instruction.

That sentence is the load-bearing one. Everything else follows.

What this commits us to:

- **One install.** Operators run `kubectl apply -k` against Kratix and get Promises + RRs + Events + Agents + Gates. No prerequisites.
- **One conceptual model.** A platform engineer learns Promises and then learns "agents are Promises that subscribe." They don't context-switch into Knative `Broker`/`Trigger`/`Source`/`Subscription` shapes.
- **One CRD surface.** `Promise`, `PromiseSignals`, `PromiseBundle`, `CloudEventSink`, `AgentProposal`, `HealthSummaryAgent`. Six CRDs total at v0.1. Knative Eventing alone adds five before you start adding event types.
- **One audit trail.** Everything is K8s Events + the CRDs above. Two-source-of-truth problems (some events in Knative, some in K8s, audit chains spanning both) don't arise.

What this gives up:

- **Reinvention work.** We maintain a forwarder, a wire format, a Go helper for emitting. CloudEvents SDK + Knative would have given us all of this.
- **Ecosystem leverage.** A Knative team's Slack integrations, sources, and sinks don't drop into our substrate as-is.
- **Industry mindshare.** "Send a CloudEvent" is a phrase a CNCF-fluent engineer knows; "write an annotated K8s Event the forwarder picks up" is a phrase only Kratix users know.

The trade is intentional. We are not a generic event-routing platform; we are a platform-engineering primitive that *happens to need* event routing for its agent layer. The substrate's quality bar is "good enough to support our agent patterns," not "competitive with Knative."

---

## 2. What each tool would have given us

A mapping from the off-the-shelf ecosystem to what we built. Honesty about what is reinvention and what isn't.

### Knative Eventing

The closest match. A CNCF project, K8s-native, production-tested.

| Knative concept | Our equivalent | Honest verdict |
|---|---|---|
| `Broker` | The forwarder + its internal fan-out | Reinvented. Knative's broker is multi-backend (in-memory, Kafka, RabbitMQ, NATS, Pub/Sub); ours is in-memory only. |
| `Trigger` (CE filter + Subscriber URL) | `CloudEventSink` CRD | Reinvented. Knative's `Trigger` supports CEL-based filters; ours is glob-only. |
| `ApiServerSource` (watches K8s API → emits CEs) | The forwarder watching K8s Events | Reinvented. ApiServerSource has been a stable Knative resource for years; it's exactly the producer-side bridge we hand-rolled. |
| `Channel` + `Subscription` | The forwarder's internal fan-out | Reinvented. |
| `EventType` registry | `PromiseSignals` CRD | **Genuinely overlapping.** Knative has `EventType` for registering CE types cluster-wide; ours adds Promise attribution, stability levels, payload schema, and producer ownership. Closer to AsyncAPI in scope than to Knative `EventType`. |

**What Knative would *not* have given us:** the escalation gate, the bundle packaging, the agent-as-Promise pattern. Those are Kratix-native.

### CloudEvents Go SDK (`github.com/cloudevents/sdk-go/v2`)

The reference implementation of the CE spec.

| CE SDK feature | Our equivalent | Verdict |
|---|---|---|
| Envelope construction (structured + binary mode) | Hand-rolled JSON in `eventing/cmd/forwarder/translate.go` | Reinvented. The SDK's `cloudevents.NewEvent()` would have done this in three lines. |
| HTTP client with retries, idempotency, content-type negotiation | `http.Post` in the forwarder + notifier | Reinvented. The SDK's `cloudevents.NewClientHTTP()` handles all of this. |
| Receivers + middleware | Our `http.Handler` for `/events` | Reinvented. |
| Type-safe CE attribute setters/getters | Hand-rolled struct + JSON tags | Reinvented. |

**Worth re-considering.** Importing the SDK for emit + receive is cheap and would save us from subtle CE-spec-compliance bugs (e.g. our `id` regeneration, our content-type handling). The SDK doesn't constrain how we route events; it just handles the envelope. **This is the lowest-friction Knative-adjacent thing we could adopt without changing our substrate's shape.**

### W3C Trace Context (`traceparent` / `tracestate`)

The cross-vendor standard for distributed-trace correlation.

| W3C concept | Our equivalent | Verdict |
|---|---|---|
| `traceparent` header / extension | `kratix.io/ce-correlation-id` annotation | Reinvented. `traceparent` is *already* a CloudEvents extension. Our ID is a random hex string with no trace-graph semantics; W3C IDs are structured with span/parent relationships. |
| OpenTelemetry context propagation | `lib/eventemit.WithCorrelationID(ctx)` | Reinvented. OTel has `propagation.TraceContext{}` doing this exact thing. |

**Worth re-considering.** Switching to `traceparent` would give us free interop with every OTel-instrumented system at no cost in functionality. The annotation key would change once; everything downstream still queries `correlation-id` the same way. v0.2 cleanup.

### AsyncAPI

The de-facto standard for documenting event-driven APIs (analogous to OpenAPI for REST).

| AsyncAPI concept | Our equivalent | Verdict |
|---|---|---|
| `channels` + `messages` schema | `PromiseSignals.spec.events[]` | Partially reinvented. AsyncAPI is YAML/JSON-Schema-based and lives in a producer's repo; we put the same information in a CRD that's queryable in-cluster. |

**Our version has a genuine advantage:** because it's a CR, you can `kubectl get promisesignals` and see the live cluster's event taxonomy. AsyncAPI is design-time documentation; `PromiseSignals` is runtime queryable contract. **This is one place where our reinvention is actually better for our use case.** Worth noting in the doc, not migrating to AsyncAPI.

### Helm / Kustomize

The de-facto K8s packaging tools.

| Helm/Kustomize feature | Our equivalent | Verdict |
|---|---|---|
| Chart dependencies (`Chart.yaml.dependencies`) | `PromiseBundle.spec.companions` | Reinvented. Helm has been doing "this thing ships with these other things" since 2016. |
| Kustomize `components` | `PromiseBundle` with inline companions | Reinvented. |
| Release/uninstall lifecycle | `PromiseBundle` owner-reference cascade | Reinvented. Helm's release lifecycle is more mature (rollback, history, hooks). |

**Worth re-considering — but maybe not.** Helm is a *tool*; `PromiseBundle` is a *Kubernetes-native resource* with a controller that reconciles. The control-loop semantics matter to us — a Helm chart sits inert until someone runs `helm upgrade`; a `PromiseBundle` reconciles continuously, applies on Promise availability, and cascades on delete. If we wanted Helm semantics, we'd shell out to Helm. We want controller semantics; we built a controller.

### Argo Events

A CNCF project for declarative event-triggered workflows.

| Argo Events feature | Our equivalent | Verdict |
|---|---|---|
| `EventSource` (turns external events into K8s events) | Job-watcher in the reference Promise | Argo's `EventSource` is generic; ours is domain-specific (Jobs/CronJobs for ScheduledJob). Producers writing agents would reuse Argo's library if interop mattered. |
| `Sensor` (event-triggered dependency graph → action) | The pause-flaking-job agent's pattern | **Closer match than Knative.** Argo Sensors do "wait for these events, then trigger this action," which is what action-taking agents do. But Sensors don't have a human-in-the-loop primitive — that's still novel. |
| `Trigger` (the action a Sensor takes) | Our agent's executor logic | Conceptually overlapping. |

**Honest read.** Argo Events would have given us a much better starting point for *agent runtimes* specifically. The reason we didn't reach for it is the same as Knative: it's another platform install. But if we ever wanted "build an agent" to be a 10-minute task for a producer, an Argo-Events-style declarative Sensor format would be a strong shape. Worth considering for a future "low-code agent" tier.

---

## 3. What's actually novel

After all that subtraction, what remains:

### 3.1 The escalation gate

`AgentProposal` CRD + the gate controller's `proposed → approved → executed → expired` flow. I cannot find a standard primitive for this. Closest analogues are:

- **GitHub Actions environments with required reviewers.** Same pattern (action proposes; humans approve; action executes), but tied to a CI/CD vendor and not portable to in-cluster actions.
- **OPA/Gatekeeper admission policies with manual override.** Inverse direction (validate-and-allow vs. propose-and-approve) and there's no audit chain.
- **PagerDuty/Opsgenie incident workflows.** Adjacent but not the same; they manage the *incident*, not the *action*.

The novel contribution is **making the human-in-the-loop gate a first-class K8s resource with a CE-driven audit trail**, applicable to any in-cluster action. This deserves to be its own thing.

### 3.2 `PromiseSignals` as runtime contract

AsyncAPI does this at design time. We do it as a CRD with stability levels, payload hints, and Promise attribution — and it's queryable in the live cluster. The combination of *contract* + *runtime artefact* is what's new.

### 3.3 Agents as Kratix-native primitives

Agents are configured as Resource Requests against an agent-Promise, packaged as a `PromiseBundle` companion, gated through `AgentProposal`. The whole loop sits inside the Kratix mental model. **No other system shapes it this way** because no other system has the Promise primitive at the centre.

### 3.4 The producer ⇒ consumer recursion

A consumer of one Promise is a producer of another. An agent that emits `agent.foo.proposed` is a producer of that event-type; a downstream agent that consumes proposals to dispatch them to Slack is the consumer. The substrate doesn't care which role any participant plays. This isn't strictly *novel* — Knative's eventing model also allows this — but in Kratix it's the **default**, baked into the agent-as-Promise pattern.

---

## 4. Interop, not replacement

Kratix being its own system doesn't mean **hostile** to the ecosystem. Two interop points are worth committing to:

### 4.1 CloudEvents wire format is the contract

We emit valid CloudEvents v1.0 envelopes. A platform team that wires a Knative `Broker` to receive from our forwarder, or attaches a CE-aware sink (Splunk, Datadog, SIEM) directly to our `CloudEventSink`, gets working interop with zero changes on our side.

**This is the load-bearing interop promise.** Everything else (where the broker lives, which CRDs route events) is implementation detail; the wire is the contract.

### 4.2 Bridges, not replacements

A future `eventing/cmd/knative-bridge/` could:

- Read our `CloudEventSink` CRs and materialise Knative `Trigger` resources.
- Forward our annotated K8s Events to a Knative `Broker` instead of (or in addition to) HTTP sinks.
- Accept CEs from a Knative `Broker` as a `Source` so non-Kratix-produced events can flow into our agents.

**Bridges live alongside our substrate, not instead of it.** A platform team with Knative running for other reasons drops in the bridge; everything else stays the same. A team without Knative installed runs only our substrate; everything still works.

This is the "Knative as optional acceleration" model, not "Knative as required dependency."

---

## 5. What we'd do differently if starting today

The honest list, separate from "ship it" pragmatism:

1. **Use the CloudEvents Go SDK for envelope construction and HTTP receive.** Cheap; saves us from spec-compliance drift; doesn't change the substrate's shape. v0.2 candidate.
2. **Adopt `traceparent` (W3C Trace Context) instead of `kratix.io/ce-correlation-id`.** Free interop with OTel. One annotation key change. v0.2 candidate.
3. **Document the interop bridge upfront** — even if we don't build it, naming it as a v0.2 promise reduces "why isn't this Knative?" anxiety.
4. **Keep the gate, bundle, and signals CRDs exactly as they are.** Those are the contributions.
5. **Maybe** rename `CloudEventSink` to something less directly-overlapping with Knative `Trigger`. The current name is fine but invites comparison the architecture doesn't want. (Naming is the cheap part to defer.)

What we **wouldn't** do differently:

- **The decision to build our own substrate.** Necessary for the "Kratix is its own system" stance.
- **The escalation gate.** Solid contribution; build it the same way.
- **The agent-as-Promise pattern.** This is what makes the eventing layer Kratix-native rather than a generic CE app stitched on top.

---

## 6. When you should *not* use Kratix eventing

For completeness, the honest negative case:

- **Your platform doesn't have Promises.** The agent-as-Promise pattern, the bundle CRD, and the signals contract all assume Promises are your shape. If you're building event-driven systems without that, Knative is a strictly better starting point.
- **You need exactly-once, at-scale event delivery with replay.** Kratix's substrate is at-least-once, in-memory fan-out, no replay. Knative-with-Kafka is purpose-built for this.
- **You need 50+ event sources beyond K8s.** Knative has `KafkaSource`, `GitHubSource`, `PingSource`, `RedisStreamSource`, dozens more. We have "annotated K8s Event." Adding sources is a real engineering cost on our side.
- **You're a single-platform shop already running Knative.** The interop bridge isn't free; the simpler answer is to skip our substrate and use ours via the CE wire format only.

---

## 7. The reading list

If you're evaluating this comparison and want primary sources:

- **CloudEvents spec:** [cloudevents.io](https://cloudevents.io). The wire format we emit.
- **Knative Eventing:** [knative.dev/docs/eventing](https://knative.dev/docs/eventing). The closest off-the-shelf comparison.
- **CloudEvents Go SDK:** [github.com/cloudevents/sdk-go](https://github.com/cloudevents/sdk-go). What we'd use for v0.2 envelope handling.
- **W3C Trace Context:** [w3.org/TR/trace-context](https://www.w3.org/TR/trace-context/). The correlation-id standard we should adopt.
- **AsyncAPI:** [asyncapi.com](https://www.asyncapi.com). The design-time analogue to PromiseSignals.
- **Argo Events:** [argoproj.github.io/argo-events](https://argoproj.github.io/argo-events/). The alternative declarative-agent model.

---

## 8. Closing position

**Kratix is its own system.** The substrate we built is fit for purpose, internally coherent, and the cost of maintaining it is the price of one-install adoption. The pieces we deliberately reinvented (forwarder, wire format, emit helper) are pieces a producer never sees — they're our infrastructure, not their contract.

The pieces a producer *does* see (`Promise`, `PromiseSignals`, `PromiseBundle`, `HealthSummaryAgent`, the action-taking agent pattern, the escalation gate) are where we put the design energy. **Those are Kratix-native by intent, not by accident.**

A future where we adopt the CloudEvents Go SDK, switch correlation IDs to `traceparent`, and ship a Knative bridge is plausible and cheap. A future where we replace our substrate wholesale isn't — that would be a different project.

This document exists so the next person asking "why isn't this Knative?" gets the same answer the team gave itself.
