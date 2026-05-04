# SETI — Plot Test Primer

**Version:** 1.0  
**Last Updated:** 2026-05-01  
**Audience:** Wr4nglers writing behavioral test plots for TCA constellations

---

## What Is a Plot?

A Plot is a behavioral test that verifies a real user flow across multiple services.
Where contract tests verify that individual endpoints respond correctly, plots verify
that a sequence of calls — representing an actual user action — produces the correct
outcome across service boundaries.

Plots are committed artifacts.  They live in `contracts/plots/` alongside the OpenAPI
contracts and are versioned with the application.  A plot that passes means the
user flow it describes is working end to end.

---

## Plot File Format

Plots are JSON files stored in `contracts/plots/` in the constellation's repository.
SETI's connie-agent fetches them automatically on each sync cycle and registers them
with plot-store.

### Top-Level Fields

```json
{
  "name": "Poll Creation Flow",
  "version": "v1",
  "description": "Verifies the poll creation path across poll-builder and poll-wr4ngler.",
  "constellation": "tca-vox",
  "execution_mode": "internal",
  "gateway_url": "",
  "context": {
    "owner_id": "test-owner-123"
  },
  "steps": []
}
```

| Field | Required | Description |
|-------|----------|-------------|
| `name` | Yes | Display name in the SETI UI |
| `version` | No | Semantic version string — displayed in UI |
| `description` | No | Explains what user flow this plot tests |
| `constellation` | Yes | The constellation ID this plot belongs to |
| `execution_mode` | No | `internal` (default) or `external` |
| `gateway_url` | No | Required for `external` mode — the constellation's public gateway URL |
| `context` | No | Initial variable values available to all steps via `{variable}` substitution |

---

## Step Format

All steps use the same unified schema regardless of execution mode.

```json
{
  "step": 1,
  "service": "poll-builder",
  "method": "POST",
  "path": "/polls",
  "body": {
    "title": "Test Poll",
    "owner_id": "{owner_id}"
  },
  "headers": {
    "X-Custom-Header": "value"
  },
  "expected_status": 201,
  "expected_fields": ["poll_id", "title"],
  "extract_fields": {
    "id": "poll_id"
  },
  "stop_on_failure": true,
  "notes": "Creates a new poll and captures the poll_id for subsequent steps"
}
```

| Field | Required | Description |
|-------|----------|-------------|
| `step` | Yes | Step number — steps execute in ascending order |
| `service` | Internal only | The service name as registered with AC.  Used for internal routing. |
| `method` | Yes | HTTP method: GET, POST, PUT, PATCH, DELETE |
| `path` | Yes | URL path.  Supports `{variable}` substitution from context or previous captures. |
| `body` | No | Request body.  Supports `{variable}` substitution in string values. |
| `headers` | No | Additional request headers |
| `expected_status` | Yes | HTTP status code the step must return to pass |
| `expected_fields` | No | Response body must contain these top-level fields |
| `extract_fields` | No | Capture response fields into context for later steps — `{"context_name": "response_field"}` |
| `stop_on_failure` | No | If true, stops the entire plot on this step's failure.  Default: false |
| `notes` | No | Human-readable explanation of what this step does |

---

## Variable Substitution

Variables in `{braces}` are substituted at runtime.  There are two sources:

**1. Plot-level context** — defined in the top-level `context` object:
```json
"context": {
  "owner_id": "test-owner-123",
  "base_path": "/api/v1"
}
```

**2. Step captures** — extracted from a previous step's response via `extract_fields`:
```json
"extract_fields": {
  "poll_id": "id"
}
```

This captures the `id` field from the response body and makes it available as
`{poll_id}` in all subsequent steps.

Example usage in a later step:
```json
{
  "step": 2,
  "method": "GET",
  "path": "/polls/{poll_id}",
  "expected_status": 200
}
```

---

## Execution Modes

### Internal Mode (default)

**Use when:** Testing inter-service behavior from the inside — verifying that services
talk to each other correctly using their internal mTLS connections.

**How it works:**
1. SETI plot-test sends the plot to signal-aggregator's federation proxy
2. Signal-aggregator forwards to the remote constellation's AC via mTLS monitor cert
3. The remote AC executes each step using its own internal service registry
4. The `service` field tells AC which internal service to call
5. Results are returned to SETI and written to plot-store

**The `service` field is required** for internal plots.  It must match the service
name as registered with the constellation's AC.

```json
{
  "execution_mode": "internal",
  "steps": [
    {
      "step": 1,
      "service": "poll-builder",
      "method": "POST",
      "path": "/polls",
      "expected_status": 201
    }
  ]
}
```

**What internal mode tests:**  Whether services are up, reachable from AC, and
responding correctly to each other over mTLS.  This is the primary mode for
verifying inter-service contracts are holding in a running constellation.

---

### External Mode

**Use when:** Testing the constellation as an external client would — through the
public gateway, with real authentication.  Catches issues that internal testing
misses: gateway routing bugs, auth middleware failures, public API surface problems.

**How it works:**
1. SETI plot-test requests a short-lived token from connie-agent
2. Connie-agent authenticates with the remote AC using the Star-Gazer trust channel
3. The remote AC issues a scoped, short-lived JWT
4. SETI plot-test executes each step directly against the remote gateway using that token
5. Results are written to plot-store

**The `gateway_url` field is required** for external plots.

**The `service` field is ignored** for external plots — the gateway handles routing.
The `path` should be the full gateway-routable path.

```json
{
  "execution_mode": "external",
  "gateway_url": "https://vox.tca.local",
  "steps": [
    {
      "step": 1,
      "method": "POST",
      "path": "/api/polls/polls",
      "body": { "title": "Test Poll" },
      "expected_status": 201,
      "extract_fields": { "poll_id": "id" }
    },
    {
      "step": 2,
      "method": "GET",
      "path": "/api/polls/polls/{poll_id}",
      "expected_status": 200
    }
  ]
}
```

**What external mode tests:**  Whether the full request lifecycle works from outside
the cluster — gateway routing, TLS termination, authentication, and the public API
surface as a real user would experience it.

---

## Complete Example — Internal Plot

```json
{
  "name": "Poll Creation Flow",
  "version": "v1",
  "description": "Verifies the poll creation path across poll-builder and poll-wr4ngler.  Creates a poll in poll-builder, retrieves it by ID to confirm persistence, then confirms poll-wr4ngler can list it by owner.  Exercises the boundary between the poll definition layer and the vote mechanics layer without requiring auth.",
  "constellation": "tca-vox",
  "execution_mode": "internal",
  "context": {
    "owner_id": "test-owner-plot-ci"
  },
  "steps": [
    {
      "step": 1,
      "service": "poll-builder",
      "method": "POST",
      "path": "/polls",
      "body": {
        "title": "Plot Test Poll",
        "description": "Created by SETI plot test",
        "owner_id": "{owner_id}",
        "options": ["Option A", "Option B"]
      },
      "expected_status": 201,
      "expected_fields": ["id", "title"],
      "extract_fields": {
        "poll_id": "id"
      },
      "stop_on_failure": true,
      "notes": "Create poll and capture ID for subsequent steps"
    },
    {
      "step": 2,
      "service": "poll-builder",
      "method": "GET",
      "path": "/polls/{poll_id}",
      "expected_status": 200,
      "expected_fields": ["id", "title", "options"],
      "notes": "Confirm poll persisted and is retrievable by ID"
    },
    {
      "step": 3,
      "service": "poll-wr4ngler",
      "method": "GET",
      "path": "/polls?ownerId={owner_id}",
      "expected_status": 200,
      "notes": "Confirm poll-wr4ngler can list polls by owner across service boundary"
    }
  ]
}
```

---

## Where Plot Files Live

Plots are stored in the constellation's GitHub repository under `contracts/plots/`.
One file per plot, named descriptively:

```
contracts/
  plots/
    poll-creation-flow.json
    user-registration-flow.json
    vote-submission-flow.json
```

SETI's connie-agent fetches this directory on each sync cycle using the registry
token configured in `remote-apps-configmap.yaml`.  New plots appear in the SETI UI
within one sync cycle (default 60 minutes, or on connie-agent restart).

---

## Running Plots

**Single plot:**  Click `> Run` on the plot card in the Plots page.

**All plots:**  Click `>> Run All (N)` to run every plot for the active constellation
sequentially.

**Results:**  Expand the plot card to see step-by-step results with actual vs expected
status codes, response bodies, and captured variable values.  Failed steps show the
failure reason inline.

A downloadable text report is available after a Run All via the `↓ Report` button.

---

## Guidelines for Writing Good Plots

**Keep plots focused.**  One plot = one user flow.  Don't try to test everything
in a single plot.  A plot that does five unrelated things is hard to debug when it fails.

**Use stop_on_failure wisely.**  If step 2 depends on data created in step 1,
set `stop_on_failure: true` on step 1.  There's no point running step 2 if the
poll_id was never captured.

**Use descriptive notes.**  The `notes` field appears in failure reports.  A good
note tells the Wr4ngler what the step is supposed to prove, not just what it does.

**Don't use production data.**  Use clearly synthetic values for owner IDs, titles,
and other identifiers.  Prefix with `plot-ci-` or similar so test data is identifiable.

**Test the boundary, not the implementation.**  A plot should verify observable
behavior — status codes, response shape, cross-service data consistency.  It should
not verify internal implementation details that could change without breaking the
user flow.

---

*Document maintained by the SETI Connie Wr4ngler.*  
*For architecture questions, see `SETI-GUIDELINES.md` and `AC-GUIDELINES.md`.*
