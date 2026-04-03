# Build context: project root (.)
FROM golang:1.22-bookworm AS builder

WORKDIR /app
COPY augur-canis/go.mod augur-canis/go.sum ./
RUN go mod download
COPY augur-canis/*.go .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o augur-canis .

WORKDIR /hc
COPY healthcheck/go.mod healthcheck/go.sum ./
RUN go mod download
COPY healthcheck/main.go .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /healthcheck .

FROM scratch
COPY --from=builder /app/augur-canis /augur-canis
COPY --from=builder /healthcheck /healthcheck
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

EXPOSE 4010
ENTRYPOINT ["/augur-canis"]
