# Local Kubernetes testing

The local overlay tests NERV inside a disposable Kubernetes cluster. It uses in-cluster authentication and the same ServiceAccount and RBAC resources as the base deployment.

## Requirements

- Docker Desktop with Kubernetes enabled, or another disposable cluster
- `docker`
- `kubectl`
- `task`
- `curl`
- `jq`

The default context is `docker-desktop`. Local tasks refuse to run when `KUBE_CONTEXT=homelab`.

## Start the stack

Wait until Docker Desktop reports that Kubernetes is running, then check the context:

```bash
kubectl config get-contexts
kubectl --context docker-desktop cluster-info
```

Build and deploy:

```bash
task local:up
```

This command:

1. verifies the target context
2. builds `nerv:dev`
3. applies `deploy/local`
4. starts ephemeral PostgreSQL
5. waits for PostgreSQL and NERV rollouts

The local overlay uses:

- `emptyDir` PostgreSQL storage
- `echo` as the LLM provider
- Decision Gateway disabled
- Gotify disabled
- OTLP export disabled
- `imagePullPolicy: Never` for `nerv:dev`

No local test request calls hosted AI providers.

## Smoke test

```bash
task local:smoke
```

The smoke test opens a temporary port forward and verifies:

- `/readyz` can reach PostgreSQL
- `/ask` completes a DBOS workflow
- NERV reads its own Pod through the in-cluster ServiceAccount
- Kubernetes resource evidence appears in the finding
- `/alert` returns a durable incident ID
- the incident reaches `completed`

Results from the `/ask` request are written to `/tmp/nerv-local-ask.json`.

## Recovery test

```bash
task local:recovery
```

The recovery test creates an alert investigation, waits for completion, restarts the NERV Deployment, then reads the same finding from PostgreSQL.

This verifies persistence across a NERV pod restart. It does not yet kill NERV during an active external call. A later chaos test should pause a workflow between steps and confirm DBOS resumes from the last checkpoint.

## Logs

```bash
task local:logs
```

Direct commands remain available:

```bash
kubectl --context docker-desktop -n nerv get pods
kubectl --context docker-desktop -n nerv logs deployment/nerv
kubectl --context docker-desktop -n nerv logs deployment/postgres
```

## Cleanup

```bash
task local:down
```

PostgreSQL uses `emptyDir`, so deleting the local stack removes its data.

## Other disposable clusters

Set `KUBE_CONTEXT` for kind, k3d, or another test cluster:

```bash
task local:up KUBE_CONTEXT=kind-nerv
```

The image must be available to that cluster. `task local:build` imports `nerv:dev` into Docker Desktop's kind node. Other kind and k3d clusters may require an explicit image load:

```bash
kind load docker-image nerv:dev --name nerv
```

Use the matching cluster name and context for your setup.
