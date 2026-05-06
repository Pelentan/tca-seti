# Build context: project root (.)
FROM golang:1.22-bookworm AS builder

WORKDIR /app
COPY connie-agent/go.mod ./
RUN go mod download
COPY connie-agent/*.go .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o connie-agent .

WORKDIR /hc
COPY healthcheck/go.mod ./
COPY healthcheck/*.go .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /healthcheck .

FROM gcr.io/distroless/static-debian12

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /app/connie-agent /connie-agent
COPY --from=builder /healthcheck /healthcheck

EXPOSE 4014

ENTRYPOINT ["/connie-agent"]
