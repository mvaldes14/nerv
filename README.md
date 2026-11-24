<p align="center">
  <img src="assets/nerv-logo.svg" alt="NERV, Kubernetes incident investigation agent" width="820">
</p>

<p align="center">
  Durable incident investigation for Kubernetes.
</p>

NERV runs inside a cluster, accepts questions and alerts, autonomously detects sustained Kubernetes failures, gathers evidence, and records each investigation in PostgreSQL. DBOS checkpoints the workflow so it can resume after a restart instead of repeating completed work.

NERV keeps Kubernetes access read-only. It can investigate workloads, notify operators, record human-approved GitOps remediation proposals, and use a least-privilege GitHub App to create remediation branches, commits, and pull requests. It cannot merge PRs or mutate cluster resources directly.

## How it works

NERV starts the same investigation workflow from two inputs:

- `POST /ask` for operator questions
- `POST /alert` for Alertmanager-compatible webhooks
- client-go informers for sustained Pod and workload failures
- the embedded private operator UI at `/ui/`

For non-suppressed work, bounded durable agent loop lets model discover workloads and choose approved typed read-only tools for status, events, logs, owner chains, and nodes. Operator questions receive local `investigate` decision; alerts and autonomous detections use Decision Gateway to choose whether to suppress, investigate, research, or escalate. Every model round and tool execution is separate DBOS checkpoint. NERV persists executed evidence, structured finding, budget accounting, and Gotify notification.

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
       +----> GitHub App (approved remediation PRs)
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

Open `http://127.0.0.1:8080/ui/` and enter `NERV_AUTH_TOKEN`. Operator panels remain hidden until token validation succeeds. Console separates freeform Ask, incident history, and remediation review into tabs. UI keeps token in browser `sessionStorage` for at most 30 minutes and calls only NERV HTTP APIs.

Ask about a pod from the CLI:

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
| `GET /investigations/filter-options` | Return source, status, namespace, and workload dropdown values |
| `GET /investigations` | List incident history with bounded pagination and filters |
| `GET /investigations/{id}` | Return incident metadata, status, finding, evidence, and notifications |
| `POST/GET /investigations/{id}/remediation-proposals` | Create or list bounded GitOps remediation proposals |
| `GET /remediation-proposals/{id}` | Return proposal, evidence snapshots, deterministic projection, and diff |
| `POST /remediation-proposals/{id}/approve` | Record human approval only |
| `POST /remediation-proposals/{id}/reject` | Record human rejection only |
| `POST /remediation-proposals/{id}/submit` | Create allowlisted GitHub branch, commit, and PR |
| `POST /remediation-proposals/{id}/refresh` | Poll and persist PR open/closed/merged state |
| `GET /ui/` | Embedded operator UI shell (API calls still require bearer authentication) |

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
internal/remediation/ GitOps proposal and approval policy
internal/githubapp/   GitHub App authentication and PR client
internal/store/       PostgreSQL records and migrations
deploy/base/          shared Kubernetes manifests
deploy/local/         disposable local cluster overlay
docs/                 architecture, API, and testing notes
```

## Guardrails

- Kubernetes access is read-only.
- Tools use `client-go`; NERV does not execute shell commands.
- Agent rounds, tool calls, tokens, time, concurrency, logs, evidence, and tool output have validated limits.
- Logs, events, status messages, and annotations are treated as untrusted input.
- Agentgateway fallback is disabled unless `NERV_LLM_FALLBACK_MODEL` is explicitly configured, and only retryable primary provider/transport failures qualify.
- GenAI telemetry records operation, tool, budget, model, finish, and token metadata—not prompts, evidence, model output, or tool payloads.
- API routes require bearer authentication when `NERV_AUTH_TOKEN` is set. Configure it for every private or shared deployment.
- Operator UI stores bearer token in per-tab `sessionStorage` with a 30-minute expiry; token is never embedded in served HTML or JavaScript.
- NERV has no public ingress in the base manifests.
- Approval changes PostgreSQL state only; explicit submission is a separate operator action.
- GitHub App is scoped to selected repositories with `contents:write`, `pull_requests:write`, and `metadata:read`.
- Submission verifies expected repository state, edits one allowlisted workload replica field, and opens a human-reviewed PR.
- NERV has no merge operation, no direct Flux mutation, no Kubernetes write RBAC, and no shell or command executor.
- Submitted PR state is polled and persisted without requiring a webhook. Post-merge Flux reconciliation and cluster-state verification remain the next remediation increment.

Gotify, PostgreSQL, and NERV share the cluster failure domain. A total cluster outage needs an external monitor and an out-of-band notification path.

## License

[MIT](LICENSE)
