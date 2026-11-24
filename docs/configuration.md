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
| `NERV_NOTIFICATION_DEBOUNCE` | `2m` | Quiet period before one digest is sent for a burst of autonomous detector incidents |
| `NERV_HTTP_TIMEOUT` | `30s` | General outbound HTTP timeout |
| `NERV_GITOPS_REPOSITORY` | empty | Trusted `owner/repository` copied into remediation proposals; configure with targets |
| `NERV_GITOPS_TARGETS_JSON` | empty | Trusted target allowlist containing IDs, relative YAML paths, and exact apps/v1 workload identities |
| `NERV_GITHUB_APP_ID` | empty | GitHub App ID; configure together with installation ID and private key |
| `NERV_GITHUB_APP_INSTALLATION_ID` | empty | Installation ID scoped to the trusted repository |
| `NERV_GITHUB_APP_PRIVATE_KEY` | empty | RSA private key PEM; secret value supplied through Vault/ESO |
| `NERV_GITHUB_BASE_BRANCH` | `main` | Base branch used for remediation pull requests |
| `NERV_GITHUB_API_URL` | `https://api.github.com` | GitHub REST API endpoint |
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
| `NERV_ADMISSION_MAX_CONCURRENT` | `2` | Maximum active investigation leases shared across replicas |
| `NERV_ADMISSION_LEASE_TTL` | `10m` | Expiry for abandoned concurrency leases; must cover the agent timeout |
| `NERV_ADMISSION_ASK_PER_MINUTE` | `6` | Global fixed-window limit for HTTP and CLI operator questions |
| `NERV_ADMISSION_ALERT_PER_MINUTE` | `30` | Global fixed-window limit for external alert investigations |
| `NERV_ADMISSION_DETECTOR_PER_MINUTE` | `30` | Global fixed-window limit for autonomous detector investigations |
| `NERV_ADMISSION_DAILY_MAX_TOKENS` | `240000` | Settled token usage allowed per UTC day before new work is rejected |
| `NERV_ADMISSION_DAILY_MAX_COST_USD` | `0` | Optional settled daily cost ceiling; zero disables it and nonzero requires model prices |
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
| `OTEL_EXPORTER_OTLP_ENDPOINT` | empty | OTLP gRPC URL; empty disables HTTP, service, and GenAI trace/metric export |
| `OTEL_EXPORTER_OTLP_INSECURE` | `true` | Use plaintext OTLP gRPC |

Agent and admission limits must be positive and within built-in safety caps. Startup fails on invalid values. Configure both price variables together; configuring a per-investigation or daily cost ceiling without both prices also fails startup. When prices are absent, token limits still apply and findings report cost accounting as `unknown_disabled` rather than inventing a cost.

## GitOps remediation proposal configuration

Both GitOps variables must be empty (feature disabled) or configured together. Target IDs must be unique. Paths must be clean relative `.yaml` or `.yml` paths: absolute paths, `..`, backslashes, and empty path segments are rejected. Resources are limited to `apps/v1` `Deployment` and `StatefulSet` identities.

```bash
NERV_GITOPS_REPOSITORY=mvaldes14/k8s-apps
NERV_GITOPS_TARGETS_JSON='[{"id":"checkout","path":"apps/checkout/deployment.yaml","resource":{"api_version":"apps/v1","kind":"Deployment","namespace":"apps","name":"checkout"}}]'
NERV_GITHUB_APP_ID=5905005
NERV_GITHUB_APP_INSTALLATION_ID=185399975
NERV_GITHUB_APP_PRIVATE_KEY='-----BEGIN RSA PRIVATE KEY-----...'
NERV_GITHUB_BASE_BRANCH=main
```

Requests select only configured `target_id`; repository, path, resource identity, raw patches, commands, URLs, file content, and caller-rendered diffs are never accepted. Proposals store deterministic manifest projections, unified diffs, evidence snapshots, and approval/rejection state in PostgreSQL. Approval alone changes only PostgreSQL state. An explicit submit action then verifies the base-branch manifest still matches `expected_state`, creates a deterministic branch, changes only the allowlisted resource's `spec.replicas`, commits, and opens a pull request. NERV cannot merge the PR and retains read-only Kubernetes RBAC.

## Durable admission behavior

PostgreSQL serializes admission decisions across replicas. Per-source rate windows, expiring concurrency leases, and settled UTC-day token/cost usage bound new investigations. Duplicate requests resolving to an existing workflow do not consume another rate-limit unit or concurrency lease. Abandoned leases expire after `NERV_ADMISSION_LEASE_TTL`; successfully completed workflows release immediately.

HTTP `/ask` and `/alert` return `429 Too Many Requests` with `Retry-After` when admission is denied. Autonomous detector acceptance failures remain retryable through informer resync. Daily checks use completed finding budgets, so bounded in-flight work can overshoot the configured daily ceiling by at most the global concurrency limit multiplied by the per-investigation budget.

Operational metrics include `nerv.admission.requests`, `nerv.admission.active`, `nerv.admission.daily_tokens`, `nerv.admission.daily_cost`, `nerv.detector.queue.depth`, `nerv.detector.queue.dropped`, and `nerv.detector.queue.wait_time`.

## Autonomous detection behavior

`serve` starts shared informers for Pods, Deployments, StatefulSets, and DaemonSets when detection is explicitly enabled. Initial informer lists reconcile resources that were unhealthy before NERV started. Periodic resyncs re-evaluate persistence without relying on Kubernetes Events and recover state after watch reconnects. Set `NERV_DETECTION_NAMESPACES` before enabling detection in a shared cluster; an empty value watches every namespace.

A condition must remain continuously present for `NERV_DETECTION_PERSISTENCE`. Recovery before that threshold clears it. One active condition opens one workflow; repeated updates and relists are suppressed. Completed incident correlations remain in cooldown for `NERV_DETECTION_COOLDOWN` before they can resolve to a new incident. NERV and recognized external alerts use the same stable PostgreSQL correlation key, so overlapping intake returns the existing DBOS workflow even across watcher restarts.

Repeated restart detection ignores historical restart counts unless the latest unexpected termination occurred inside `NERV_DETECTION_RESTART_WINDOW`. This prevents a rollout or old node disruption from opening incidents indefinitely.

Autonomous detector findings keep separate incident records, but Gotify delivery uses a sliding `NERV_NOTIFICATION_DEBOUNCE` quiet period. Related findings arriving during that period extend the deadline and produce one cluster-level digest instead of one notification per workload. Operator questions and external alerts remain immediate.

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

NERV pins emitted telemetry to OpenTelemetry semantic conventions `1.37.0`. It exports traces plus the `gen_ai.client.token.usage` histogram, with `gen_ai.token.type=input|output`, through the configured OTLP gRPC endpoint.

NERV emits GenAI operation metadata such as model names, token counts, finish reasons, and errors. Content capture is disabled in code: it does not export prompts, Kubernetes evidence, model responses, system instructions, or tool arguments/results. Any future content capture requires an explicit security review, opt-in configuration, redaction, and size limits before export.
