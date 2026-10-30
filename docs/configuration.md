# Configuration

NERV reads configuration from environment variables. The Kubernetes manifests load non-secret values from `nerv-config` and secrets from `nerv-secrets`.

| Variable | Default | Purpose |
| --- | --- | --- |
| `NERV_ADDR` | `127.0.0.1:8080` | HTTP listen address |
| `NERV_AUTH_TOKEN` | empty | Bearer token for protected API routes |
| `DATABASE_URL` | required | PostgreSQL URL used by DBOS and NERV tables |
| `KUBECONFIG` | standard client-go lookup | Local Kubernetes configuration; in-cluster auth is preferred |
| `NERV_LLM_PROVIDER` | `agentgateway` | LLM provider name; use `echo` for local tests |
| `NERV_LLM_MODEL` | `openai/gpt-5-mini` | Model sent to Agentgateway |
| `NERV_AGENTGATEWAY_URL` | empty | OpenAI-compatible Agentgateway base URL |
| `NERV_AGENTGATEWAY_TOKEN` | empty | Optional Agentgateway caller token |
| `NERV_DECISION_GATEWAY_URL` | empty | Decision Gateway base URL; empty uses local investigate policy |
| `NERV_DECISION_GATEWAY_TOKEN` | empty | Optional Decision Gateway caller token |
| `NERV_DECISION_NAME` | `alert-disposition` | Decision contract name |
| `NERV_DECISION_VERSION` | `v1` | Decision contract version |
| `NERV_GOTIFY_URL` | empty | Gotify base URL; empty disables delivery |
| `NERV_GOTIFY_TOKEN` | empty | Gotify application token |
| `NERV_HTTP_TIMEOUT` | `30s` | Outbound HTTP timeout |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | empty | OTLP gRPC host and port; empty disables trace export |
| `OTEL_EXPORTER_OTLP_INSECURE` | `true` | Use plaintext OTLP gRPC |

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
