defmodule FeedWrangler.Router do
  @moduledoc "HTTP router for Feed Wr4ngler — mTLS enforced by Cowboy."

  use Plug.Router
  require Logger

  plug Plug.Parsers,
    parsers: [:json],
    json_decoder: JSON

  plug :match
  plug :dispatch

  # Health
  get "/health" do
    feeds = FeedWrangler.FeedRegistry.all()
    send_json(conn, 200, %{
      status: "healthy",
      feeds_active: length(feeds),
      uptime_seconds: :erlang.monotonic_time(:second) - Application.get_env(:feed_wrangler, :start_time, 0)
    })
  end

  # Create feed
  post "/feeds" do
    wrangler_id = conn.body_params["wrangler_id"]

    if is_nil(wrangler_id) or wrangler_id == "" do
      send_json(conn, 400, %{code: "INVALID_REQUEST", message: "wrangler_id is required"})
    else
      clearance_level = conn.body_params["clearance_level"] || "connie-wr4ngler"
      filters = conn.body_params["filters"] || []

      feed = FeedWrangler.FeedRegistry.create(wrangler_id, clearance_level, filters)
      Logger.info("[feed-wrangler] Feed created: #{feed.feed_id} for #{wrangler_id}")
      send_json(conn, 201, feed)
    end
  end

  # List feeds
  get "/feeds" do
    feeds = FeedWrangler.FeedRegistry.all()
    send_json(conn, 200, %{feeds: feeds, total: length(feeds)})
  end

  # Get feed
  get "/feeds/:feed_id" do
    case FeedWrangler.FeedRegistry.get(feed_id) do
      nil -> send_json(conn, 404, %{code: "NOT_FOUND", message: "feed #{feed_id} not found"})
      feed -> send_json(conn, 200, feed)
    end
  end

  # Delete feed
  delete "/feeds/:feed_id" do
    case FeedWrangler.FeedRegistry.get(feed_id) do
      nil ->
        send_json(conn, 404, %{code: "NOT_FOUND", message: "feed #{feed_id} not found"})
      _feed ->
        FeedWrangler.FeedRegistry.delete(feed_id)
        Logger.info("[feed-wrangler] Feed deleted: #{feed_id}")
        send_json(conn, 200, %{status: "ok", message: "feed #{feed_id} deleted"})
    end
  end

  # Publish event to feed (used by Signal Aggregator)
  post "/feeds/:feed_id/publish" do
    case FeedWrangler.FeedRegistry.get(feed_id) do
      nil ->
        send_json(conn, 404, %{code: "NOT_FOUND", message: "feed #{feed_id} not found"})
      _feed ->
        FeedWrangler.FeedRegistry.increment_delivered(feed_id)
        send_json(conn, 202, %{status: "accepted"})
    end
  end

  match _ do
    send_json(conn, 404, %{code: "NOT_FOUND", message: "#{conn.method} #{conn.request_path} not found"})
  end

  defp send_json(conn, status, body) do
    conn
    |> put_resp_content_type("application/json")
    |> send_resp(status, JSON.encode!(body))
  end
end
