# Build context: project root (.)
FROM golang:1.22-bookworm AS builder

WORKDIR /app
COPY gateway/go.mod ./
COPY gateway/*.go .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o gateway .

WORKDIR /hc
COPY healthcheck/go.mod ./
COPY healthcheck/*.go .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /healthcheck .

FROM gcr.io/distroless/static-debian12
COPY --from=builder /app/gateway /gateway
COPY --from=builder /healthcheck /healthcheck
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

EXPOSE 4000
ENTRYPOINT ["/gateway"]
