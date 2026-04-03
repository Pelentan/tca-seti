# Build context: project root (.)
FROM golang:1.22-bookworm AS builder

WORKDIR /app
COPY seti-observability/go.mod seti-observability/go.sum ./
RUN go mod download
COPY seti-observability/*.go .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o seti-observability .

WORKDIR /hc
COPY healthcheck/go.mod healthcheck/go.sum ./
RUN go mod download
COPY healthcheck/main.go .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /healthcheck .

FROM scratch
COPY --from=builder /app/seti-observability /seti-observability
COPY --from=builder /healthcheck /healthcheck
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

EXPOSE 4011
ENTRYPOINT ["/seti-observability"]
