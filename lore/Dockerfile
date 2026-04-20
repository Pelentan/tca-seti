# Build context: project root (.)
FROM golang:1.22-bookworm AS builder

WORKDIR /app
COPY lore/go.mod lore/go.sum ./
RUN go mod download
COPY lore/*.go .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o lore .

WORKDIR /hc
COPY healthcheck/go.mod ./
COPY healthcheck/*.go .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /healthcheck .

FROM scratch
COPY --from=builder /app/lore /lore
COPY --from=builder /healthcheck /healthcheck
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

EXPOSE 4110
ENTRYPOINT ["/lore"]
