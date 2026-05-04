# Build context: project root (.)
FROM golang:1.22-bookworm AS builder

WORKDIR /app
COPY contract-test/go.mod ./
COPY contract-test/*.go .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o contract-test .

WORKDIR /hc
COPY healthcheck/go.mod ./
COPY healthcheck/*.go .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /healthcheck .

FROM gcr.io/distroless/static-debian12
COPY --from=builder /app/contract-test /contract-test
COPY --from=builder /healthcheck /healthcheck
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

EXPOSE 4003
ENTRYPOINT ["/contract-test"]
