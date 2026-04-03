# cert-forge — TCA certificate authority init tool.
# Build context: project root (.)
#
# Multi-stage: build the static binary in golang:bookworm, copy to scratch.
# The runtime stage is scratch — no shell, no OS, no attack surface.
# Supply chain: Go standard library only. No external dependencies.
# go.sum is intentionally absent — there are no module dependencies to verify.

FROM golang:1.22-bookworm AS builder

WORKDIR /forge
COPY cert-forge/go.mod ./
COPY cert-forge/main.go ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -o cert-forge .

FROM scratch
COPY --from=builder /forge/cert-forge /cert-forge
COPY cert-forge/forge.json /forge.json

ENTRYPOINT ["/cert-forge"]
