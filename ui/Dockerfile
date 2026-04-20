# Stage 1: Build React SPA
# Build context: project root (.)
FROM node:20-bookworm AS ui-builder

RUN npm install -g npm@10.9.2

WORKDIR /app
COPY ui/package.json ui/package-lock.json ./
RUN npm ci --audit=false --fund=false
COPY ui/tsconfig.json ui/tsconfig.node.json ui/vite.config.ts ui/index.html ./
COPY ui/src/ ./src/
ARG VITE_GATEWAY_URL
ARG VITE_AUTH_MODE
ENV VITE_GATEWAY_URL=$VITE_GATEWAY_URL
ENV VITE_AUTH_MODE=$VITE_AUTH_MODE
RUN npm run build

# Stage 2: Build Go static file server + healthcheck
FROM golang:1.22-bookworm AS server-builder

WORKDIR /app
COPY ui/server/*.go .
RUN go mod init seti-ui-server && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o ui-server .

# Build healthcheck binary inline
WORKDIR /hc
COPY healthcheck/go.mod ./
COPY healthcheck/*.go .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /healthcheck .

# Stage 3: Runtime
FROM gcr.io/distroless/static-debian12

COPY --from=server-builder /app/ui-server /ui-server
COPY --from=server-builder /healthcheck /healthcheck
COPY --from=ui-builder /app/dist /app/dist

EXPOSE 4020
ENTRYPOINT ["/ui-server"]
