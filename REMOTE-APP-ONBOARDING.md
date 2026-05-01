# SETI — Remote Application Onboarding Guide

**Version:** 1.0  
**Last Updated:** 2026-04-29  
**Audience:** Wr4nglers onboarding a new TCA constellation into SETI monitoring  
**Repository:** github.com/Pelentan/tca-seti

---

## Overview

Onboarding a remote TCA constellation into SETI establishes a federated monitoring
relationship between SETI and the remote application's Augur Canis (AC).  Once
complete, SETI will:

- Display inter-service traffic from the remote constellation on the SETI dashboard
- Run contract tests against the remote constellation on demand and on schedule
- Display contract test results in the Ring Reports tab
- Execute Plot tests against the remote constellation
- Feed anomaly data through the SETI intelligence layer (AI-lien, Lore, Interactions)

The federation uses a three-message cryptographic handshake secured by the Star-Gazer
keypair.  The remote AC must be running and reachable from the SETI cluster.

---

## Prerequisites

Before starting:

1. The remote constellation must be running in Kubernetes with its own cert-forge
   and augur-canis deployed.
2. The remote AC must have the SETI federation port enabled (default: 3052).
3. You must have `kubectl` access to both the SETI namespace and the remote
   constellation's namespace.
4. The remote constellation's namespace must be labelled with
   `kubernetes.io/metadata.name: <namespace>` (K8s sets this automatically).
5. You need a GitHub Personal Access Token (PAT) scoped to `read:contents` on the
   remote constellation's repository — for fetching Plot definitions.

---

## Step 1 — Generate the GitHub Registry Token

Plot definitions are fetched directly from the remote constellation's GitHub
repository.  You need a fine-grained Personal Access Token (PAT) scoped to just
that repository with read-only access to its contents.

### 1.1 — Log into GitHub

Go to [https://github.com](https://github.com) and sign in with the account that
has access to the remote constellation's repository.

### 1.2 — Open Token Settings

Click your **profile photo** in the top-right corner of any GitHub page.  
Select **Settings** from the dropdown menu.

On the Settings page, scroll all the way to the bottom of the left sidebar.  
Click **Developer settings**.

### 1.3 — Navigate to Fine-Grained Tokens

In the Developer settings left sidebar, click **Personal access tokens**.  
Click **Fine-grained tokens** in the submenu that appears.

Click the green **Generate new token** button in the top right.

### 1.4 — Configure the Token

Fill in the form as follows:

**Token name:**  
Enter a descriptive name that identifies what this token is for.  
Example: `seti-monitor-tca-vox`

**Expiration:**  
Click the dropdown and select an expiration period.  90 days is a reasonable
default for most environments.  For production, align with your security policy.
GitHub will send you an email reminder before it expires.

**Description (optional):**  
Example: `Read-only access for SETI monitoring to fetch Plot definitions`

**Resource owner:**  
Leave this as your personal account, or select the organization that owns the
repository if it belongs to an org.

### 1.5 — Select the Repository

Under **Repository access**, select **Only select repositories**.

A dropdown will appear.  Click it and type the name of the remote constellation's
repository.  Select it from the list.

Example: `Pelentan/tca-vox`

### 1.6 — Set Permissions

Scroll down to the **Permissions** section.  You will see two groups:
**Repository permissions** and **Account permissions**.

Under **Repository permissions**, find **Contents** in the list.  
Click the dropdown next to it (it will say "No access") and select **Read-only**.

Leave everything else at **No access**.  Do not grant any additional permissions.

**A note on folder-level scoping:**  
GitHub fine-grained PATs grant read access to the entire repository's contents —
GitHub does not currently support restricting a token to a specific folder or path.
However, SETI only ever requests content from the `contracts/plots/` path within
the repository.  The token has the technical capability to read more, but connie-agent
never exercises it.  If your security policy requires tighter control, the recommended
approach is to host Plot definitions in a dedicated repository that contains only
that content, then scope the token to that repository.

### 1.7 — Generate the Token

Scroll to the bottom and click the green **Generate token** button.

GitHub will show you the token exactly once.  It looks like:
`github_pat_11ABCDE...` (a long string starting with `github_pat_`)

**Copy the token immediately.**  Once you leave this page you cannot see it again.
If you lose it, you must generate a new one.

Store it temporarily in a password manager or secure note — you will use it in
the next step and then it will live only in the K8s secret.

---

## Step 2 — Load the Registry Token into K8s

### Development (Option 1 — File method, no shell history)

```bash
# Write token to a temp file — do NOT use echo "token" > file (stays in history)
# Instead open an editor or use read:
read -rs REGISTRY_TOKEN
# Paste your token and press Enter, then Ctrl+D

printf '%s' "$REGISTRY_TOKEN" > /tmp/registry-token.txt

# Load into K8s secret in the SETI namespace
kubectl create secret generic tca-vox-registry-token \
  --from-file=token=/tmp/registry-token.txt \
  -n seti

# Overwrite and delete the temp file
shred -u /tmp/registry-token.txt

# Verify (value will be base64 encoded — that's correct)
kubectl get secret tca-vox-registry-token -n seti
```

### Production (Option 2 — Vault/password manager method)

Replace `pass show github/tca-vox-token` with your vault's retrieval command.

```bash
# 1Password CLI example:
op item get "seti-monitor-tca-vox" --fields token | \
  kubectl create secret generic tca-vox-registry-token \
  --from-literal=token="$(cat)" \
  -n seti

# pass example:
pass show github/tca-vox-token | \
  kubectl create secret generic tca-vox-registry-token \
  --from-literal=token="$(cat)" \
  -n seti

# HashiCorp Vault example:
vault kv get -field=token secret/seti/tca-vox-registry | \
  kubectl create secret generic tca-vox-registry-token \
  --from-literal=token="$(cat)" \
  -n seti
```

---

## Step 3 — Add the Remote Application to the ConfigMap

Edit `charts/seti/templates/configmaps/remote-apps-configmap.yaml` in the SETI
repository.  Add the new constellation to the JSON object:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: seti-remote-apps
  namespace: {{ .Values.namespace }}
  labels:
    {{- include "seti.labels" . | nindent 4 }}
data:
  remote-apps.json: |
    {
      "tca-vox": {
        "name": "TCA Vox",
        "description": "Polyglot polling application — TCA proof of concept.",
        "namespace": "tca-vox",
        "ac_endpoint": "https://augur-canis.tca-vox.svc.cluster.local:3025",
        "federation_endpoint": "https://augur-canis.tca-vox.svc.cluster.local:3052",
        "ca_url": "http://cert-forge.tca-vox.svc.cluster.local:3022/ca",
        "registry_url": "https://api.github.com/repos/Pelentan/tca-vox/contents",
        "registry_type": "github",
        "registry_token_secret": "tca-vox-registry-token"
      }
    }
```

**Field reference:**

| Field | Description |
|-------|-------------|
| `name` | Display name shown in the SETI UI tabs |
| `description` | Brief description of the application |
| `namespace` | Kubernetes namespace the remote app runs in |
| `ac_endpoint` | mTLS URL for the remote AC's admin port (3025) |
| `federation_endpoint` | TLS URL for the remote AC's federation port (3052) |
| `ca_url` | HTTP URL for the remote cert-forge's public CA endpoint |
| `registry_url` | GitHub API base URL for the repository contents |
| `registry_type` | `github` (only supported type currently) |
| `registry_token_secret` | Name of the K8s Secret created in Step 2 |

---

## Step 4 — Add Network Policy for the Remote Namespace

The remote AC must allow ingress from the SETI namespace on its mTLS and federation
ports.  In the remote application's Helm chart, ensure a network policy exists:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: augur-canis-seti-ingress
  namespace: <remote-namespace>
spec:
  podSelector:
    matchLabels:
      app: augur-canis
  policyTypes:
    - Ingress
  ingress:
    - from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: seti
      ports:
        - port: 3025   # mTLS admin port
        - port: 3052   # federation port
```

Apply to the cluster and upgrade the remote application's Helm chart.

---

## Step 5 — Write the Star-Gazer Cert to the Remote Namespace

The Star-Gazer cert is SETI's pre-shared trust anchor.  Connie-agent writes it
automatically during its first sync cycle.  You can verify it was written:

```bash
kubectl get secret seti-stargazer-cert -n <remote-namespace>
```

If it is not present after connie-agent's first cycle (up to 60 seconds after
deployment), check connie-agent logs:

```bash
kubectl logs deployment/connie-agent -n seti | grep "stargazer\|tca-vox"
```

The remote AC mounts this secret at `/stargazer/tls.crt` and reloads it on every
federation handshake attempt — no manual restart required when SETI rotates the cert.

---

## Step 6 — Deploy the SETI Changes

```bash
# From the tca-seti repository root:
helm upgrade seti charts/seti -f charts/seti/values/dev.yaml -n seti
```

Connie-agent will automatically restart (new ConfigMap triggers rollout) and begin
the federation handshake within 30 seconds.

---

## Step 7 — Verify Federation

Watch connie-agent establish the federation:

```bash
kubectl logs -f deployment/connie-agent -n seti | grep "federation\|tca-vox"
```

Expected sequence:

```
[connie-agent] tca-vox: initiating federation
[federation] Fetched constellation CA from http://cert-forge.tca-vox...
[federation] Sending Message 1 to https://augur-canis.tca-vox...
[federation] Message 2 decrypted — AC verified (constellation=tca-vox session=XXXXXXXX)
[federation] Sending Message 3 to https://augur-canis.tca-vox...
[federation] Handshake complete — ... (constellation=tca-vox session=XXXXXXXX)
[federation] signal-aggregator connected to tca-vox stream
[connie-agent] tca-vox: activated in Policy
```

Verify the TCA Vox tab appears in the SETI dashboard.

---

## Step 8 — Verify Signal Aggregator

```bash
kubectl logs deployment/signal-aggregator -n seti | grep "Constellation connected"
```

Expected:
```
[federation] Constellation connected app=tca-vox endpoint=https://augur-canis.tca-vox... session=XXXXXXXX reconnects=0 monitorCertLeaves=1
```

`monitorCertLeaves=1` confirms the monitor cert is loaded and ready for proxy requests.

---

## Step 9 — Verify Contract Tests

From the SETI dashboard with the TCA Vox tab active, click **Run Contract Tests**.
The button should change to **Test Queued ✓**.

After the test run completes (typically 2-10 seconds), navigate to **Ring → Reports**
with the TCA Vox tab active.  The most recent run should appear in the Contract Tests
column.

---

## Ongoing Operation

### Self-Healing

Federation is self-healing.  Connie-agent monitors the `federation:<tag>:events`
Redis channel.  If no events arrive within 30 seconds, it automatically re-runs the
full handshake and re-enrolls with signal-aggregator.  A warning is published to
`seti:alerts` when this occurs.

The remote AC reloads the Star-Gazer cert from disk on every handshake attempt,
so SETI restarts do not require manual intervention on the remote side.

### Removing a Remote Application

1. Remove the application entry from `remote-apps-configmap.yaml`
2. Delete the registry token secret: `kubectl delete secret <token-secret> -n seti`
3. Deploy: `helm upgrade seti charts/seti -f charts/seti/values/dev.yaml -n seti`

Connie-agent will detect the removal on restart and clean up the federation
subscription in signal-aggregator automatically.

### Token Rotation

When a GitHub PAT expires:

1. Generate a new token on GitHub (same permissions as Step 1)
2. Delete the old secret: `kubectl delete secret <token-secret> -n seti`
3. Create the new secret using Step 2 (Option 1 or 2)

No application restart is required — connie-agent reads the secret on each plot sync cycle.

---

## Troubleshooting

### "signature verification failed" on Message 1

The Star-Gazer cert in the remote namespace is stale.  Wait for connie-agent to
write a fresh cert (it runs every 60 seconds) and retry.  The remote AC reloads
the cert automatically — no manual restart needed.

### TCA Vox tab not appearing in SETI UI

Check Policy received the activation:
```bash
kubectl logs deployment/policy -n seti | grep "tca-vox\|registered"
```

If Policy shows the app but the tab doesn't appear, refresh the browser — the UI
polls `/available-applications` on mount.

### "constellation not active" on Run Contract Tests

Signal-aggregator lost its federation state (typically after a restart).  Wait for
connie-agent's next silence detection cycle (up to 30 seconds) to re-initiate
the handshake and re-register with signal-aggregator.

### Federation handshake succeeding but no events on TCA Vox tab

Check the gateway is subscribed to the federation channel:
```bash
kubectl logs deployment/gateway -n seti | grep "federation:tca-vox"
```

If missing, the gateway missed the announcement.  Restart the gateway — it queries
Policy for active constellations on startup and subscribes automatically.

---

*Document maintained by the SETI Connie Wr4ngler.*  
*For architecture questions, see `SETI-GUIDELINES.md` and `AC-GUIDELINES.md`.*
