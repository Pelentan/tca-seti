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

    # Obtain instance cert from cert-forge before starting servers
    cert_mat = case FeedWrangler.CertForge.obtain_certs() do
      {:ok, mat} -> mat
      {:error, reason} ->
        Logger.warning("[feed-wrangler] cert-forge unavailable: #{reason} — falling back to volume certs")
        nil
    end

    cowboy_opts = build_cowboy_opts(port, cert_mat)

    children = [
      FeedWrangler.FeedRegistry,
      FeedWrangler.EventSubscriber,
      {Plug.Cowboy, cowboy_opts}
    ]

    opts = [strategy: :one_for_one, name: FeedWrangler.Supervisor]
    result = Supervisor.start_link(children, opts)

    # Self-register with Augur Canis asynchronously
    if cert_mat do
      Task.start(fn -> FeedWrangler.CertForge.self_register(cert_mat) end)
    else
      Task.start(fn -> FeedWrangler.SelfRegister.register() end)
    end

    result
  end

  defp build_cowboy_opts(port, cert_mat) do
    if cert_mat do
      Logger.info("[feed-wrangler] mTLS certs from cert-forge — starting HTTPS")
      # Write temp files for cowboy — cleaned up after process starts
      cert_path = "/tmp/fw-srv.crt"
      key_path  = "/tmp/fw-srv.key"
      ca_path   = "/tmp/fw-srv-ca.crt"
      File.write!(cert_path, cert_mat.instance_cert)
      File.write!(key_path,  cert_mat.instance_key)
      File.write!(ca_path,   cert_mat.ca_cert)
      [
        scheme: :https,
        plug: FeedWrangler.Router,
        options: [
          port: port,
          certfile: cert_path,
          keyfile: key_path,
          cacertfile: ca_path,
          verify: :verify_peer,
          fail_if_no_peer_cert: true
        ]
      ]
    else
      Logger.warning("[feed-wrangler] No cert-forge material — starting HTTP (dev fallback)")
      [
        scheme: :http,
        plug: FeedWrangler.Router,
        options: [port: port]
      ]
    end
  end
end
