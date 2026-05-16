# Kratix Eventing

Adjacent subsystem that turns Kratix's existing Kubernetes `Event` emissions into a stream of CloudEvents v1.0 envelopes, deliverable to user-defined HTTP sinks. Foundation for downstream bounded agents — see [`docs/vision-cloudevents-signal-layer.md`](../docs/vision-cloudevents-signal-layer.md) for the full vision.

**Status:** v0.1 / MVP. The wire format will change before v1alpha1. Pin everything.

## What's here

```
eventing/
  WIRE-FORMAT.md              The producer/consumer contract
  api/v1alpha1/               CloudEventSink CRD Go types
  pkg/schema/                 Annotation keys, type constants, helpers
  cmd/forwarder/              The forwarder binary
  config/crd/                 Generated CRD manifest
  deploy/                     Dockerfile, Deployment, RBAC
```

## How it works

1. Kratix controllers emit standard K8s Events with `kratix.io/ce-*` annotations.
2. The forwarder watches Events cluster-wide.
3. For each Event carrying the required annotations, it translates to a CloudEvent (structured-mode JSON).
4. It POSTs the CE to every `CloudEventSink` whose `typeFilter` matches.

The K8s Event remains canonical. CloudEvents are derived; if they disagree, the Event wins.

## Try it locally

```sh
# 1. Install the CRD
kubectl apply -f eventing/config/crd/cloudeventsink.yaml

# 2. Create a sink (e.g. webhook.site for a quick echo)
cat <<EOF | kubectl apply -f -
apiVersion: eventing.kratix.io/v1alpha1
kind: CloudEventSink
metadata:
  name: debug-echo
spec:
  url: https://webhook.site/<your-uuid>
  typeFilter:
    - "kratix.promise.*"
EOF

# 3. Run the forwarder out-of-cluster
go run ./eventing/cmd/forwarder --install-id=dev --log-level=debug
```

## Boundaries

- **Kratix does not import this package.** Producers emit K8s Events with documented annotation keys; the forwarder consumes them. The contract is the wire format in `WIRE-FORMAT.md`.
- **Agents do not import Kratix.** They consume CloudEvents and may copy constants from `pkg/schema/` if they prefer compile-time checks over the raw strings.
- **Status writes from the forwarder back to `CloudEventSink` are not implemented in v0.1.** The `Conditions` field is present in the CRD but unmanaged.
