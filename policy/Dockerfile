# Build context: project root (.)
FROM golang:1.22-bookworm AS builder

WORKDIR /app
COPY policy/go.mod ./
COPY policy/*.go .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o policy .

WORKDIR /hc
COPY healthcheck/go.mod ./
COPY healthcheck/*.go .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /healthcheck .

FROM gcr.io/distroless/static-debian12
COPY --from=builder /app/policy /policy
COPY --from=builder /healthcheck /healthcheck
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

EXPOSE 4002
ENTRYPOINT ["/policy"]
