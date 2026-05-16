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
    -o /out/job-watcher ./samples/reference-promise/job-watcher

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /out/job-watcher /job-watcher
USER 65532:65532
ENTRYPOINT ["/job-watcher"]
