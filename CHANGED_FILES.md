# Phase S5b — feed-wrangler jason elimination

## Summary
Replaced jason hex package with Elixir 1.18 stdlib JSON module.
Elixir 1.18 ships with OTP 27 which includes the :json Erlang stdlib module
and the JSON Elixir wrapper. Interface is identical to Jason — direct
substitution with no logic changes.

## Changed Files

### Modified
- `feed-wrangler/Dockerfile`
  Builder stage: elixir:1.16-slim → elixir:1.18-slim (OTP 27, stdlib JSON)
  Runtime stage unchanged (Debian bookworm-slim, BEAM bundled in release)

- `feed-wrangler/mix.exs`
  Removed: {:jason, "~> 1.4"}
  Remaining dep: plug_cowboy only

- `feed-wrangler/lib/feed_wrangler/cert_forge.ex`
  Jason.decode → JSON.decode, Jason.encode! → JSON.encode! (6 sites)

- `feed-wrangler/lib/feed_wrangler/event_subscriber.ex`
  Jason.decode → JSON.decode, Jason.encode! → JSON.encode! (2 sites)

- `feed-wrangler/lib/feed_wrangler/router.ex`
  Jason.encode! → JSON.encode!, json_decoder: Jason → json_decoder: JSON

- `feed-wrangler/lib/feed_wrangler/self_register.ex`
  Jason.decode → JSON.decode, Jason.encode! → JSON.encode! (2 sites)

## Remaining Third-Party Dependencies (updated)

| Job          | Dependency  | Action                          |
|--------------|-------------|---------------------------------|
| lore         | lib/pq      | Stays — no Go stdlib PG driver  |
| feed-wrangler| plug_cowboy | Stays — no stdlib HTTP server   |
| ui           | react, vite | Build-time only, never in prod  |

jason is eliminated. feed-wrangler now has exactly one hex dependency.

## Rebuild Command
```
docker build \
  -f feed-wrangler/Dockerfile \
  -t localhost:5000/seti-feed-wrangler:dev \
  feed-wrangler/ \
&& docker push localhost:5000/seti-feed-wrangler:dev \
&& kubectl rollout restart deployment/feed-wrangler -n seti
```
