# TCA Federation Protocol
## Cross-CA mTLS Trust Establishment

**Version:** 1.3  
**Status:** Draft  
**Authors:** Michael E. Shaffer / Claude Sonnet 4.6  
**Date:** 2026-04-25  
**Updated:** 2026-04-26

---

## Overview

The TCA Federation Protocol establishes mutual TLS trust between two TCA constellations that share no common Certificate Authority.  It uses a pre-shared identity keypair — the Star-Gazer cert — as the sole trust anchor.  No CA bundles are exchanged, no pre-shared secrets beyond the Star-Gazer keypair are required.

Once the handshake completes, all subsequent communication uses standard mTLS with certificates issued by each constellation's own cert-forge.

This protocol is novel.  It is not based on SPIFFE, ACME, or any existing federation standard.  It is specific to the TCA architecture.

---

## Participants

| Role | Component | Key Material |
|------|-----------|--------------|
| **Monitor** | SETI connie-agent | Star-Gazer private key + public cert; connie-agent SETI instance cert |
| **Target AC** | Constellation Augur Canis | Star-Gazer public cert (pre-loaded at startup) |

---

## Cryptographic Model

**Federation port transport:**
- Target AC binds a dedicated federation port on startup, using the star-gazer cert as its TLS server certificate
- This port only opens if the star-gazer cert is present — if absent, the port does not bind and federation is unavailable
- connie-agent connects to this port and verifies the server cert against the star-gazer public cert it already holds
- This provides mutual recognition at the transport layer before any application data is exchanged

**Application-layer authentication:**
- Messages 1 and 3: connie-agent signs with the Star-Gazer private key (rsa.SignPKCS1v15); Target AC verifies with the Star-Gazer public cert (rsa.VerifyPKCS1v15)
- Message 2: Target AC encrypts with the Star-Gazer public cert (rsa.EncryptOAEP); connie-agent decrypts with the Star-Gazer private key (rsa.DecryptOAEP)

All RSA operations use proper padding (PKCS1v15 for signing, OAEP for encryption).  Raw RSA operations are not used.

---

## Federation Port

Target AC binds a dedicated federation port separate from its standard mTLS port.

| Property | Value |
|----------|-------|
| Convention | mTLS port last two digits flipped (e.g. 3025 -> 3052) |
| Configurable | Yes — set via FEDERATION_PORT env var |
| Server cert | Star-Gazer cert (not the AC instance cert) |
| Client cert | Not required — application-layer signatures authenticate the caller |
| Conditional | Port only binds if star-gazer cert is present at startup |
| Endpoints | /federation/register and /federation/acknowledge only |

connie-agent connects to this port and verifies the server cert against the star-gazer public cert it already holds.  A successful TLS handshake proves the server possesses the star-gazer cert — mutual recognition before any application data is exchanged.

The federation_endpoint field in remote-apps.json specifies the full URL including port.  There is no fixed port requirement — the convention of flipping the last two digits is a guideline, not a constraint.

---

## Pre-conditions

Before the handshake begins:

- Target AC has loaded the Star-Gazer public cert at startup from the seti-stargazer-cert Secret in its namespace (written by connie-agent on first onboarding — requires a manual AC restart to take effect).
- Target AC has bound the federation port using the star-gazer cert as its TLS server certificate.
- connie-agent holds the Star-Gazer private key and public cert (mounted from the SETI cert Secret).
- connie-agent knows the Target AC's federation endpoint via the federation_endpoint field in remote-apps.json.
- Both sides have running cert-forge instances that can issue instance certs on demand.
- Neither side knows the other's CA cert.
- connie-agent has its own SETI-issued instance cert from SETI cert-forge.

---

## AC Restart Policy

- First write (new constellation onboarding): manual AC restart required.  This is a deliberate human checkpoint before trust is established.
- Subsequent writes (cert rotation): connie-agent triggers an automated rolling restart of the AC deployment via the Kubernetes API.

connie-agent determines first-write vs rotation by whether the Secret already existed before writing — POST (new Secret) flags manual; PUT (existing Secret) triggers automated restart.

---

## The Three-Message Handshake

### Message 1 — Monitor -> Target AC
Endpoint: POST /federation/register  
Transport: TLS on federation port — server cert is star-gazer cert, no client cert required

Monitor sends:
```json
{
  "payload": "<base64 — JSON of {seti_instance_id, timestamp}>",
  "signature": "<base64 — RSA-SHA256 signature of payload, signed with Star-Gazer private key>"
}
```

Target AC verifies:
1. Decodes payload from base64 and parses the JSON.
2. Verifies signature using rsa.VerifyPKCS1v15 with the pre-loaded Star-Gazer public cert.
3. Verifies timestamp is within +/-5 minutes to prevent replay attacks.
4. If all checks pass — Monitor is authenticated as a legitimate SETI instance.

On success, Target AC:
- Requests a new session cert from its own cert-forge (signed by the constellation CA).
- Records seti_instance_id in pending federation state.
- Sets a 30-second timeout — if Message 3 is not received, partial state is discarded.

---

### Message 2 — Target AC -> Monitor
Response to Message 1. Status: 200 OK

Target AC responds:
```json
{
  "encrypted_payload": "<base64 — OAEP-encrypted JSON of {session_cert, session_cert_id, issued_at, constellation_id}>"
}
```

Payload is encrypted with the Star-Gazer public cert (rsa.EncryptOAEP).  Only the Star-Gazer private key holder can decrypt it.

Monitor verifies:
1. Decrypts encrypted_payload using the Star-Gazer private key (rsa.DecryptOAEP).  Successful decryption confirms a legitimate AC responded.
2. Parses session_cert — this becomes the trusted root for subsequent mTLS connections.

Monitor does not send any cert material until this decryption succeeds.

---

### Message 3 — Monitor -> Target AC
Endpoint: POST /federation/acknowledge  
Transport: TLS on federation port

Monitor sends:
```json
{
  "payload": "<base64 — JSON of {seti_instance_id, session_cert_id, monitor_cert_public}>",
  "signature": "<base64 — RSA-SHA256 signature of payload, signed with Star-Gazer private key>"
}
```

Target AC verifies:
1. Verifies signature using rsa.VerifyPKCS1v15 with the Star-Gazer public cert.
2. Decodes and parses payload.
3. Verifies session_cert_id matches what was issued in Message 2.
4. Parses monitor_cert_public — the cert signal-aggregator will present for all subsequent mTLS connections.
5. Adds monitor_cert_public to its TLS ClientCAs pool dynamically (mutex-protected).
6. Marks the federation session as active.  Clears pending handshake state.

---

## Handshake Atomicity

The handshake is all-or-nothing.  Nothing is persisted on either side until Message 3 completes successfully.

If connie-agent restarts between Message 1 and Message 3: connie-agent discards partial state on startup and initiates a fresh Message 1 on its next sync cycle.  Target AC's partial state is abandoned.

If Target AC restarts between Message 1 and Message 3: connie-agent detects the failure and initiates a fresh Message 1 on its next sync cycle.

Rule: No cert material is written to any store until Message 3 is successfully verified.

---

## Session Lifetime

A federation session lives as long as the Target AC pod.  When Target AC restarts, the session is lost and connie-agent must re-run the full handshake on its next sync cycle.

Session state is held in memory only.  There is no persistence layer for federation state.  A restart is a clean slate.

---

## Post-Handshake

connie-agent calls signal-aggregator POST /federation/subscriptions with:
- application_id
- ac_endpoint
- session_cert (trusted root for server cert verification)
- session_cert_id (for event signature verification)
- monitor_cert_pem (the cert signal-aggregator presents as its mTLS client cert)
- monitor_key_pem (the private key for that cert)

Signal-aggregator builds its mTLS client from those pieces and connects to GET /stream on the standard mTLS port.  It never touches the Star-Gazer key.  It never knows how trust was established.

---

## Post-Handshake State

After Message 3 completes:

| Side | Holds | Trusts |
|------|-------|--------|
| Monitor (connie-agent) | Target AC session cert (as trusted root) | Target AC mTLS server cert (signed by constellation CA, verified via session cert) |
| Target AC | Monitor instance cert public key (from Message 3) | Monitor mTLS client cert (SETI-signed, verified against stored public key) |

All subsequent connections use standard mTLS on the standard AC port.  The federation port is only used for the three-message handshake.

---

## Reconnection

If the SSE stream drops:
1. Signal-aggregator detects stream close.
2. Signal-aggregator re-presents its SETI instance cert on a new connection to GET /stream.
3. Target AC verifies the cert matches monitor_cert_public stored from Message 3.
4. If the session is still active — connection resumes.  No full re-handshake required.
5. If Target AC has restarted — connie-agent re-runs the full handshake from Message 1.

---

## Security Properties

- Minimal exposure: Message 1 carries only identity and timestamp — no cert material until both sides have verified each other.
- Transport-layer recognition: The federation port uses the star-gazer cert as the server cert providing mutual recognition before application data is exchanged.
- No CA coupling: Neither side shares or distributes its CA cert.
- Replay prevention: Timestamp in Message 1 with +/-5 minute skew window.
- Forward integrity: Star-Gazer key compromise after handshake completion does not affect ongoing sessions.
- Single pre-shared secret: Only the Star-Gazer keypair is pre-shared.
- Key isolation: The Star-Gazer private key lives exclusively in connie-agent.
- Safe padding: All RSA operations use PKCS1v15 or OAEP padding.
- Dedicated federation port: Federation endpoints are isolated from the mTLS port.

---

## Error Cases

| Condition | Expected Behavior |
|-----------|-------------------|
| Star-Gazer cert absent at AC startup | Federation port does not bind; federation unavailable |
| Signature verification failure on Message 1 or 3 | 401 — reject, log, discard all state |
| Timestamp outside skew window | 401 — reject, log (possible replay) |
| Decryption failure on Message 2 (connie-agent) | Discard partial state, retry next sync cycle |
| session_cert_id mismatch in Message 3 | 401 — reject, log, discard all state |
| monitor_cert_public already registered (active session) | 200 — return existing session, no re-handshake |
| Message 3 not received within 30 seconds | Target AC discards partial state |
| Either side restarts mid-handshake | Both discard partial state; connie-agent re-runs full handshake next sync cycle |
| Target AC restart during active session | Signal-aggregator detects stream close; connie-agent re-runs full handshake next sync cycle |

---

## Implementation Notes

- monitor_cert_public is sent only in Message 3, after Monitor has confirmed it is talking to a legitimate AC.
- The Star-Gazer cert is never sent over the wire.  Target AC has it pre-loaded.
- The dynamic ClientCAs pool in Target AC must be protected by a mutex — live TLS config write.
- connie-agent holds the Star-Gazer private key.  No other SETI Job holds it.
- Clock skew: NTP synchronization between connie-agent and Target AC is a required operational assumption.
- Partial state cleanup: Target AC discards partial handshake state after 30 seconds if Message 3 is not received.
- Federation port: Uses the star-gazer cert as the TLS server cert.  Does not require client certs.  Serves only /federation/register and /federation/acknowledge.
- remote-apps.json: Must include federation_endpoint field with full URL including port.
