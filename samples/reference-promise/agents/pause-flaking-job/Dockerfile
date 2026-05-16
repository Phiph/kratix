FROM golang:1.25 AS builder
WORKDIR /workspace

COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY api/ api/
COPY eventing/ eventing/
COPY internal/ internal/
COPY lib/ lib/
COPY samples/ samples/

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/pause-flaking-job ./samples/reference-promise/agents/pause-flaking-job

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /out/pause-flaking-job /pause-flaking-job
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/pause-flaking-job"]
