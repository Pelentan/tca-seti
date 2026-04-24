defmodule FeedWrangler.EventSubscriber do
  @moduledoc """
  Subscribes to seti:events on Redis.  Fans events to registered feeds.
  OTP-supervised — restarts on crash.  Handles Redis unavailability gracefully.
  Uses FeedWrangler.Redis for both subscription and publish connections.
  """
  use GenServer
  require Logger

  def start_link(_opts) do
    GenServer.start_link(__MODULE__, [], name: __MODULE__)
  end

  def init(_opts) do
    {:ok, %{sub_pid: nil, pub_conn: nil}, {:continue, :connect}}
  end

  def handle_continue(:connect, state) do
    redis_url = System.get_env("REDIS_URL", "redis:6379")
    [host, port_str] = String.split(redis_url, ":")
    port = String.to_integer(port_str)

    with {:ok, pub_conn} <- FeedWrangler.Redis.connect(host, port),
         {:ok, sub_pid} <- FeedWrangler.Redis.subscribe(host, port, "seti:events", self()) do
      Logger.info("[feed-wrangler] Subscribed to seti:events at #{redis_url}")
      {:noreply, %{pub_conn: pub_conn, sub_pid: sub_pid}}
    else
      {:error, reason} ->
        Logger.warning("[feed-wrangler] Redis connection failed: #{inspect(reason)} — retrying in 5s")
        Process.send_after(self(), :reconnect, 5_000)
        {:noreply, state}
    end
  end

  def handle_info(:reconnect, state) do
    {:noreply, state, {:continue, :connect}}
  end

  # Message published on seti:events — decode and fan out.
  def handle_info({:redis_message, "seti:events", payload}, state) do
    case JSON.decode(payload) do
      {:ok, event} -> fan_out(event, state.pub_conn)
      {:error, _}  -> :ok
    end
    {:noreply, state}
  end

  # Server confirmed subscription — no action needed.
  def handle_info({:redis_subscribed, _channel}, state) do
    {:noreply, state}
  end

  # Subscriber process detected TCP disconnect.  Clean up publish conn and schedule reconnect.
  def handle_info({:redis_disconnected}, state) do
    Logger.warning("[feed-wrangler] Redis disconnected — reconnecting in 5s")
    if state.pub_conn, do: FeedWrangler.Redis.disconnect(state.pub_conn)
    Process.send_after(self(), :reconnect, 5_000)
    {:noreply, %{state | sub_pid: nil, pub_conn: nil}}
  end

  def handle_info(_msg, state), do: {:noreply, state}

  # ---------------------------------------------------------------------------
  # Graceful shutdown — x-tca-lifecycle
  #
  # OTP calls terminate/2 when the supervisor shuts down this GenServer.
  # Stop both Redis connections so the server sees a clean disconnect.
  # The supervisor handles SIGTERM via Application.stop/1 — no signal handling
  # needed here.
  # ---------------------------------------------------------------------------

  def terminate(_reason, %{sub_pid: sub_pid, pub_conn: pub_conn}) do
    if sub_pid,  do: FeedWrangler.Redis.stop_subscriber(sub_pid)
    if pub_conn, do: FeedWrangler.Redis.disconnect(pub_conn)
    Logger.info("[feed-wrangler] EventSubscriber terminated — Redis connections closed")
    :ok
  end

  def terminate(_reason, _state), do: :ok

  # ---------------------------------------------------------------------------

  defp fan_out(_event, nil), do: :ok
  defp fan_out(event, pub_conn) do
    feeds   = FeedWrangler.FeedRegistry.all()
    encoded = JSON.encode!(event)
    Enum.each(feeds, fn feed ->
      FeedWrangler.Redis.command(pub_conn, ["PUBLISH", "feed:#{feed.feed_id}", encoded])
      FeedWrangler.FeedRegistry.increment_delivered(feed.feed_id)
    end)
  end
end
