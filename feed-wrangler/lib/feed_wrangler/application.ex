defmodule FeedWrangler.Application do
  @moduledoc """
  Feed Wr4ngler — scoped signal delivery.
  Provisions per-Wr4ngler feed channels scoped by clearance level.
  OTP supervision: each feed is a supervised process. Crashed feeds
  restart in isolation.
  """

  use Application
  require Logger

  def start(_type, _args) do
    port = System.get_env("PORT", "4007") |> String.to_integer()
    redis_url = System.get_env("REDIS_URL", "redis:6379")

    Logger.info("[feed-wrangler] Starting on port #{port}")
    Logger.info("[feed-wrangler] Redis: #{redis_url}")

    cowboy_opts = build_cowboy_opts(port)

    children = [
      FeedWrangler.FeedRegistry,
      FeedWrangler.EventSubscriber,
      {Plug.Cowboy, cowboy_opts}
    ]

    opts = [strategy: :one_for_one, name: FeedWrangler.Supervisor]
    Supervisor.start_link(children, opts)
  end

  defp build_cowboy_opts(port) do
    cert = "/certs/feed-wrangler.crt"
    key  = "/certs/feed-wrangler.key"
    ca   = "/certs/ca.crt"

    if File.exists?(cert) and File.exists?(key) and File.exists?(ca) do
      Logger.info("[feed-wrangler] mTLS certs found — starting HTTPS")
      [
        scheme: :https,
        plug: FeedWrangler.Router,
        options: [
          port: port,
          certfile: cert,
          keyfile: key,
          cacertfile: ca,
          verify: :verify_peer,
          fail_if_no_peer_cert: true
        ]
      ]
    else
      Logger.warning("[feed-wrangler] Certs not found — starting HTTP (dev fallback)")
      [
        scheme: :http,
        plug: FeedWrangler.Router,
        options: [port: port]
      ]
    end
  end
end
