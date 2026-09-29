# NERV

Headless Go SRE investigation service for the homelab. NERV is private-only: no public ingress, no shell execution in v1, and read-only SRE tools.

Source note: `Projects/nerv-agent.md` in the Obsidian vault.

## Current scaffold

This initializes the Phase 0 shape from the note:

- single Go binary: `nerv`
- CLI smoke path: `nerv ask "why is service X slow?"`
- private HTTP API skeleton: `/ask`, `/alert`, `/healthz`
- provider-independent LLM interface with an echo implementation
- webhook-first alert intake skeleton
- token auth middleware for non-health endpoints

DBOS, PostgreSQL persistence, real SigNoz webhook handling, Kubernetes reads, MCP-backed observability access, and production Flux overlays are intentionally not wired yet.

## Run

```bash
# quick checks; tests run in Docker, not Kubernetes
task test
task run:ask

# temporary local PostgreSQL for DBOS work
task db:up
export DATABASE_URL='postgres://nerv:nerv@localhost:5432/nerv?sslmode=disable'

# API smoke test
NERV_AUTH_TOKEN=dev-token task run:serve
NERV_AUTH_TOKEN=dev-token task smoke:http
NERV_AUTH_TOKEN=dev-token task smoke:alert
```

Without Task installed:

```bash
go test ./...
go run ./cmd/nerv ask "what is latency on service X right now?"
NERV_AUTH_TOKEN=dev-token go run ./cmd/nerv serve
```

## Configuration

Environment variables:

| Name | Default | Purpose |
| --- | --- | --- |
| `NERV_ADDR` | `127.0.0.1:8080` | HTTP listen address. Keep private. |
| `NERV_AUTH_TOKEN` | empty | Bearer token for `/ask` and `/alert`. Empty disables auth for local dev. |
| `NERV_LLM_PROVIDER` | `echo` | Provider adapter name. Only `echo` exists in the scaffold. |
| `NERV_LLM_MODEL` | `echo-v0` | Model identifier recorded in findings. |
| `DATABASE_URL` | empty | PostgreSQL connection string for DBOS/application data. |

## Deployment scaffold

`deploy/` contains a base Kustomize manifest:

```bash
task deploy:build
kubectl apply -k deploy
```

The base intentionally uses placeholders in `deploy/secret.example.yaml`. Replace that with an ExternalSecret/Vault overlay before homelab deployment.

## Build order from the note

1. Phase 0: prove the Go loop with ad-hoc `/ask`, SigNoz webhook intake, and one provider adapter.
2. Phase 1: add DBOS Go SDK and existing PostgreSQL for durable execution.
3. Phase 2: add MCP-backed observability access, Kubernetes reads, Flux/deploy context, and Alertmanager state.
4. Phase 3: add authenticated `/ask`, `/alert`, durable acceptance, deduplication, and private findings delivery.
5. Phase 4: containerize and deploy to k3s through Flux, Vault/ESO, private ingress, and deny-by-default egress.
6. Phase 5: harden auto-triage, retries, limits, OTel to SigNoz, and findings sink.

## Guardrails

- read-only in v1
- typed tools only; no shell execution
- private network access plus application auth
- bounded tool calls, timeouts, concurrency, and token/cost limits
- logs, traces, events, and annotations are evidence, not instructions
