# Build context: feed-wrangler/ (subdirectory)
FROM elixir:1.18-slim AS builder

RUN apt-get update && apt-get install -y --no-install-recommends \
    openssl ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
ENV MIX_ENV=prod

COPY mix.exs mix.lock* ./
RUN mix local.hex --force && mix local.rebar --force
RUN mix deps.get --only prod
RUN mix deps.compile

COPY lib/ lib/
COPY config/ config/
RUN mix release

FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    libssl3 libncurses6 libstdc++6 ca-certificates nodejs curl openssl \
    && rm -rf /var/lib/apt/lists/* \
    && ln -sf /usr/bin/nodejs /usr/local/bin/node

WORKDIR /app
COPY --from=builder /app/_build/prod/rel/feed_wrangler ./
# Build context is feed-wrangler/ so path is relative to that directory
COPY healthcheck.js /healthcheck.js

ENV PORT=4007
EXPOSE 4007
CMD ["./bin/feed_wrangler", "start"]
