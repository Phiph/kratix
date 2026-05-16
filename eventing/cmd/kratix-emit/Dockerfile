# Build the kratix-emit CLI as a static binary. The image is intended to be
# used as a FROM stage in user pipeline base images:
#
#     FROM ghcr.io/syntasso/kratix-emit:v0.1 AS emit
#     FROM debian:stable-slim
#     COPY --from=emit /kratix-emit /usr/local/bin/kratix-emit
#
# The runtime image is distroless so the binary can also be exec'd directly
# from a Job pod that uses this image as-is (handy for testing).
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
    -o /out/kratix-emit ./eventing/cmd/kratix-emit

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /out/kratix-emit /kratix-emit
USER 65532:65532
ENTRYPOINT ["/kratix-emit"]
