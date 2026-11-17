# Configuration

NERV reads configuration from environment variables. The Kubernetes manifests load non-secret values from `nerv-config` and secrets from `nerv-secrets`.

| Variable | Default | Purpose |
| --- | --- | --- |
| `NERV_ADDR` | `127.0.0.1:8080` | HTTP listen address |
| `NERV_AUTH_TOKEN` | empty | Bearer token for protected API routes and operator UI data calls; set it in private/shared deployments |
| `DATABASE_URL` | required | PostgreSQL URL used by DBOS and NERV tables |
| `KUBECONFIG` | standard client-go lookup | Local Kubernetes configuration; in-cluster auth is preferred |
| `NERV_LLM_PROVIDER` | `agentgateway` | LLM provider name; use `echo` for local tests |
| `NERV_LLM_MODEL` | `openai/gpt-5-mini` | Primary model sent to Agentgateway |
| `NERV_LLM_FALLBACK_MODEL` | empty | Optional fallback model; disabled by default and used only after retryable primary provider/transport failures |
| `NERV_AGENTGATEWAY_URL` | empty | OpenAI-compatible Agentgateway base URL |
| `NERV_AGENTGATEWAY_TOKEN` | empty | Optional Agentgateway caller token |
| `NERV_DECISION_GATEWAY_URL` | empty | Decision Gateway base URL; empty uses local investigate policy |
| `NERV_DECISION_GATEWAY_TOKEN` | empty | Optional Decision Gateway caller token |
| `NERV_DECISION_NAME` | `alert-disposition` | Decision contract name |
| `NERV_DECISION_VERSION` | `v1` | Decision contract version |
| `NERV_GOTIFY_URL` | empty | Gotify base URL; empty disables delivery |
| `NERV_GOTIFY_TOKEN` | empty | Gotify application token |
| `NERV_HTTP_TIMEOUT` | `30s` | General outbound HTTP timeout |
| `NERV_AGENT_MAX_ROUNDS` | `4` | Maximum model rounds per investigation |
| `NERV_AGENT_MAX_TOOL_CALLS` | `6` | Maximum requested tool calls; rejected calls consume budget |
| `NERV_AGENT_TIMEOUT` | `75s` | Total agent-loop wall-clock budget |
| `NERV_AGENT_MAX_TOKENS` | `12000` | Aggregate known input and output token budget across rounds/fallback |
| `NERV_AGENT_TOOL_CONCURRENCY` | `2` | Maximum concurrent durable tool steps |
| `NERV_AGENT_MODEL_TIMEOUT` | `30s` | Per model-attempt timeout |
| `NERV_AGENT_TOOL_TIMEOUT` | `30s` | Per tool-execution timeout |
| `NERV_AGENT_MAX_EVIDENCE_BYTES` | `32768` | Aggregate persisted agent-tool evidence byte limit |
| `NERV_AGENT_MAX_TOOL_OUTPUT_BYTES` | `32768` | Maximum output bytes accepted from one tool |
| `NERV_AGENT_MAX_COST_USD` | `0` | Optional known-cost ceiling; zero disables cost ceiling |
| `NERV_AGENT_INPUT_PRICE_PER_MILLION_TOKENS_USD` | `0` | Input-token price used for cost accounting |
| `NERV_AGENT_OUTPUT_PRICE_PER_MILLION_TOKENS_USD` | `0` | Output-token price used for cost accounting |
| `NERV_DETECTION_ENABLED` | `false` | Run autonomous Kubernetes detection with `serve`; the base manifest enables it explicitly |
| `NERV_DETECTION_PERSISTENCE` | `2m` | Time a deterministic failure must remain present before workflow acceptance |
| `NERV_DETECTION_COOLDOWN` | `30m` | Minimum interval before a recovered failure identity can open another investigation |
| `NERV_DETECTION_RESYNC` | `30s` | Informer resync interval used for persistence checks and reconnect reconciliation |
| `NERV_DETECTION_WORKERS` | `2` | Bounded detector worker count |
| `NERV_DETECTION_QUEUE_CAPACITY` | `512` | Maximum queued resource reconciliations; later resync recovers dropped events |
| `NERV_DETECTION_RESTART_THRESHOLD` | `3` | Unexpected container restart count that qualifies as a failure condition |
| `NERV_DETECTION_RESTART_WINDOW` | `10m` | Only count an unexpected restart when its latest termination finished within this window |
| `NERV_DETECTION_NAMESPACES` | empty (all) | Comma-separated namespace allowlist; the base manifest starts with `nerv` only |
| `NERV_DETECTION_MAINTENANCE_ANNOTATION` | `nerv.io/maintenance` | Resource annotation that suppresses detection when set to `true` or present with an empty value |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | empty | OTLP gRPC host and port; empty disables HTTP, service, and GenAI trace export |
| `OTEL_EXPORTER_OTLP_INSECURE` | `true` | Use plaintext OTLP gRPC |

Agent limits must be positive and within built-in safety caps. Startup fails on invalid values. Configure both price variables together; configuring a cost ceiling without both prices also fails startup. When prices are absent, token limits still apply and findings report cost accounting as `unknown_disabled` rather than inventing a cost.

## Autonomous detection behavior

`serve` starts shared informers for Pods, Deployments, StatefulSets, and DaemonSets when detection is explicitly enabled. Initial informer lists reconcile resources that were unhealthy before NERV started. Periodic resyncs re-evaluate persistence without relying on Kubernetes Events and recover state after watch reconnects. Set `NERV_DETECTION_NAMESPACES` before enabling detection in a shared cluster; an empty value watches every namespace.

A condition must remain continuously present for `NERV_DETECTION_PERSISTENCE`. Recovery before that threshold clears it. One active condition opens one workflow; repeated updates and relists are suppressed. Completed incident correlations remain in cooldown for `NERV_DETECTION_COOLDOWN` before they can resolve to a new incident. NERV and recognized external alerts use the same stable PostgreSQL correlation key, so overlapping intake returns the existing DBOS workflow even across watcher restarts.

Repeated restart detection ignores historical restart counts unless the latest unexpected termination occurred inside `NERV_DETECTION_RESTART_WINDOW`. This prevents a rollout or old node disruption from opening incidents indefinitely.

Set the configured maintenance annotation on a Pod or workload to suppress autonomous detection for that resource. Detection resumes after annotation removal and persistence threshold elapses. NERV only reads annotations; it never creates or changes them.

## Operator UI authentication

The embedded UI is available at `/ui/`. Its static shell can load without credentials, but incident reads and `/ask` submissions call protected NERV APIs. The operator enters `NERV_AUTH_TOKEN` in the browser; the UI stores it only in per-tab `sessionStorage` with a 30-minute expiry and sends it as a bearer token. NERV does not render the token into HTML or JavaScript. Keep the service private and set `NERV_AUTH_TOKEN` outside isolated local development.

## Local host configuration

`.env.example` uses the echo provider and local Docker PostgreSQL:

```bash
cp .env.example .env
set -a; source .env; set +a
```

## Kubernetes secrets

The base includes `deploy/base/secret.example.yaml` so Kustomize can render without external dependencies. Its values are placeholders.

For a shared cluster, remove that resource from the production overlay and create `nerv-secrets` with External Secrets or another secret controller. Required secret keys:

```text
NERV_AUTH_TOKEN
DATABASE_URL
NERV_GOTIFY_TOKEN
```

Agentgateway and Decision Gateway tokens are optional when those internal routes do not authenticate callers.

## Current homelab service addresses

The base ConfigMap uses these internal services:

```text
http://agentgateway-proxy.agentgateway.svc.cluster.local
http://decision-gateway.decision-gateway.svc.cluster.local:8080
http://gotify-svc.gotify.svc.cluster.local
postgres.chi.svc.cluster.local:5432
signoz-signoz-helm-otel-collector.signoz.svc.cluster.local:4317
```

A different cluster should patch these values instead of editing application code.

## Telemetry content policy

NERV emits GenAI operation metadata such as model names, token counts, finish reasons, and errors. It does not export prompts, Kubernetes evidence, model responses, system instructions, or tool arguments/results. Content capture must remain opt-in if added later and must apply redaction and size limits before export.
