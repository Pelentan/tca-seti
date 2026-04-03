defmodule FeedWrangler.FeedRegistry do
  @moduledoc """
  Owns the in-memory feed registry.
  Each feed has: feed_id, wrangler_id, clearance_level, filters, created_at.
  """
  use Agent

  def start_link(_opts) do
    Agent.start_link(fn -> %{} end, name: __MODULE__)
  end

  def create(wrangler_id, clearance_level, filters \\ []) do
    feed_id = "feed-#{:erlang.unique_integer([:positive])}"
    feed = %{
      feed_id: feed_id,
      wrangler_id: wrangler_id,
      clearance_level: clearance_level,
      filters: filters,
      events_delivered: 0,
      created_at: DateTime.utc_now() |> DateTime.to_iso8601()
    }
    Agent.update(__MODULE__, &Map.put(&1, feed_id, feed))
    feed
  end

  def get(feed_id), do: Agent.get(__MODULE__, &Map.get(&1, feed_id))

  def delete(feed_id) do
    Agent.update(__MODULE__, &Map.delete(&1, feed_id))
  end

  def all, do: Agent.get(__MODULE__, &Map.values(&1))

  def increment_delivered(feed_id) do
    Agent.update(__MODULE__, fn feeds ->
      Map.update(feeds, feed_id, %{}, &Map.update(&1, :events_delivered, 0, fn n -> n + 1 end))
    end)
  end
end
