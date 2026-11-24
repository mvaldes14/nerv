# HTTP API

NERV serves JSON over HTTP. All data routes require a bearer token when `NERV_AUTH_TOKEN` is set. `/healthz`, `/readyz`, and the static `/ui/` shell do not require credentials; every API call made by the UI uses the bearer token.

```http
Authorization: Bearer <token>
Content-Type: application/json
```

## Health

### `GET /healthz`

Returns `200` while the process is running.

```json
{"status":"ok"}
```

### `GET /readyz`

Checks PostgreSQL with a two-second timeout. It returns `503` when PostgreSQL cannot be reached.

```json
{"status":"ready"}
```

## Ask

### `POST /ask`

Starts an investigation and waits for finding. Server wait timeout is agent budget plus 15 seconds (90 seconds with defaults).

```json
{
  "question": "why is this pod restarting?",
  "idempotency_key": "optional-caller-key",
  "target": {
    "namespace": "apps",
    "kind": "pod",
    "name": "checkout-abc",
    "container": "checkout"
  }
}
```

Only `question` is required; it is limited to 4,000 characters, and target fields are limited to 253 characters. Without a target, bounded agent loop can discover relevant workloads with approved all-namespace or namespace-scoped list tools. `/ask` represents explicit operator intent, so it bypasses Decision Gateway and receives local `investigate` decision with provider `nerv.local` and no external request ID. CLI `ask` command uses same decision path.

```bash
curl -sS http://127.0.0.1:8080/ask \
  -H 'Authorization: Bearer dev-token' \
  -H 'Content-Type: application/json' \
  -d '{
    "question":"why is this pod restarting?",
    "target":{"namespace":"apps","kind":"pod","name":"checkout-abc"}
  }' | jq
```

Example response:

```json
{
  "id": "3a0cc572-879d-4c5d-b53f-9e84a75b8450",
  "workflow_id": "incident-3a0cc572-879d-4c5d-b53f-9e84a75b8450",
  "question": "why is this pod restarting?",
  "answer": "...",
  "evidence": [
    {"source":"kubernetes.describe","summary":"..."},
    {"source":"kubernetes.events","summary":"..."}
  ],
  "decision": {
    "action": "investigate",
    "reason": "explicit operator investigation request",
    "provider": "nerv.local"
  },
  "confidence": "high",
  "uncertainty": [],
  "recommended_action": "Continue monitoring Vault readiness.",
  "tools_used": ["list_workloads", "describe_resource"],
  "budget": {
    "max_rounds": 4,
    "rounds_used": 3,
    "max_tool_calls": 6,
    "tool_calls_used": 2,
    "max_tokens": 12000,
    "total_tokens": 1840,
    "cost_accounting": "unknown_disabled"
  },
  "provider": "agentgateway",
  "model": "openai/gpt-5-mini",
  "created_at": "2026-10-30T22:52:25Z"
}
```

## Alert intake

### `POST /alert`

Accepts an Alertmanager-compatible webhook. NERV starts one workflow for each item in `alerts` and returns `202` after DBOS accepts them.

```bash
curl -sS http://127.0.0.1:8080/alert \
  -H 'Authorization: Bearer dev-token' \
  -H 'Content-Type: application/json' \
  --data-binary @testdata/signoz-alert.sample.json | jq
```

Example response:

```json
{
  "status": "accepted",
  "receiver": "nerv",
  "investigations": [
    {
      "id": "8f76c07a5f2bd8f824dd6c13d5d0c451",
      "workflow_id": "incident-8f76c07a5f2bd8f824dd6c13d5d0c451",
      "status": "accepted"
    }
  ]
}
```

NERV derives the incident ID from the alert fingerprint. Replaying the same fingerprint returns the existing workflow. Alert requests use the configured Decision Gateway and retain the documented disposition mapping. Autonomous `kubernetes.detector` requests use that same remote path.

## Incident history

### `GET /investigations/filter-options`

Returns known and persisted values for `sources`, `statuses`, `namespaces`, and `workloads`. Operator UI uses these values for dropdown filters.

### `GET /investigations`

Returns incidents newest first. `limit` defaults to 50 and is capped at 100. `offset` is capped at 10000. Optional exact-match filters are `source`, `status`, `namespace`, and `workload` (target name).

```bash
curl -sS 'http://127.0.0.1:8080/investigations?limit=25&status=completed&namespace=apps' \
  -H 'Authorization: Bearer dev-token' | jq
```

```json
{
  "items": [
    {
      "id": "8f76c07a5f2bd8f824dd6c13d5d0c451",
      "workflow_id": "incident-8f76c07a5f2bd8f824dd6c13d5d0c451",
      "source": "kubernetes.detector",
      "question": "Investigate autonomous Kubernetes failure...",
      "status": "completed",
      "target": {"namespace":"apps","kind":"deployment","name":"checkout"},
      "created_at": "2026-10-30T22:52:25Z",
      "updated_at": "2026-10-30T22:52:32Z"
    }
  ],
  "limit": 25,
  "offset": 0,
  "has_more": false
}
```

Autonomous detections, alerts, and operator questions share this endpoint and the same detail flow.

## Investigation lookup

### `GET /investigations/{id}`

Returns the product status and any persisted finding.

```bash
curl -sS http://127.0.0.1:8080/investigations/<incident-id> \
  -H 'Authorization: Bearer dev-token' | jq
```

```json
{
  "incident": {
    "id": "8f76c07a5f2bd8f824dd6c13d5d0c451",
    "source": "http.alert",
    "status": "completed",
    "question": "p95 latency exceeded threshold",
    "created_at": "2026-10-30T22:52:25Z",
    "updated_at": "2026-10-30T22:52:32Z"
  },
  "status": "completed",
  "finding": {
    "id": "8f76c07a5f2bd8f824dd6c13d5d0c451",
    "workflow_id": "incident-8f76c07a5f2bd8f824dd6c13d5d0c451",
    "question": "p95 latency exceeded threshold",
    "answer": "Investigation suppressed by policy.\nReason: disposition=duplicate urgency=immediate\nDecision provider: laya\nDecision request ID: req-1\nPolicy decision metadata does not identify or link an existing incident.",
    "evidence": [],
    "decision": {
      "action":"suppress",
      "reason":"disposition=duplicate urgency=immediate",
      "provider":"laya",
      "request_id":"req-1"
    },
    "confidence": "unknown",
    "provider": "laya",
    "model": "",
    "created_at": "2026-10-30T22:52:25Z"
  },
  "evidence": [{"source":"kubernetes.events","summary":"..."}],
  "notifications": [{"sink":"gotify","status":"sent","attempted_at":"2026-10-30T22:52:32Z"}]
}
```

Evidence/tool output is returned separately from finding and is explicitly executed evidence, not suggested follow-up. `confidence`, `uncertainty`, `recommended_action`, `tools_used`, and `budget` expose structured result and accounting. `budget.stop_reason` is present when loop ends on a limit. Cost accounting is `unknown_disabled` unless both configured prices are available. Decision metadata remains backward-compatible under `finding.decision`: `action`, `reason`, `review_required`, `provider`, and `request_id` are present when available.

For `suppress`, NERV does not call Agentgateway. It persists a human-readable policy outcome with the reason and available provider/request metadata, and the UI labels it **Policy outcome** rather than model-generated output. A remote `duplicate` classification does not prove that another incident exists. Only PostgreSQL/DBOS correlation can reuse an incident, and NERV never fabricates an incident link from decision metadata.

## GitOps remediation proposals

All remediation routes use existing bearer authentication. Proposals are linked to completed incidents with findings and copy selected evidence into immutable snapshots. Only configured targets and `set_workload_replicas/v1` are accepted.

### `POST /investigations/{id}/remediation-proposals`

```json
{
  "action_kind": "set_workload_replicas/v1",
  "target_id": "checkout",
  "expected": {"replicas": 2},
  "desired": {"replicas": 4},
  "summary": "Scale checkout for sustained load",
  "rationale": "Persisted metrics show saturation.",
  "risk": {"level": "medium", "detail": "Higher resource use."},
  "evidence_ordinals": [0, 2],
  "ttl_seconds": 86400
}
```

Unknown fields are rejected. Replica values must be 0 through 1000 and differ. At least one valid evidence ordinal is required. TTL defaults to 24 hours and must be 5 minutes through 7 days. Repository, path, resource, patch, command, shell, kubectl, URL, file content, and rendered diff fields are not accepted.

Returns `201` with trusted repository/path/resource, evidence snapshots, deterministic `manifest_projection/v1` before/after text, unified diff, and `pending` status.

### `GET /investigations/{id}/remediation-proposals`

Returns up to 50 proposals newest first:

```json
{"items": []}
```

### `GET /remediation-proposals`

Returns the operator remediation queue across incidents, with pending proposals first. Optional `status=pending|approved|rejected|expired` filters the queue.

### `GET /remediation-proposals/{id}`

Returns one proposal. Reads lazily persist `expired` when database time reaches `expires_at`.

### `POST /remediation-proposals/{id}/approve`
### `POST /remediation-proposals/{id}/reject`

```json
{"actor":"alice","comment":"Reviewed evidence and target."}
```

One atomic transition from `pending` wins. Terminal or expired proposals return `409`; missing proposals return `404`. `decision_auth_subject` is persisted as `shared-bearer`. Approval and rejection only update PostgreSQL state.

### `POST /remediation-proposals/{id}/submit`

Requires an approved proposal and configured GitHub App. Submission atomically claims the proposal, reads the allowlisted file from the configured base branch, verifies the exact resource identity and expected replica count, creates a deterministic `nerv/remediation-*` branch, changes only `spec.replicas`, commits, and opens a pull request. The response persists `submission_status`, branch, commit SHA, PR number, and PR URL. Failed submissions persist a bounded error and may be retried. Already-submitted proposals return their existing PR data.

### `POST /remediation-proposals/{id}/refresh`

Polls GitHub for a submitted pull request and persists its open/closed state, merged flag, close/merge timestamps, and last checked time. Operator UI automatically refreshes open PRs when remediation queue loads and also provides a manual refresh action. No webhook is required.

NERV never merges the pull request, pushes to the base branch, calls Flux mutation APIs, or writes Kubernetes resources. Repository drift returns `409` rather than overwriting unexpected state.

## Operator UI

### `GET /ui/`

Serves embedded HTML, CSS, and vanilla JavaScript from the same Go binary. The initial screen is an authentication gate; operator panels remain hidden until `GET /auth/check` validates the bearer token. The token is stored only in browser `sessionStorage` with a 30-minute expiry and is never inserted into HTML, JavaScript, URLs, or server logs. Static UI assets are not a security boundary; every data and mutation API remains bearer-protected.

The authenticated console has separate Ask, Incidents, and Remediation tabs. Ask presents a conversational browser-session thread with one freeform question field. Each question remains a separate durable investigation; structured targets remain supported by API clients but are not required in operator UI. Incident filters are dropdowns populated from known source/status values and distinct persisted namespaces/workloads. Remediation queue exposes evidence, deterministic diff, approval/rejection controls, submission and PR lifecycle status, and PR URL without requiring operators to find originating incident first. Approval and PR submission require separate explicit browser confirmations. The UI has no direct PostgreSQL, Kubernetes, SigNoz, ClickHouse, Git, or GitHub access.

## Errors

Errors use a small JSON envelope:

```json
{"error":"question is required"}
```

Common status codes:

| Status | Meaning |
| --- | --- |
| `400` | Invalid JSON, validation failure, missing question, or empty alert list |
| `401` | Missing or invalid bearer token |
| `404` | Investigation or remediation proposal ID not found |
| `409` | Incident not completed with finding, proposal state conflict, concurrent submission, or repository drift |
| `429` | Durable rate, concurrency, daily token, or daily spend limit rejected new work; response includes `Retry-After` |
| `500` | Investigation failed while `/ask` was waiting |
| `503` | Alert could not be durably accepted, or readiness failed |
