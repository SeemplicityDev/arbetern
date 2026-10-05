# ClickHouse Integration

Arbetern integrates with **ClickHouse** so the **ovad** and **hermes** agents can
run **read-only SQL** against a service's actual databases and tables via the
ClickHouse HTTP interface (`clickhouse_query`). It works with any ClickHouse
service that exposes the HTTP interface — self-hosted or managed.

> **Scope: this integration is restricted to the `ovad` and `hermes` agents.**
> Its tools are advertised exclusively to those two and the dispatch layer
> rejects the call from any other agent, even if its model fabricates the tool
> name. The allowlist lives in one place — `restrictedIntegrations` in
> [`commands/helpers.go`](../commands/helpers.go)
> (`"clickhouse": {"ovad", "hermes"}`). To expose ClickHouse to additional
> agents, add their IDs there; both tool registration and dispatch read the
> same map.

The query tool POSTs read-only SQL to the service's HTTP(S) endpoint and returns
the result rows.

## Required Credentials

Authentication uses **HTTP Basic auth** with a read-only database user:

| Environment Variable | Required | Description |
|---|---|---|
| `CLICKHOUSE_QUERY_ENDPOINT` | yes | Service HTTP(S) endpoint including the port, e.g. `http://clickhouse.internal:8123` or `https://clickhouse.example.com:8443` |
| `CLICKHOUSE_QUERY_USER` | yes | Read-only database username (HTTP Basic username) |
| `CLICKHOUSE_QUERY_PASSWORD` | no | Database password (HTTP Basic password) |

The tool is advertised only once its connectivity check succeeds. A lightweight
background probe (`SELECT 1`) is attempted and retried, so the tool becomes
available once its first call succeeds; until then it is not advertised to the
model. Startup log:

```
ClickHouse SQL query interface enabled (endpoint: http://clickhouse.internal:8123)
```

## SQL Query Interface (read-only)

The `clickhouse_query` tool runs read-only SQL against a ClickHouse service's
own **HTTP interface** (port **8123** for HTTP, **8443** for HTTPS by default).
It is intentionally **generic**: it exposes the raw read-only query capability
(`SHOW` / `DESCRIBE` / `EXISTS` / `SELECT`) so the agent can discover databases
and tables and count or aggregate rows. Any business-specific mapping — e.g.
which database belongs to a given tenant, or which table holds a particular kind
of record — lives in the **agent's prompt**, never in arbetern's source.

### Set up a read-only database user

Create (or reuse) a database user that can only read, and use it for
`CLICKHOUSE_QUERY_USER` / `CLICKHOUSE_QUERY_PASSWORD`:

```sql
CREATE USER arbetern_ro IDENTIFIED BY 'REPLACE_ME' SETTINGS readonly = 1;
GRANT SELECT, SHOW TABLES, SHOW DATABASES ON *.* TO arbetern_ro;
```

### Defense in depth

Read-only is enforced two ways: the recommended `readonly`/SELECT-only user
**and** an in-code guard that rejects any statement which is not clearly
read-only before it is ever sent. Only `SELECT` / `WITH` / `SHOW` / `DESCRIBE` /
`EXISTS` / `EXPLAIN` / `VALUES` are permitted; `INSERT` / `ALTER` / `CREATE` /
`DROP` / `DELETE` / `SET` / `SYSTEM` and any other write or state change are
refused (whether standalone or hidden inside a `WITH …` prefix). Result sets are
capped server-side via `max_result_rows`.

### Attribution

Every query is sent with the HTTP `User-Agent: arbetern/clickhouse-connector`,
which ClickHouse records as `http_user_agent` in `system.query_log`. You can
therefore see exactly which queries came from arbetern:

```sql
SELECT event_time, user, query
FROM system.query_log
WHERE http_user_agent LIKE 'arbetern/%'
ORDER BY event_time DESC
LIMIT 20;
```

## Helm Deployment

Static credentials via the chart's secret values:

```yaml
createSecret: true

secretValues:
  clickhouse-query-endpoint: "http://clickhouse.internal:8123"
  clickhouse-query-user: "arbetern_ro"
  clickhouse-query-password: "REPLACE_ME"
```

Or create the secret manually:

```bash
kubectl create secret generic arbetern-secrets \
  --from-literal=clickhouse-query-endpoint=... \
  --from-literal=clickhouse-query-user=... \
  --from-literal=clickhouse-query-password=...
```

The chart only emits the `CLICKHOUSE_QUERY_*` env vars when
`clickhouse-query-endpoint` is non-empty in `secretValues`, so leaving the block
unset cleanly disables the integration.

### Restricting to the allowed agents at deploy time

Because the tool is already gated to ovad and hermes in code, the simplest
production setup is to mount the ClickHouse credentials **only for those
agents** via the chart's per-agent credential overlay, instead of the global
secret:

```yaml
createSecret: true

customCredentials:
  ovad: &clickhouse-creds
    clickhouse-query-endpoint: "http://clickhouse.internal:8123"
    clickhouse-query-user: "arbetern_ro"
    clickhouse-query-password: "REPLACE_ME"
  hermes: *clickhouse-creds
```

This provisions `arbetern-ovad-secrets` and `arbetern-hermes-secrets`, mounts
each at `/etc/arbetern/agent-credentials/<agent>/`, and the app overlays those
keys on top of the global config for those agents only. Give hermes its own
database user if you want the two agents' queries distinguishable in
`system.query_log`.

## Available Tool

| Tool | Description |
|---|---|
| **clickhouse_query** | Run read-only SQL against the configured ClickHouse service and return the rows as a table. Use `SHOW DATABASES` / `SHOW TABLES FROM <db>` to discover schema, `DESCRIBE <db>.<table>` / `EXISTS TABLE <db>.<table>` to inspect, and `SELECT …` (e.g. `SELECT count() FROM <db>.<table>`) to count or aggregate. Accepts `sql` (required) and an optional `row_limit` (1..10000, default 1000). Non-read-only statements are rejected |

## Example Slack Commands

```
/ovad in ClickHouse, list the databases
/ovad in ClickHouse, how many rows does the findings table in the acme database have
/hermes in ClickHouse, describe acme.findings
/hermes review the ORDER BY of acme.findings against this query: <SQL>
```

## Limitations

- **Read-only.** `clickhouse_query` refuses any statement that is not clearly
  read-only and is best paired with a SELECT-only database user. It never
  creates, modifies or deletes anything.
- **No system tables.** The read-only guard blocks the `system` token, so
  `system.*` tables (parts, mutations, merges, query_log) are not reachable
  through the tool.
- **Query result cap.** `clickhouse_query` returns at most `row_limit` rows
  (default 1000, max 10000); larger result sets are capped server-side and
  marked truncated.
