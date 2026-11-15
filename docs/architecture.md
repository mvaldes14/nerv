# Architecture

NERV is a Go service that runs inside Kubernetes. It uses DBOS for durable execution and PostgreSQL for workflow state, incident records, evidence, findings, and notification history.

## Components

| Component | Responsibility |
| --- | --- |
| HTTP API | Accept operator questions and Alertmanager-compatible webhooks |
| DBOS | Checkpoint workflow steps and recover interrupted investigations |
| PostgreSQL | Store DBOS state and NERV reporting tables |
| Kubernetes tools | Read pods, workloads, nodes, events, logs, and owner references |
| Decision Gateway | Classify alert disposition and urgency |
| Agentgateway | Route OpenAI-compatible LLM requests |
| Gotify | Deliver investigation findings |
| OpenTelemetry | Export HTTP, service, and GenAI semantic-convention traces |

## Investigation workflow

Both `/ask` and `/alert` start `nerv.investigate`.

1. Normalize the request and assign an incident ID.
2. Start the workflow with a stable DBOS workflow ID.
3. Persist the incident in the `nerv` PostgreSQL schema.
4. Collect Kubernetes evidence when the request includes a target.
5. Persist evidence before calling AI services.
6. Execute the configured Decision Gateway contract.
7. Skip the LLM when the decision is `suppress`.
8. Otherwise, send the question and evidence to Agentgateway.
9. Persist the finding.
10. Send the finding to Gotify.
11. Mark the incident complete.

External calls and database writes run as separate DBOS steps. Recovery reuses completed step results.

## GenAI telemetry

Agentgateway inference emits OpenTelemetry GenAI semantic-convention spans. Each OpenAI-compatible request creates a `chat <model>` client span with operation, provider, requested and returned model, response ID, finish reasons, token usage, server address, and low-cardinality error type when available. The underlying HTTP request is also instrumented for propagation and transport diagnostics.

Prompt text, Kubernetes evidence, model output, system instructions, and tool payloads are not recorded in telemetry. These fields can contain credentials, personal data, or untrusted workload content.

The current deterministic investigation emits an `invoke_workflow NERV` internal span with `execute_tool <tool>` child spans for Kubernetes evidence collection. When the bounded loop allows the model to control tool selection, that agent-controlled section will emit `invoke_agent NERV`. DBOS recovery does not depend on keeping one span open across process restarts: intake trace context is serialized into workflow input, and each execution or recovery attempt links back to the originating intake span.

Telemetry conformance will be checked against a pinned GenAI semantic-convention registry with OpenTelemetry Weaver. Semantic violations should fail the conformance test; coverage reporting remains non-blocking.

## Incident identity

Operator questions receive a new UUID unless the caller supplies `idempotency_key`. Alerts use their fingerprint as the idempotency key. DBOS returns the existing workflow when the same workflow ID already exists.

Current alert deduplication is permanent for a fingerprint. Incident windows and reopening policy belong in the autonomous detection phase.

## Kubernetes evidence

Phase 1 supports these read operations:

- get and list pods
- current and previous container logs
- pod owner chain
- resource summaries for Pods, Deployments, StatefulSets, DaemonSets, and Nodes
- Kubernetes events attached to a resource

Resource summaries exclude environment variables, annotations, managed fields, and full pod specs. This keeps prompts smaller and avoids sending unrelated configuration to the model.

The collector limits each serialized evidence item to 32 KiB and pod logs to the latest 200 lines.

## Decision mapping

NERV calls the deployed `alert-disposition/v1` contract. It maps the returned disposition and urgency to a workflow action:

| Disposition | Urgency | NERV action |
| --- | --- | --- |
| `noisy`, `duplicate`, `informational` | any | `suppress` |
| `actionable` | `immediate` | `escalate` |
| `actionable` | `soon` | `investigate` |
| `actionable` | `backlog` | `research` |
| `actionable` | `none` | `suppress` |

When Decision Gateway is not configured, NERV uses `investigate`.

## Persistence

DBOS creates its system tables in the `dbos` schema. NERV creates product tables in the `nerv` schema:

- `nerv.incidents`
- `nerv.evidence`
- `nerv.findings`
- `nerv.notification_attempts`

NERV runs its schema migration during startup. Readiness fails when PostgreSQL is unavailable.

## Notifications

Gotify delivery is a DBOS step with bounded retries. NERV checks for a recorded successful delivery before sending again. A process failure after Gotify accepts a message but before PostgreSQL records it can still produce a duplicate. Gotify does not provide an idempotency key for this case.

## Security boundary

The Kubernetes ServiceAccount has `get`, `list`, and `watch` permissions only. The base ClusterRole covers Pods, Pod logs, Events, Nodes, Deployments, ReplicaSets, StatefulSets, and DaemonSets.

The base role is cluster-wide. A production overlay should replace it with namespace-scoped Roles when NERV only monitors an allowlist of namespaces.

NERV does not expose a shell tool or a generic Kubernetes client to the model. Prompt instructions do not change RBAC permissions.

## Failure behavior

- PostgreSQL unavailable at startup: NERV fails to start.
- PostgreSQL unavailable after startup: readiness fails and workflow steps retry according to DBOS policy.
- Kubernetes API unavailable: the finding records collection failure as evidence and can continue.
- Decision Gateway unavailable: its step retries, then the workflow fails if retries are exhausted.
- Agentgateway unavailable: its step retries, then the workflow fails if retries are exhausted.
- Gotify unavailable: notification retries without rerunning completed investigation steps.
- NERV restarts: DBOS resumes incomplete workflows from PostgreSQL.
