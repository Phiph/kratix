# Reference notifier for the escalation gate.
# Receives agent.*.proposed CloudEvents from the kratix-event-forwarder
# and relays them to an external webhook target (Slack, Teams, n8n, etc.)
# in either a generic JSON envelope or Slack Block Kit shape.
FROM golang:1.25 AS builder
WORKDIR /workspace

COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY api/ api/
COPY eventing/ eventing/
COPY internal/ internal/
COPY lib/ lib/

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/proposal-notifier ./eventing/cmd/proposal-notifier

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /out/proposal-notifier /proposal-notifier
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/proposal-notifier"]
