FROM node:20-bookworm AS builder

RUN npm install -g npm@10.9.2

WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci --audit=false --fund=false
COPY tsconfig.json ./
COPY src/ ./src/
RUN npm run build

FROM gcr.io/distroless/nodejs20-debian12
WORKDIR /app
COPY --from=builder /app/dist ./dist
COPY --from=builder /app/node_modules ./node_modules
COPY --from=builder /app/package.json ./
COPY healthcheck.js /healthcheck.js

EXPOSE 4001
CMD ["dist/index.js"]
