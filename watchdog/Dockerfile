# Build context: project root (.)
FROM golang:1.22-bookworm AS builder

WORKDIR /app
COPY watchdog/go.mod watchdog/go.sum ./
RUN go mod download
COPY watchdog/main.go .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o watchdog .

WORKDIR /hc
COPY healthcheck/go.mod healthcheck/go.sum ./
RUN go mod download
COPY healthcheck/main.go .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /healthcheck .

FROM scratch
COPY --from=builder /app/watchdog /watchdog
COPY --from=builder /healthcheck /healthcheck
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

EXPOSE 4010
ENTRYPOINT ["/watchdog"]
