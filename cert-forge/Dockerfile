# cert-forge — TCA PKI abstraction layer.
# Build context: project root (.)
#
# Runs as a persistent service — not an init container.
# Generates the constellation CA, issues instance certs on demand,
# holds all private key material, performs all signing operations.
#
# Runtime: scratch — no shell, no OS, one binary.
# Supply chain: Go standard library only. No external dependencies.

FROM golang:1.22-bookworm AS builder

WORKDIR /forge
COPY cert-forge/go.mod ./
COPY cert-forge/*.go ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -o cert-forge .

# Healthcheck binary
WORKDIR /hc
COPY healthcheck/go.mod healthcheck/go.sum ./
RUN go mod download
COPY healthcheck/main.go .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /healthcheck .

FROM scratch
COPY --from=builder /forge/cert-forge /cert-forge
COPY --from=builder /healthcheck /healthcheck
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY cert-forge/forge.json /forge.json

EXPOSE 4014
ENTRYPOINT ["/cert-forge"]
