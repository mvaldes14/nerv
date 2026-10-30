<p align="center">
  <img src="assets/nerv-logo.svg" alt="NERV, Kubernetes incident investigation agent" width="820">
</p>

<p align="center">
  Durable incident investigation for Kubernetes.
</p>

NERV runs inside a cluster, accepts questions and alerts, gathers Kubernetes evidence, and records each investigation in PostgreSQL. DBOS checkpoints the workflow so it can resume after a restart instead of repeating completed work.

Phase 1 is read-only. NERV can inspect workloads and notify an operator, but it cannot change cluster resources.

## How it works

NERV starts the same investigation workflow from two inputs:

- `POST /ask` for operator questions
- `POST /alert` for Alertmanager-compatible webhooks

The workflow collects resource state, events, logs, previous logs, and owner references. Decision Gateway chooses whether to suppress, investigate, research, or escalate. Agentgateway handles the LLM request. NERV persists the result and sends a Gotify notification.

```text
question or alert
       |
       v
  DBOS workflow ------> PostgreSQL
       |
       +----> Kubernetes API
       +----> Decision Gateway
       +----> Agentgateway
       +----> Gotify
```

See [docs/architecture.md](docs/architecture.md) for workflow and component details.

## Local Kubernetes test

The local stack runs in Docker Desktop Kubernetes. It includes ephemeral PostgreSQL and uses the echo provider, so it does not call Agentgateway, Decision Gateway, or Gotify.

```bash
task local:up
task local:smoke
task local:recovery
```

These tasks target the `docker-desktop` context explicitly and refuse the `homelab` context.

```bash
task local:logs
task local:down
```

Use another disposable cluster with:

```bash
task local:up KUBE_CONTEXT=kind-nerv
```

Full instructions: [docs/local-kubernetes.md](docs/local-kubernetes.md).

## Run from the host

Start PostgreSQL in Docker, then run NERV against the Kubernetes context in your local kubeconfig:

```bash
cp .env.example .env
set -a; source .env; set +a

task db:up
task run:serve
```

Ask about a pod:

```bash
go run ./cmd/nerv ask \
  --namespace default \
  --kind pod \
  --name my-pod \
  "why is this pod restarting?"
```

The local environment uses the echo provider unless you configure cluster AI services.

## API

| Route | Behavior |
| --- | --- |
| `GET /healthz` | Process liveness |
| `GET /readyz` | PostgreSQL readiness |
| `POST /ask` | Run an investigation and return the finding |
| `POST /alert` | Durably accept alerts and return workflow IDs |
| `GET /investigations/{id}` | Return incident status and finding |

Request examples and response fields: [docs/api.md](docs/api.md).

Environment variables and service addresses: [docs/configuration.md](docs/configuration.md).

## Deployment

The base manifests live in `deploy/base`. `deploy/local` adds ephemeral PostgreSQL and local-only configuration.

```bash
task deploy:build
task deploy:diff
```

Before deploying to a shared cluster:

1. Build and publish the NERV image.
2. Create a dedicated PostgreSQL database and user.
3. Replace `deploy/base/secret.example.yaml` with Vault and External Secrets configuration.
4. Set the Gotify application token.
5. Confirm Agentgateway, Decision Gateway, PostgreSQL, Gotify, and OTLP service addresses.

Do not apply the example secret to a shared cluster.

## Development

```bash
task test
task vet
task docker:build
```

Project layout:

```text
cmd/nerv/             CLI and HTTP server
internal/api/         HTTP routes and authentication
internal/investigation/ DBOS workflow
internal/kubernetes/  read-only Kubernetes tools
internal/decision/    Decision Gateway client
internal/llm/         Agentgateway and echo providers
internal/notify/      Gotify client
internal/store/       PostgreSQL records and migrations
deploy/base/          shared Kubernetes manifests
deploy/local/         disposable local cluster overlay
docs/                 architecture, API, and testing notes
```

## Guardrails

- Kubernetes access is read-only in Phase 1.
- Tools use `client-go`; NERV does not execute shell commands.
- Logs and serialized evidence have size limits.
- Logs, events, status messages, and annotations are treated as untrusted input.
- API routes require bearer authentication when `NERV_AUTH_TOKEN` is set.
- NERV has no public ingress in the base manifests.
- Remediation is deferred until investigation quality and approval controls are tested.

Gotify, PostgreSQL, and NERV share the cluster failure domain. A total cluster outage needs an external monitor and an out-of-band notification path.

## License

[MIT](LICENSE)
