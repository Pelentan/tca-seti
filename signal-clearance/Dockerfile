FROM node:20-bookworm AS builder

RUN npm install -g npm@10.9.2

WORKDIR /app
COPY signal-clearance/package.json signal-clearance/package-lock.json ./
RUN npm ci --audit=false --fund=false
COPY signal-clearance/tsconfig.json ./
COPY signal-clearance/src/ ./src/
RUN npm run build

FROM gcr.io/distroless/nodejs20-debian12
WORKDIR /app
COPY --from=builder /app/dist ./dist
COPY --from=builder /app/node_modules ./node_modules
COPY --from=builder /app/package.json ./
COPY signal-clearance/healthcheck.js /healthcheck.js

EXPOSE 4001
CMD ["dist/index.js"]
