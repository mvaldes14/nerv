# Architecture

NERV is a Go service that runs inside Kubernetes. It uses DBOS for durable execution and PostgreSQL for workflow state, incident records, evidence, findings, and notification history.

## Components

| Component | Responsibility |
| --- | --- |
| HTTP API | Accept operator questions and Alertmanager-compatible webhooks |
| Kubernetes detector | Reconcile Pod and workload informer state into deterministic failure conditions |
| DBOS | Checkpoint workflow steps and recover interrupted investigations |
| PostgreSQL | Store DBOS state and NERV reporting tables |
| Kubernetes tools | Read pods, workloads, nodes, events, logs, and owner references |
| Decision Gateway | Classify alert disposition and urgency |
| Agentgateway | Route OpenAI-compatible LLM requests |
| Gotify | Deliver investigation findings |
| OpenTelemetry | Export HTTP, service, and GenAI semantic-convention traces |

## Investigation workflow

`/ask`, `/alert`, and qualified autonomous Kubernetes failures start `nerv.investigate`.

1. Normalize the request and assign an incident ID.
2. Start the workflow with a stable DBOS workflow ID.
3. Persist the incident in the `nerv` PostgreSQL schema.
4. Persist normalized trigger evidence and route the decision by explicit intake source. Operator questions (`http.ask` and `cli`) receive a local `investigate` decision; alerts and autonomous Kubernetes failures use configured Decision Gateway.
5. Skip Agentgateway when decision is `suppress` and build policy outcome containing decision reason, provider, and request ID when available.
6. Otherwise start bounded agent loop with question, trigger evidence, optional target, strict read-only tool definitions, and structured finding schema.
7. Run each model round as deterministic DBOS step. Model may return final structured finding or request approved tools.
8. Validate every tool call. Unknown, malformed, unsafe, duplicate, and over-budget calls return bounded error results and never reach Kubernetes.
9. Run independent tool calls as named DBOS steps with concurrency cap, append results as untrusted tool messages, and persist tool evidence.
10. Stop deterministically on structured final finding or round, tool, token, time, or known-cost budget boundary.
11. Persist finding, send it to Gotify, and mark incident complete.

Completed model and tool steps replay from DBOS state after recovery instead of repeating external calls.

## GenAI telemetry

Agentgateway inference emits OpenTelemetry GenAI semantic-convention spans. Each OpenAI-compatible request creates a `chat <model>` client span with operation, provider, requested and returned model, response ID, finish reasons, token usage, server address, and low-cardinality error type when available. The underlying HTTP request is also instrumented for propagation and transport diagnostics.

Prompt text, Kubernetes evidence, model output, system instructions, and tool payloads are not recorded in telemetry. These fields can contain credentials, personal data, or untrusted workload content.

Investigation emits `invoke_workflow NERV`, nested `invoke_agent NERV`, one `chat <model>` span per primary/fallback attempt, and one `execute_tool <tool>` span per validated executor call. Spans include low-cardinality model, finish, token, budget-limit, tool-name, call-ID, and error metadata only. DBOS recovery does not depend on keeping one span open across process restarts: intake trace context is serialized into workflow input, and each execution or recovery attempt links back to originating intake span.

Telemetry conformance will be checked against a pinned GenAI semantic-convention registry with OpenTelemetry Weaver. Semantic violations should fail the conformance test; coverage reporting remains non-blocking.

## Incident identity

Operator questions receive a new UUID unless the caller supplies `idempotency_key`. Idempotency keys are source-independent. Autonomous failures use a canonical key containing namespace, kind, name, container, and deterministic failure class. Recognized external alerts build the same key; other alerts retain their Alertmanager fingerprint. PostgreSQL resolves that key to one active incident ID transactionally, and DBOS uses the incident ID as workflow identity. This suppresses overlapping watcher, reconnect, and external intake across process restarts. PostgreSQL/DBOS correlation is the only authority for reusing an incident; a Decision Gateway `duplicate` disposition only selects `suppress` and does not prove an existing incident or create an incident link.

Detector state also suppresses repeated informer updates while a condition remains active. Recovery clears in-memory active state. Completed correlations remain mapped to their incident through the configured cooldown, then may resolve to a new incident ID. Correlations whose workflows are still running remain active, preventing downstream PostgreSQL or AI failures from causing unbounded new work.

## Autonomous Kubernetes detection

Shared client-go informers watch Pods, Deployments, StatefulSets, and DaemonSets. Initial cache lists provide startup reconciliation; client-go relists and periodic resyncs provide reconnect reconciliation. A bounded, deduplicating queue feeds fixed workers, so event storms cannot create goroutine-per-event or unbounded work.

Pure rules detect CrashLoopBackOff, image pull failures, sustained unschedulable Pending Pods, repeated unexpected container restarts, failed or unavailable Deployments, and unavailable StatefulSets or DaemonSets. Observed workload generation must be current. Kubernetes Events are not used as historical state. Maintenance annotations suppress resource evaluation.

Conditions must persist for a configured threshold. Brief recovery clears pending state. Qualified requests go through `Service.Accept` and the same DBOS workflow as HTTP intake. Acceptance errors remain retryable on later resync; accepted work is suppressed before Decision Gateway or LLM calls.

## Kubernetes evidence

Model receives only explicit typed read operations:

- list Pods, Deployments, StatefulSets, and DaemonSets in one namespace or all namespaces
- describe Pods, Deployments, StatefulSets, DaemonSets, and Nodes
- list attached Kubernetes Events
- current and previous Pod logs
- Pod owner chain
- get Node status summary

Schemas reject extra properties and bound selectors, discovery count, log tails, and output bytes. Resource summaries exclude environment variables, annotations, managed fields, and full Pod specs. No shell, exec, Helm, arbitrary URL, generic resource client, Secret/ConfigMap read, or mutation tool exists.

## Source-aware decision routing

NERV routes decisions by exact source values:

| Request source | Decision path |
| --- | --- |
| `http.ask` | Local `investigate` decision with provider `nerv.local` |
| `cli` | Local `investigate` decision with provider `nerv.local` |
| `http.alert` | Configured Decision Gateway `alert-disposition/v1` contract |
| `kubernetes.detector` | Configured Decision Gateway `alert-disposition/v1` contract |

Unknown sources fail before any remote decision call. When Decision Gateway is not configured, alert and detector sources retain the existing local `investigate` fallback.

For remote sources, NERV maps disposition and urgency to a workflow action:

| Disposition | Urgency | NERV action |
| --- | --- | --- |
| `noisy`, `duplicate`, `informational` | any | `suppress` |
| `actionable` | `immediate` | `escalate` |
| `actionable` | `soon` | `investigate` |
| `actionable` | `backlog` | `research` |
| `actionable` | `none` | `suppress` |

A genuine `suppress` result persists a policy outcome rather than model output. The answer includes the policy reason and available provider/request metadata, while `finding.decision` remains the structured source of those fields. Suppression never fabricates an existing incident ID or link.

## Persistence

DBOS creates its system tables in the `dbos` schema. NERV creates product tables in the `nerv` schema:

- `nerv.incidents`
- `nerv.evidence`
- `nerv.findings`
- `nerv.incident_correlations`
- `nerv.notification_attempts`
- `nerv.notification_batches`
- `nerv.notification_batch_items`

NERV runs its schema migration during startup. Readiness fails when PostgreSQL is unavailable.

## Notifications

Gotify delivery is a DBOS step with bounded retries. Operator questions and external alerts notify immediately. Autonomous detector findings enter a PostgreSQL-backed sliding debounce batch; each new finding extends the quiet-period deadline, then the leader workflow sends one digest covering every incident in the batch. Individual investigations and per-incident delivery records remain available.

NERV checks for a recorded successful delivery before sending again. A process failure after Gotify accepts a message but before PostgreSQL records it can still produce a duplicate. Gotify does not provide an idempotency key for this case.

## Security boundary

The Kubernetes ServiceAccount has `get`, `list`, and `watch` permissions only. The base ClusterRole covers Pods, Pod logs, Events, Nodes, Deployments, ReplicaSets, StatefulSets, and DaemonSets.

The base role is cluster-wide. A production overlay should replace it with namespace-scoped Roles when NERV only monitors an allowlist of namespaces.

NERV does not expose a shell tool or a generic Kubernetes client to the model. Prompt instructions do not change RBAC permissions.

## Failure behavior

- PostgreSQL unavailable at startup: NERV fails to start.
- PostgreSQL unavailable after startup: readiness fails, workflow steps retry according to DBOS policy, and detector acceptance retries only on bounded informer resyncs.
- Kubernetes API unavailable: tool returns bounded error evidence and model may continue or report uncertainty.
- Decision Gateway unavailable: its step retries, then the workflow fails if retries are exhausted.
- Agentgateway unavailable: primary retries are bounded; configured fallback runs once only for eligible transport/provider failure. With no fallback or failed fallback, workflow fails.
- Gotify unavailable: notification retries without rerunning completed investigation steps.
- NERV restarts: DBOS resumes incomplete workflows from PostgreSQL; informer startup reconciliation re-evaluates current cluster state without multiplying stable workflow identities.
