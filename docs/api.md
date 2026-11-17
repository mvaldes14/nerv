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

Starts an investigation and waits for the finding. The HTTP request has a 60-second timeout.

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

Only `question` is required. Without a target, NERV cannot collect Kubernetes evidence. `/ask` represents explicit operator intent, so it bypasses Decision Gateway and receives a local `investigate` decision with provider `nerv.local` and no external request ID. The CLI `ask` command uses the same decision path.

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

Evidence/tool output is returned separately from the finding. `confidence` defaults to `unknown` because NERV does not infer confidence when the model or policy has not supplied one. Decision metadata remains backward-compatible under `finding.decision`: `action`, `reason`, `review_required`, `provider`, and `request_id` are present when available.

For `suppress`, NERV does not call Agentgateway. It persists a human-readable policy outcome with the reason and available provider/request metadata, and the UI labels it **Policy outcome** rather than model-generated output. A remote `duplicate` classification does not prove that another incident exists. Only PostgreSQL/DBOS correlation can reuse an incident, and NERV never fabricates an incident link from decision metadata.

## Operator UI

### `GET /ui/`

Serves the embedded server-rendered shell, CSS, and minimal vanilla JavaScript from the same Go binary. Enter the bearer token in the UI; it is stored only in browser `sessionStorage` with a 30-minute expiry and sent in the `Authorization` header for `/ask` and investigation API requests. It is never inserted into HTML, JavaScript, URLs, or server logs.

The UI talks only to NERV HTTP endpoints. It has no direct PostgreSQL, Kubernetes, SigNoz, or ClickHouse access. Richer filtering, Gotify deep links, and remediation approval controls are deferred.

## Errors

Errors use a small JSON envelope:

```json
{"error":"question is required"}
```

Common status codes:

| Status | Meaning |
| --- | --- |
| `400` | Invalid JSON, missing question, or empty alert list |
| `401` | Missing or invalid bearer token |
| `404` | Investigation ID not found |
| `500` | Investigation failed while `/ask` was waiting |
| `503` | Alert could not be durably accepted, or readiness failed |
