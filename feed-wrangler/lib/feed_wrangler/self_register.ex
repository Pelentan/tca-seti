defmodule FeedWrangler.SelfRegister do
  @moduledoc """
  Registers feed-wrangler with Augur Canis on startup.
  Signs the registration payload with the service private key via openssl.
  Uses curl for the mTLS HTTP call — avoids adding HTTP client dependencies.
  """
  require Logger

  @service_name "feed-wrangler"
  @endpoint     "https://feed-wrangler:4007"
  @cert_path    "/certs/feed-wrangler.crt"
  @key_path     "/certs/feed-wrangler.key"
  @ca_path      "/certs/ca.crt"

  def register do
    Process.sleep(5_000)

    ac_url = System.get_env("AUGUR_CANIS_URL", "https://augur-canis:4010")

    unless File.exists?(@cert_path) and File.exists?(@key_path) do
      Logger.warning("[feed-wrangler] selfRegister: certs not found — skipping")
    else
      try do
        do_register(ac_url)
      rescue
        e -> Logger.warning("[feed-wrangler] selfRegister: #{inspect(e)}")
      end
    end
  end

  defp do_register(ac_url) do
    timestamp = DateTime.utc_now() |> DateTime.to_iso8601()

    # Get cert fingerprint
    {fp_out, 0} = System.cmd("openssl", [
      "x509", "-fingerprint", "-sha256", "-noout", "-in", @cert_path
    ])
    fingerprint =
      fp_out
      |> String.trim()
      |> String.split("=")
      |> List.last()
      |> String.replace(":", "")
      |> String.downcase()

    # Build payload string and write to temp file for signing
    payload = @service_name <> @endpoint <> fingerprint <> timestamp
    tmp_payload = "/tmp/fw-reg-payload"
    tmp_sig     = "/tmp/fw-reg-sig"
    File.write!(tmp_payload, payload)

    # Sign with private key
    {_, 0} = System.cmd("openssl", [
      "dgst", "-sha256", "-sign", @key_path,
      "-out", tmp_sig, tmp_payload
    ])

    # Base64-encode the signature
    {sig_b64, 0} = System.cmd("openssl", [
      "base64", "-in", tmp_sig, "-A"
    ])

    # Clean up temp files
    File.rm(tmp_payload)
    File.rm(tmp_sig)

    body = JSON.encode!(%{
      service_name:     @service_name,
      network_endpoint: @endpoint,
      cert_fingerprint: fingerprint,
      timestamp:        timestamp,
      signature:        String.trim(sig_b64),
    })

    url = String.trim_trailing(ac_url, "/") <> "/services/register"

    # Use curl for mTLS POST — avoids adding an HTTP client dependency
    {response, exit_code} = System.cmd("curl", [
      "--silent",
      "--cacert",  @ca_path,
      "--cert",    @cert_path,
      "--key",     @key_path,
      "--request", "POST",
      "--header",  "Content-Type: application/json",
      "--data",    body,
      url
    ], stderr_to_stdout: true)

    if exit_code == 0 do
      case JSON.decode(response) do
        {:ok, ack} ->
          Logger.info("[feed-wrangler] selfRegister: registered with AC (status=#{ack["status"]})")
        _ ->
          Logger.info("[feed-wrangler] selfRegister: AC responded: #{String.slice(response, 0, 100)}")
      end
    else
      Logger.warning("[feed-wrangler] selfRegister: curl failed (exit=#{exit_code}): #{response}")
    end
  end
end
