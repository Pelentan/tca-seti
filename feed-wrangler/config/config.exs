import Config

config :logger, level: :info

config :feed_wrangler,
  start_time: System.monotonic_time(:second)
