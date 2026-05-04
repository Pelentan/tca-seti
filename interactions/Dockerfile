# Build context: project root (.)
FROM golang:1.22-bookworm AS builder
WORKDIR /app
COPY interactions/go.mod ./
RUN go mod download
COPY interactions/*.go .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o interactions .
WORKDIR /hc
COPY healthcheck/go.mod ./
COPY healthcheck/*.go .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /healthcheck .
FROM gcr.io/distroless/static-debian12
COPY --from=builder /app/interactions /interactions
COPY --from=builder /healthcheck /healthcheck
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
EXPOSE 4009
ENTRYPOINT ["/interactions"]
