# Databricks Integration

Arbetern integrates with the **Databricks SQL Statement Execution API** so the
**ovad** and **pulse** agents can run read-only SQL against a Databricks SQL warehouse and
return the results as a table — ad-hoc analytics over Unity Catalog tables and
Databricks system tables (cost/DBU reporting from `system.billing.usage`, job
run history, lineage, etc.).

> **Scope: this integration is restricted to the `ovad` and `pulse` agents.**
> The `databricks_query` tool is advertised only to those two and the dispatch
> layer rejects the call from any other agent, even if its model fabricates
> the tool name. The allowlist lives in one place —
> `restrictedIntegrations` in [`commands/helpers.go`](../commands/helpers.go)
> (`"databricks": {"ovad", "pulse"}`). To expose Databricks to additional
> agents, add their IDs there; both tool registration and dispatch read the
> same map.

Each call runs **read-only SQL** on the warehouse. The `sql` may be a single
statement or a script of several `;`-separated statements (e.g. `DECLARE`/`SET`
session variables followed by a `SELECT`); the warehouse returns the result of
the last statement. The client validates **every** statement and rejects
mutating ones (`INSERT` / `UPDATE` / `DELETE` / `MERGE` / `CREATE` / `DROP` /
`ALTER` / `TRUNCATE` / `GRANT` / …) — standalone or hidden inside a `WITH` /
`BEGIN` block. Only `SELECT`, `WITH`, `SHOW`, `DESCRIBE`, `EXPLAIN`, `VALUES`
and a small set of read/session statements (`DECLARE` / `SET` / `USE`) are
allowed.

## Required Credentials

Authentication uses the **OAuth 2.0 machine-to-machine (client-credentials)
grant** against the workspace token endpoint (`{host}/oidc/v1/token`, scope
`all-apis`). Create a **workspace service principal**, generate an OAuth
secret for it, and grant it access to a SQL warehouse (see RBAC below), then
expose:

| Environment Variable | Required | Description |
|---|---|---|
| `DATABRICKS_HOST` | yes | Default workspace URL, e.g. `https://dbc-1234abcd-5678.cloud.databricks.com`. A bare hostname is accepted and normalised to `https://` |
| `DATABRICKS_CLIENT_ID` | yes | Service principal OAuth client ID (its application ID) |
| `DATABRICKS_CLIENT_SECRET` | yes | Service principal OAuth secret |
| `DATABRICKS_WAREHOUSE_ID` | no | Default SQL warehouse ID (from the warehouse's **Connection details** tab). When unset, a warehouse the principal can use is selected automatically in whichever workspace a query targets |
| `DATABRICKS_ALLOWED_HOSTS` | no | Comma-separated extra workspace URLs a single query may be redirected to. See [Querying several workspaces](#querying-several-workspaces) |

Arbetern enables the `databricks_query` tool only when **all three** required values are
set. The OAuth token exchange is attempted in the background and retried, so
the tool becomes available once the first handshake succeeds; until then it is
not advertised to the model. Startup logs:

```
Databricks integration enabled (host: https://dbc-....cloud.databricks.com, warehouse: 1234567890abcdef)
```

## Setup & RBAC

1. **Create a service principal.** In the workspace admin settings (or via the
   account console) create a service principal and add it to the workspace.

2. **Generate an OAuth secret.** On the service principal, create an OAuth
   secret — this yields the client ID / client secret pair used above. (This
   is OAuth M2M, *not* a personal access token.)

3. **Grant warehouse access.** Give the service principal **CAN USE** on the
   target SQL warehouse so it can submit statements:
   - SQL Warehouses → your warehouse → **Permissions** → add the service
     principal with **Can use**.

4. **Grant data access (Unity Catalog).** Grant the principal `USE CATALOG` /
   `USE SCHEMA` and `SELECT` on the catalogs/schemas you want queryable.
   For Databricks **system tables** (e.g. cost analysis), grant `SELECT` on the
   relevant system schema:

   ```sql
   GRANT USE CATALOG ON CATALOG system TO `<service-principal-application-id>`;
   GRANT USE SCHEMA  ON SCHEMA  system.billing TO `<service-principal-application-id>`;
   GRANT SELECT      ON SCHEMA  system.billing TO `<service-principal-application-id>`;
   ```

> Grant only the catalogs/schemas the agent genuinely needs. The connector is
> read-only by construction, but Unity Catalog grants are still the real
> boundary on what data the agent can see.

> Every request is sent with the HTTP `User-Agent: arbetern/databricks-connector`,
> so queries this connector runs are attributable to arbetern in the workspace's
> audit logs.

## Helm Deployment

Static credentials via the chart's secret values:

```yaml
createSecret: true

secretValues:
  databricks-host: "https://dbc-1234abcd-5678.cloud.databricks.com"
  databricks-client-id: "00000000-0000-0000-0000-000000000000"
  databricks-client-secret: "dose..."
  databricks-warehouse-id: "1234567890abcdef"
```

Or create the secret manually:

```bash
kubectl create secret generic arbetern-secrets \
  --from-literal=databricks-host=https://dbc-....cloud.databricks.com \
  --from-literal=databricks-client-id=... \
  --from-literal=databricks-client-secret=... \
  --from-literal=databricks-warehouse-id=...
```

The chart only emits the `DATABRICKS_*` env vars when `databricks-host` is
non-empty in `secretValues`, so leaving the block unset cleanly disables the
integration.

### Restricting to a single agent at deploy time

Because the tool is already gated to ovad/pulse in code, the simplest
production setup is to mount the Databricks credentials **only for the agents
that need them** via the chart's per-agent credential overlay, instead of the
global secret. Each agent gets its own defaults and its own allowlist, so a
reporting agent can be given a second workspace without widening ovad's reach:

```yaml
createSecret: true

customCredentials:
  ovad:
    databricks-host: "https://dbc-1234abcd-5678.cloud.databricks.com"
    databricks-client-id: "00000000-0000-0000-0000-000000000000"
    databricks-client-secret: "dose..."
    databricks-warehouse-id: "1234567890abcdef"
  pulse:
    databricks-host: "https://dbc-eu-1234.cloud.databricks.com"
    databricks-client-id: "00000000-0000-0000-0000-000000000000"
    databricks-client-secret: "dose..."
    # No warehouse ID: one value cannot serve both workspaces, so each query's
    # warehouse is resolved in whichever workspace it targets. ovad above stays
    # pinned to its single default because it has no allowlist.
    databricks-allowed-hosts: "https://dbc-eu-1234.cloud.databricks.com,https://dbc-us-5678.cloud.databricks.com"
```

This provisions `arbetern-<agent>-secrets`, mounts it at
`/etc/arbetern/agent-credentials/<agent>/`, and the app overlays those keys on
top of the global config for that agent only.

## Available Tool

| Tool | Description |
|---|---|
| **databricks_query** | Run read-only SQL against a SQL warehouse and return the rows as a table. Accepts `sql` (required — one statement or a `;`-separated read-only script), an optional `parameters` array of `{name, value, type}` bound to `:name` markers, an optional `row_limit` (default 1000, max 10000), and optional `host` / `warehouse_id` overrides that redirect this one statement to another workspace. Polls the statement to completion and walks result chunks up to `row_limit`. Mutating statements are rejected |

### Querying several workspaces

`DATABRICKS_HOST` / `DATABRICKS_WAREHOUSE_ID` are **defaults, not a hard
binding**. A single query may be redirected to another workspace by passing
`host` to `databricks_query`, which is how one report covers data that is
physically split across workspaces — most commonly one
workspace per region, each holding only that region's rows under the same
catalog name:

```json
{
  "sql": "SELECT tenant, count(*) AS events FROM analytics.silver.events GROUP BY tenant",
  "host": "https://dbc-us-5678.cloud.databricks.com",
  "row_limit": 500
}
```

Rules that make this safe and predictable:

- **The same service principal must be a member of every workspace you
  target.** There is one client ID/secret; the client-credentials grant is
  exchanged separately at each workspace's own `{host}/oidc/v1/token`, and
  tokens are cached per workspace. Grant the principal **CAN USE** on a
  warehouse and the necessary Unity Catalog grants in each one.
- **`host` alone is enough.** A warehouse ID exists inside exactly one
  workspace, so the configured default is never reused against a different
  host — a warehouse is discovered in the target workspace instead (preferring
  a running one, cached per workspace, and logged when chosen). Pass
  `warehouse_id` only to pin a specific one, and then only with its own `host`.
- **Only allow-listed workspaces are reachable.** `host` must appear in
  `DATABRICKS_ALLOWED_HOSTS` (the default host is always permitted). With
  that variable unset, every host override is rejected and the integration
  behaves exactly as before. Each entry must additionally be a bare `https`
  origin on a Databricks-owned domain — no path, query string or userinfo.
- **Readiness still tracks the default workspace only,** so an unreachable
  secondary workspace never un-advertises the tool.

The allowlist is a security boundary, not just validation: the
service-principal secret is sent as HTTP Basic auth to `{host}/oidc/v1/token`,
so an unchecked host taken from a prompt would be a credential-exfiltration
path. Keep the list to the workspaces you actually report on.

```yaml
secretValues:
  databricks-host: "https://dbc-eu-1234.cloud.databricks.com"
  databricks-warehouse-id: "1234567890abcdef"
  databricks-allowed-hosts: "https://dbc-eu-1234.cloud.databricks.com,https://dbc-us-5678.cloud.databricks.com"
```

Each query result is labelled with the workspace it ran on (rendered above
the rows), so a report fanning out across regions can attribute every result
set correctly.

### Parameters, not string concatenation

Always bind user-supplied values with **named parameter markers** (`:name`)
rather than concatenating them into the SQL. This avoids injection and quoting
bugs. Named parameters are preferred over `DECLARE` / `SET` variables, though a
read-only `DECLARE`/`SET` … `SELECT` script is also accepted when you genuinely
need session variables:

```json
{
  "sql": "SELECT sku_name, sum(usage_quantity) AS dbus FROM system.billing.usage WHERE usage_date >= :since GROUP BY sku_name ORDER BY dbus DESC",
  "parameters": [
    { "name": "since", "value": "2024-01-01", "type": "DATE" }
  ],
  "row_limit": 100
}
```

### AI / forecasting functions

Databricks AI SQL functions such as `ai_forecast()`, `ai_query()` and
`vector_search()` are supported when invoked inside a `SELECT` / `WITH`.
For a forecast, selecting the historical series in a CTE and passing it to
`ai_forecast()` in the outer query is usually cleaner than a multi-statement
`DECLARE` block:

```sql
WITH history AS (
  SELECT usage_date, sum(usage_quantity) AS dbus
  FROM system.billing.usage
  WHERE usage_date >= :since
  GROUP BY usage_date
)
SELECT * FROM ai_forecast(TABLE(history), horizon => 30)
```

## Example Slack Commands

```
/ovad query databricks for last 30 days of DBU usage by sku from system.billing.usage
/ovad in databricks, sum daily billing usage for the last 14 days
/ovad forecast next month's databricks spend from system.billing.usage
/ovad show me the columns of system.billing.usage in databricks
```

## Limitations

- **Read-only only.** Every statement in the request must be read-only; mutating
  statements are rejected before the warehouse is hit, whether standalone or
  inside a `WITH` / `BEGIN` block. A `;`-separated script of read-only
  statements (e.g. `DECLARE`/`SET` … `SELECT`) is allowed; it returns the result
  of the last statement.
- **Inline results.** Results are fetched inline (JSON), capped by `row_limit`
  (max 10000) and by the API's 25 MiB inline ceiling. Large exports should be
  narrowed with SQL aggregation or a tighter `row_limit`.
- **Cold warehouses.** A stopped SQL warehouse takes time to start; the first
  query after idle may run near the client's poll budget before returning.
