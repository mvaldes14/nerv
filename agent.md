# NERV agent notes

- Run tests in Docker, not in Kubernetes.
- Kubernetes is only for manifest rendering/diff/apply checks during deploy work.
- Use the local Docker PostgreSQL from `compose.postgres.yml` for DBOS/PostgreSQL development and smoke tests.
- Do not add direct SigNoz/ClickHouse datastore access. SigNoz alert data enters through webhooks; additional observability lookups should go through an approved MCP.
- Keep the service private-by-default: no public ingress, no NodePort, no Cloudflare Tunnel.
