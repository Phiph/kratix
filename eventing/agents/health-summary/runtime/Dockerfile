# Long-running runtime for the health-summary-agent.
# Listens on :8080, consumes CloudEvent POSTs from the kratix-event-forwarder,
# emits hourly digest CloudEvents.
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
    -o /out/agent ./eventing/agents/health-summary/runtime

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /out/agent /agent
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/agent"]
