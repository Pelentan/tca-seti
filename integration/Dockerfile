# Build context: project root (.)
FROM golang:1.22-bookworm AS builder
WORKDIR /app
COPY integration/go.mod integration/go.sum ./
RUN go mod download
COPY integration/*.go .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o integration .
WORKDIR /hc
COPY healthcheck/go.mod healthcheck/go.sum ./
RUN go mod download
COPY healthcheck/main.go .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /healthcheck .
FROM gcr.io/distroless/static-debian12
COPY --from=builder /app/integration /integration
COPY --from=builder /healthcheck /healthcheck
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
EXPOSE 4013
ENTRYPOINT ["/integration"]
