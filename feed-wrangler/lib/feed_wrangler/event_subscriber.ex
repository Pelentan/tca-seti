defmodule FeedWrangler.EventSubscriber do
  @moduledoc """
  Subscribes to seti:events on Redis. Fans events to registered feeds.
  OTP-supervised — restarts on crash. Handles Redis unavailability gracefully.
  Uses Redix.PubSub for subscriptions, plain Redix for publishing.
  """
  use GenServer
  require Logger

  def start_link(_opts) do
    GenServer.start_link(__MODULE__, [], name: __MODULE__)
  end

  def init(_opts) do
    {:ok, %{pub_sub: nil, pub_conn: nil}, {:continue, :connect}}
  end

  def handle_continue(:connect, state) do
    redis_url = System.get_env("REDIS_URL", "redis:6379")
    [host, port_str] = String.split(redis_url, ":")
    port = String.to_integer(port_str)

    with {:ok, pub_sub} <- Redix.PubSub.start_link(host: host, port: port),
         {:ok, pub_conn} <- Redix.start_link(host: host, port: port),
         {:ok, _ref} <- Redix.PubSub.subscribe(pub_sub, "seti:events", self()) do
      Logger.info("[feed-wrangler] Subscribed to seti:events at #{redis_url}")
      {:noreply, %{pub_sub: pub_sub, pub_conn: pub_conn}}
    else
      {:error, reason} ->
        Logger.warning("[feed-wrangler] Redis connection failed: #{inspect(reason)} — retrying in 5s")
        Process.send_after(self(), :reconnect, 5000)
        {:noreply, state}
    end
  end

  def handle_info(:reconnect, state) do
    {:noreply, state, {:continue, :connect}}
  end

  def handle_info({:redix_pubsub, _pub_sub, _ref, :message, %{channel: "seti:events", payload: payload}}, state) do
    case Jason.decode(payload) do
      {:ok, event} -> fan_out(event, state.pub_conn)
      {:error, _}  -> :ok
    end
    {:noreply, state}
  end

  def handle_info({:redix_pubsub, _pub_sub, _ref, :subscribed, _}, state) do
    {:noreply, state}
  end

  def handle_info({:redix_pubsub, _pub_sub, _ref, :disconnected, _}, state) do
    Logger.warning("[feed-wrangler] Redis pub/sub disconnected — reconnecting in 5s")
    Process.send_after(self(), :reconnect, 5000)
    {:noreply, %{state | pub_sub: nil, pub_conn: nil}}
  end

  def handle_info(_msg, state), do: {:noreply, state}

  # ---------------------------------------------------------------------------
  # Graceful shutdown — x-tca-lifecycle
  #
  # OTP calls terminate/2 when the supervisor shuts down this GenServer.
  # Explicitly stop the Redix connections so the Redis server sees a clean
  # disconnect rather than a timeout. The supervisor handles SIGTERM via
  # Application.stop/1 — no signal handling needed here.
  # ---------------------------------------------------------------------------

  def terminate(_reason, %{pub_sub: pub_sub, pub_conn: pub_conn}) do
    if pub_sub,  do: Redix.PubSub.stop(pub_sub)
    if pub_conn, do: Redix.stop(pub_conn)
    Logger.info("[feed-wrangler] EventSubscriber terminated — Redis connections closed")
    :ok
  end

  def terminate(_reason, _state), do: :ok

  defp fan_out(_event, nil), do: :ok
  defp fan_out(event, pub_conn) do
    feeds = FeedWrangler.FeedRegistry.all()
    encoded = Jason.encode!(event)
    Enum.each(feeds, fn feed ->
      Redix.command(pub_conn, ["PUBLISH", "feed:#{feed.feed_id}", encoded])
      FeedWrangler.FeedRegistry.increment_delivered(feed.feed_id)
    end)
  end
end
