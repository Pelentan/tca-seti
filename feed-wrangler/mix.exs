defmodule FeedWrangler.MixProject do
  use Mix.Project

  def project do
    [
      app: :feed_wrangler,
      version: "0.1.0",
      elixir: "~> 1.16",
      start_permanent: Mix.env() == :prod,
      deps: deps()
    ]
  end

  def application do
    [
      extra_applications: [:logger, :crypto, :ssl],
      mod: {FeedWrangler.Application, []}
    ]
  end

  defp deps do
    [
      {:redix, "~> 1.3"},
      {:jason, "~> 1.4"},
      {:plug_cowboy, "~> 2.7"}
    ]
  end
end
