defmodule FeedWrangler.Redis do
  @moduledoc """
  TCA RESP2 client for Elixir.  No external dependencies — :gen_tcp only.

  Two connection modes:

    Command connections — synchronous request/response.
      connect/2   → {:ok, socket} | {:error, reason}
      command/2   → {:ok, value}  | {:error, reason}
      disconnect/1

    Subscribe connections — event-driven, dedicated process.
      subscribe/4      → {:ok, sub_pid} | {:error, reason}
      stop_subscriber/1

  Subscribe message protocol (sent to notify_pid):
    {:redis_message,    channel, payload}   — message received on subscribed channel
    {:redis_subscribed, channel}            — subscription confirmed by server
    {:redis_disconnected}                   — TCP connection lost

  TCA immutability discipline: function signatures are frozen on first
  production use.  Add functions; never remove, rename, or change signatures.
  """

  require Logger

  # ---------------------------------------------------------------------------
  # Command connection — synchronous RESP2
  # ---------------------------------------------------------------------------

  @doc """
  Open a command connection to Redis.  Returns {:ok, socket} | {:error, reason}.
  The socket is used for synchronous request/response commands (PUBLISH etc.).
  Not safe for concurrent use from multiple processes — each caller needs its
  own connection.
  """
  @spec connect(String.t(), non_neg_integer()) :: {:ok, :gen_tcp.socket()} | {:error, term()}
  def connect(host, port) do
    opts = [:binary, active: false, packet: :raw, send_timeout: 5_000]
    :gen_tcp.connect(String.to_charlist(host), port, opts, 10_000)
  end

  @doc """
  Send a RESP2 command and return the parsed response.
  args is a list of strings, e.g. ["PUBLISH", "chan", "msg"].
  Returns {:ok, value} | {:error, reason}.
  """
  @spec command(:gen_tcp.socket(), [String.t()]) :: {:ok, term()} | {:error, term()}
  def command(socket, args) do
    frame = encode(args)
    with :ok <- :gen_tcp.send(socket, frame) do
      recv_sync(socket, "")
    end
  end

  @doc "Close a command connection."
  @spec disconnect(:gen_tcp.socket()) :: :ok
  def disconnect(socket), do: :gen_tcp.close(socket)

  # ---------------------------------------------------------------------------
  # Subscribe connection — spawns a dedicated reader process
  # ---------------------------------------------------------------------------

  @doc """
  Subscribe to a channel.  Connects to Redis with active: false, sends SUBSCRIBE,
  then spawns a dedicated process that takes socket ownership, enables active mode,
  and forwards push messages to notify_pid.

  Returns {:ok, sub_pid} | {:error, reason}.

  This function is safe to call from inside a GenServer handler because the
  subscribe command is sent synchronously in the caller and the receive loop
  runs entirely in the spawned process.  No receive/after block in the caller.
  """
  @spec subscribe(String.t(), non_neg_integer(), String.t(), pid()) ::
          {:ok, pid()} | {:error, term()}
  def subscribe(host, port, channel, notify_pid) do
    opts = [:binary, active: false, packet: :raw, send_timeout: 5_000]
    case :gen_tcp.connect(String.to_charlist(host), port, opts, 10_000) do
      {:ok, sock} ->
        case :gen_tcp.send(sock, encode(["SUBSCRIBE", channel])) do
          :ok ->
            # Spawn loop process and hand socket ownership to it.
            # Socket stays active: false until the subprocess enables active: true,
            # so no tcp messages are delivered to the current process after this point.
            pid = spawn_link(fn -> sub_take(sock, channel, notify_pid) end)
            case :gen_tcp.controlling_process(sock, pid) do
              :ok ->
                {:ok, pid}
              {:error, reason} ->
                Process.exit(pid, :kill)
                :gen_tcp.close(sock)
                {:error, reason}
            end
          {:error, reason} ->
            :gen_tcp.close(sock)
            {:error, reason}
        end
      {:error, reason} ->
        {:error, reason}
    end
  end

  @doc """
  Stop a subscriber process cleanly.  Sends :stop — the process closes the
  socket and exits.  Returns :ok immediately (fire-and-forget).
  """
  @spec stop_subscriber(pid()) :: :ok
  def stop_subscriber(pid) when is_pid(pid) do
    send(pid, :stop)
    :ok
  end

  # ---------------------------------------------------------------------------
  # Subscriber process internals
  # ---------------------------------------------------------------------------

  # Entry point for the subscriber process.  Takes socket ownership then loops.
  defp sub_take(sock, channel, notify_pid) do
    # We now own the socket.  Enable active mode — tcp messages flow to us.
    :inet.setopts(sock, [active: true])
    sub_loop(sock, channel, notify_pid, "")
  end

  defp sub_loop(sock, channel, notify_pid, buf) do
    receive do
      :stop ->
        :gen_tcp.close(sock)

      {:tcp, ^sock, data} ->
        {values, remaining} = parse_all(buf <> data, [])
        Enum.each(values, &dispatch_push(&1, notify_pid))
        sub_loop(sock, channel, notify_pid, remaining)

      {:tcp_closed, ^sock} ->
        Logger.warning("[redis] Subscriber socket closed (channel: #{channel})")
        send(notify_pid, {:redis_disconnected})

      {:tcp_error, ^sock, reason} ->
        Logger.warning("[redis] Subscriber socket error: #{inspect(reason)} (channel: #{channel})")
        :gen_tcp.close(sock)
        send(notify_pid, {:redis_disconnected})
    end
  end

  defp dispatch_push(["message", channel, payload], pid),
    do: send(pid, {:redis_message, channel, payload})
  defp dispatch_push(["subscribe", channel, _count], pid),
    do: send(pid, {:redis_subscribed, channel})
  defp dispatch_push(_other, _pid), do: :ok

  # ---------------------------------------------------------------------------
  # RESP2 encoder
  # ---------------------------------------------------------------------------

  defp encode(args) do
    header = "*#{length(args)}\r\n"
    parts =
      Enum.map(args, fn a ->
        s = to_string(a)
        "$#{byte_size(s)}\r\n#{s}\r\n"
      end)
    IO.iodata_to_binary([header | parts])
  end

  # ---------------------------------------------------------------------------
  # RESP2 incremental parser
  # Used by both subscriber (active mode) and command recv (blocking mode).
  #
  # try_parse/1 returns:
  #   {:ok, value, rest_binary}  — one complete value parsed
  #   :incomplete                — not enough data yet, accumulate more
  # ---------------------------------------------------------------------------

  defp parse_all(buf, acc) do
    case try_parse(buf) do
      {:ok, val, rest} -> parse_all(rest, [val | acc])
      :incomplete -> {Enum.reverse(acc), buf}
    end
  end

  defp try_parse(buf) do
    # Find the first CRLF to get the type prefix line.
    case :binary.split(buf, "\r\n") do
      [_incomplete] -> :incomplete
      [line, rest] -> parse_type(line, rest)
    end
  end

  # Simple string
  defp parse_type("+" <> val, rest), do: {:ok, val, rest}

  # Error (returned as {:redis_error, msg} so callers can distinguish)
  defp parse_type("-" <> msg, rest), do: {:ok, {:redis_error, msg}, rest}

  # Integer
  defp parse_type(":" <> num, rest), do: {:ok, String.to_integer(num), rest}

  # Bulk string  ($-1 = null, $N = N bytes of data starting immediately in rest)
  defp parse_type("$" <> n, rest) do
    len = String.to_integer(n)
    if len < 0 do
      {:ok, nil, rest}
    else
      # rest starts immediately after the $N\r\n line.
      # We need exactly len bytes of data followed by \r\n (len + 2 total).
      need = len + 2
      if byte_size(rest) >= need do
        {:ok, binary_part(rest, 0, len), binary_part(rest, need, byte_size(rest) - need)}
      else
        :incomplete
      end
    end
  end

  # Array  (*-1 = null array, *N = N elements)
  defp parse_type("*" <> n, rest) do
    count = String.to_integer(n)
    if count < 0 do
      {:ok, nil, rest}
    else
      parse_array(count, rest, [])
    end
  end

  defp parse_type(_unknown, _rest), do: :incomplete

  defp parse_array(0, buf, acc), do: {:ok, Enum.reverse(acc), buf}
  defp parse_array(n, buf, acc) do
    case try_parse(buf) do
      {:ok, val, rest} -> parse_array(n - 1, rest, [val | acc])
      :incomplete -> :incomplete
    end
  end

  # ---------------------------------------------------------------------------
  # Synchronous recv for command connections (active: false)
  # Loops calling :gen_tcp.recv until a complete RESP2 value is assembled.
  # ---------------------------------------------------------------------------

  defp recv_sync(socket, buf) do
    case try_parse(buf) do
      {:ok, {:redis_error, msg}, _rest} ->
        {:error, msg}
      {:ok, val, _rest} ->
        {:ok, val}
      :incomplete ->
        case :gen_tcp.recv(socket, 0, 5_000) do
          {:ok, data} -> recv_sync(socket, buf <> data)
          {:error, reason} -> {:error, reason}
        end
    end
  end
end
