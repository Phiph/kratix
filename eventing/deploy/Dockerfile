# Build the forwarder binary
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
    -o /out/forwarder ./eventing/cmd/forwarder

# Runtime image
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /out/forwarder /forwarder
USER 65532:65532
ENTRYPOINT ["/forwarder"]
