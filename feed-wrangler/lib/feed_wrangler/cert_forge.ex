defmodule FeedWrangler.CertForge do
  @moduledoc """
  cert-forge client for feed-wrangler.

  Obtains instance cert from cert-forge on startup.
  Private key held in memory — never written to the shared volume.
  Uses curl for all HTTP calls — avoids adding HTTP client dependencies.
  """
  require Logger

  @service_name "feed-wrangler"
  @endpoint     "https://feed-wrangler:4007"
  @enrollment_cert System.get_env("ENROLLMENT_CERT", "/certs/enrollment.crt")
  @enrollment_key  System.get_env("ENROLLMENT_KEY",  "/certs/enrollment.key")

  defstruct [:ca_cert, :instance_cert, :instance_key, :fingerprint, :instance_cn, :instance_id]

  @type t :: %__MODULE__{
    ca_cert:      binary(),
    instance_cert: binary(),
    instance_key:  binary(),
    fingerprint:  String.t(),
    instance_cn:  String.t(),
    instance_id:  String.t(),
  }

  @doc "Obtain all cert material — call once on startup, returns {:ok, mat} or {:error, reason}"
  def obtain_certs do
    forge_url    = System.get_env("CERT_FORGE_URL", "https://cert-forge:4014")
    public_port  = System.get_env("PUBLIC_PORT", "4016")
    enroll_port  = System.get_env("ENROLLMENT_PORT", "4015")
    instance_id  = System.get_env("HOSTNAME", "local")

    public_url  = derive_url(forge_url, public_port) |> String.replace("https://", "http://")
    enroll_url  = derive_url(forge_url, enroll_port)

    with {:ok, ca_cert}    <- fetch_ca_cert(public_url),
         {:ok, cert, key, fp, cn} <- request_instance_cert(enroll_url, instance_id) do
      mat = %__MODULE__{
        ca_cert:       ca_cert,
        instance_cert: cert,
        instance_key:  key,
        fingerprint:   fp,
        instance_cn:   cn,
        instance_id:   instance_id,
      }
      Logger.info("[cert-forge] Instance cert obtained (CN=#{cn})")
      {:ok, mat}
    end
  end

  defp derive_url(url, port) do
    Regex.replace(~r/:\d+$/, url, ":#{port}")
  end

  defp fetch_ca_cert(public_url, attempt \\ 1)
  defp fetch_ca_cert(_, 31), do: {:error, "Could not obtain CA cert after 30 attempts"}
  defp fetch_ca_cert(public_url, attempt) do
    url = String.trim_trailing(public_url, "/") <> "/ca"
    {output, code} = System.cmd("curl", ["--silent", "--fail", url], stderr_to_stdout: true)
    if code == 0 do
      case JSON.decode(output) do
        {:ok, %{"ca_cert" => ca_cert}} ->
          Logger.info("[cert-forge] CA cert obtained")
          {:ok, ca_cert}
        _ ->
          Logger.info("[cert-forge] Waiting for /ca (attempt #{attempt}/30)")
          Process.sleep(2_000)
          fetch_ca_cert(public_url, attempt + 1)
      end
    else
      Logger.info("[cert-forge] Waiting for /ca (attempt #{attempt}/30): curl exit #{code}")
      Process.sleep(2_000)
      fetch_ca_cert(public_url, attempt + 1)
    end
  end

  defp request_instance_cert(enroll_url, instance_id, attempt \\ 1)
  defp request_instance_cert(_, _, 31), do: {:error, "Could not obtain instance cert after 30 attempts"}
  defp request_instance_cert(enroll_url, instance_id, attempt) do
    url  = String.trim_trailing(enroll_url, "/") <> "/instance-cert"
    body = JSON.encode!(%{service_name: @service_name, instance_id: instance_id})

    enroll_cert = System.get_env("ENROLLMENT_CERT", "/certs/enrollment.crt")
    enroll_key  = System.get_env("ENROLLMENT_KEY",  "/certs/enrollment.key")

    {output, code} = System.cmd("curl", [
      "--silent", "--fail",
      "--cert",    enroll_cert,
      "--key",     enroll_key,
      "--insecure",  # enrollment CA verified server-side
      "--request", "POST",
      "--header",  "Content-Type: application/json",
      "--data",    body,
      url
    ], stderr_to_stdout: true)

    if code == 0 do
      case JSON.decode(output) do
        {:ok, %{"cert" => cert, "key" => key, "fingerprint" => fp, "instance_cn" => cn}} ->
          {:ok, cert, key, fp, cn}
        _ ->
          Logger.info("[cert-forge] Waiting for /instance-cert (attempt #{attempt}/30)")
          Process.sleep(2_000)
          request_instance_cert(enroll_url, instance_id, attempt + 1)
      end
    else
      Logger.info("[cert-forge] Waiting for /instance-cert (attempt #{attempt}/30): curl exit #{code}")
      Process.sleep(2_000)
      request_instance_cert(enroll_url, instance_id, attempt + 1)
    end
  end

  @doc "Sign payload via cert-forge /sign, returns {:ok, signature} or {:error, reason}"
  def sign_payload(mat, payload) do
    forge_url  = System.get_env("CERT_FORGE_URL", "https://cert-forge:4014")
    encoded    = Base.encode64(payload)
    body       = JSON.encode!(%{
      service_name: @service_name,
      instance_id:  mat.instance_id,
      payload:      encoded,
    })

    # Write cert+key to temp files for curl
    with {:ok, cert_path, key_path, ca_path} <- write_temp_certs(mat),
         {output, 0} <- System.cmd("curl", [
           "--silent", "--fail",
           "--cacert",  ca_path,
           "--cert",    cert_path,
           "--key",     key_path,
           "--request", "POST",
           "--header",  "Content-Type: application/json",
           "--data",    body,
           forge_url <> "/sign"
         ], stderr_to_stdout: true),
         {:ok, %{"signature" => sig}} <- JSON.decode(output) do
      cleanup_temp_files([cert_path, key_path, ca_path])
      {:ok, sig}
    else
      {output, code} ->
        Logger.warning("[cert-forge] /sign failed (exit=#{code}): #{output}")
        {:error, "sign failed"}
      err ->
        {:error, inspect(err)}
    end
  end

  @doc "Register with Augur Canis"
  def self_register(mat) do
    ac_url    = System.get_env("AUGUR_CANIS_URL", "https://augur-canis:4010")
    timestamp = DateTime.utc_now() |> DateTime.to_iso8601()
    payload   = @service_name <> @endpoint <> mat.fingerprint <> timestamp

    do_register(mat, ac_url, timestamp, payload, 1)
  end

  defp do_register(_, _, _, _, 11) do
    Logger.warning("[feed-wrangler] selfRegister: giving up after 10 attempts")
  end
  defp do_register(mat, ac_url, timestamp, payload, attempt) do
    case sign_payload(mat, payload) do
      {:ok, sig} ->
        body = JSON.encode!(%{
          service_name:     @service_name,
          network_endpoint: @endpoint,
          cert_fingerprint: mat.fingerprint,
          cert_pem:         mat.instance_cert,
          timestamp:        timestamp,
          signature:        sig,
        })
        url = String.trim_trailing(ac_url, "/") <> "/services/register"
        with {:ok, cert_path, key_path, ca_path} <- write_temp_certs(mat),
             {response, 0} <- System.cmd("curl", [
               "--silent",
               "--cacert",  ca_path,
               "--cert",    cert_path,
               "--key",     key_path,
               "--request", "POST",
               "--header",  "Content-Type: application/json",
               "--data",    body,
               url
             ], stderr_to_stdout: true),
             {:ok, ack} <- JSON.decode(response) do
          cleanup_temp_files([cert_path, key_path, ca_path])
          Logger.info("[feed-wrangler] selfRegister: registered with AC (status=#{ack["status"]})")
        else
          _ ->
            Logger.info("[feed-wrangler] selfRegister: attempt #{attempt}/10 failed")
            Process.sleep(3_000)
            do_register(mat, ac_url, timestamp, payload, attempt + 1)
        end
      {:error, reason} ->
        Logger.info("[feed-wrangler] selfRegister: sign failed (attempt #{attempt}/10): #{reason}")
        Process.sleep(3_000)
        do_register(mat, ac_url, timestamp, payload, attempt + 1)
    end
  end

  # Write cert+key+ca to temp files for curl — cleaned up immediately after use
  defp write_temp_certs(mat) do
    cert_path = "/tmp/fw-cert-#{:erlang.unique_integer([:positive])}.crt"
    key_path  = "/tmp/fw-key-#{:erlang.unique_integer([:positive])}.key"
    ca_path   = "/tmp/fw-ca-#{:erlang.unique_integer([:positive])}.crt"
    File.write!(cert_path, mat.instance_cert)
    File.write!(key_path,  mat.instance_key)
    File.write!(ca_path,   mat.ca_cert)
    {:ok, cert_path, key_path, ca_path}
  end

  defp cleanup_temp_files(paths) do
    Enum.each(paths, &File.rm/1)
  end
end
