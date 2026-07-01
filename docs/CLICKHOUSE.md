# ClickHouse Cloud Integration

Arbetern integrates with **ClickHouse** so the **ovad** agent can:

- pull an organization's **usage-cost report** from the **ClickHouse Cloud API**
  — a per-day, per-entity breakdown of spend in **ClickHouse Credits (CHC)**
  across services, data warehouses and ClickPipes (`clickhouse_usage_cost`); and
- run **read-only SQL** against a service's actual databases and tables via the
  ClickHouse HTTP interface (`clickhouse_query`).

The two surfaces are configured independently — enable either or both.

> **Scope: this integration is restricted to the `ovad` agent only.** Its tools
> are advertised exclusively to ovad and the dispatch layer rejects the call
> from any other agent, even if its model fabricates the tool name. The
> allowlist lives in one place — `restrictedIntegrations` in
> [`commands/helpers.go`](../commands/helpers.go) (`"clickhouse": {"ovad"}`). To
> expose ClickHouse to additional agents, add their IDs there; both tool
> registration and dispatch read the same map.

The billing tool hits the read-only endpoint
`GET /v1/organizations/{organizationId}/usageCost` and returns the grand total
plus the daily per-entity cost records for the requested window. The query tool
POSTs read-only SQL to the service's HTTPS endpoint and returns the result rows.

## Required Credentials

Authentication uses **HTTP Basic auth** with a ClickHouse Cloud **API key** (a
key ID as the username and a key secret as the password). Create the key in the
ClickHouse Cloud console and note your **organization ID**, then expose:

| Environment Variable | Required | Description |
|---|---|---|
| `CLICKHOUSE_KEY_ID` | for billing | Cloud API key ID (HTTP Basic username) |
| `CLICKHOUSE_KEY_SECRET` | for billing | Cloud API key secret (HTTP Basic password) |
| `CLICKHOUSE_ORGANIZATION_ID` | for billing | Organization ID the usage-cost report covers (a UUID) |
| `CLICKHOUSE_QUERY_ENDPOINT` | for SQL query | Service HTTPS endpoint, e.g. `https://abc123.us-east-1.aws.clickhouse.cloud:8443` |
| `CLICKHOUSE_QUERY_USER` | for SQL query | Read-only database username (HTTP Basic username) |
| `CLICKHOUSE_QUERY_PASSWORD` | for SQL query | Database password (HTTP Basic password) |

The two surfaces are independent: set the three `CLICKHOUSE_KEY_*` /
`CLICKHOUSE_ORGANIZATION_ID` values to enable the billing tool
(`clickhouse_usage_cost`), and/or set `CLICKHOUSE_QUERY_ENDPOINT` +
`CLICKHOUSE_QUERY_USER` (password optional) to enable the read-only SQL query
tool (`clickhouse_query`). Each tool is advertised only once its own
connectivity check succeeds. A lightweight background probe (a minimal
single-day `usageCost` call for billing, a `SELECT 1` for the query interface —
the same paths and permissions the tools use) is attempted and retried, so a
tool becomes available once its first call succeeds; until then it is not
advertised to the model. Startup logs:

```
ClickHouse integration enabled (organization: 00000000-0000-0000-0000-000000000000)
ClickHouse SQL query interface enabled (endpoint: https://abc123.us-east-1.aws.clickhouse.cloud:8443)
```

## Setup

1. **Find your organization ID.** In the ClickHouse Cloud console open
   **Organization** settings; the organization ID (a UUID) is shown there and in
   the console URL.

2. **Create an API key.** In the console go to **Organization → API Keys →
   New API key**. Give it a name and the **organization-level** role needed to
   read billing/usage data. The console returns a **key ID** and a **key
   secret** — copy the secret immediately (it is shown only once).

3. **Grant least privilege.** The key only needs to read the organization's
   usage cost. Prefer the most restrictive role your organization offers and
   avoid granting service-admin scope to a reporting key.

> The key authenticates at the organization scope; the connector is read-only by
> construction (it only issues `GET …/usageCost`), but the role you assign is the
> real boundary on what the key can do.

## SQL Query Interface (read-only)

The `clickhouse_query` tool runs read-only SQL against a ClickHouse service's
own **HTTP interface** (typically port **8443** for HTTPS on ClickHouse Cloud).
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

`CLICKHOUSE_QUERY_ENDPOINT` is the service's HTTPS endpoint including the port,
e.g. `https://abc123.us-east-1.aws.clickhouse.cloud:8443` (find it under
**Connect** in the ClickHouse Cloud console → **HTTPS**).

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
  clickhouse-key-id: "your-clickhouse-key-id"
  clickhouse-key-secret: "your-clickhouse-key-secret"
  clickhouse-organization-id: "00000000-0000-0000-0000-000000000000"
  # Optional — read-only SQL query interface (clickhouse_query):
  clickhouse-query-endpoint: "https://abc123.us-east-1.aws.clickhouse.cloud:8443"
  clickhouse-query-user: "arbetern_ro"
  clickhouse-query-password: "REPLACE_ME"
```

Or create the secret manually:

```bash
kubectl create secret generic arbetern-secrets \
  --from-literal=clickhouse-key-id=... \
  --from-literal=clickhouse-key-secret=... \
  --from-literal=clickhouse-organization-id=...
```

The chart only emits the `CLICKHOUSE_*` env vars when `clickhouse-key-id` is
non-empty in `secretValues`, so leaving the block unset cleanly disables the
billing integration. The `CLICKHOUSE_QUERY_*` env vars are emitted independently
when `clickhouse-query-endpoint` is non-empty.

### Restricting to a single agent at deploy time

Because the tool is already gated to ovad in code, the simplest production setup
is to mount the ClickHouse credentials **only for ovad** via the chart's
per-agent credential overlay, instead of the global secret:

```yaml
createSecret: true

customCredentials:
  ovad:
    clickhouse-key-id: "your-clickhouse-key-id"
    clickhouse-key-secret: "your-clickhouse-key-secret"
    clickhouse-organization-id: "00000000-0000-0000-0000-000000000000"
    clickhouse-query-endpoint: "https://abc123.us-east-1.aws.clickhouse.cloud:8443"
    clickhouse-query-user: "arbetern_ro"
    clickhouse-query-password: "REPLACE_ME"
```

This provisions `arbetern-ovad-secrets`, mounts it at
`/etc/arbetern/agent-credentials/ovad/`, and the app overlays those keys on top
of the global config for ovad only.

## Available Tool

| Tool | Description |
|---|---|
| **clickhouse_usage_cost** | Retrieve the organization usage-cost report for a date range. Returns the grand total plus a per-day, per-entity breakdown (service / data warehouse / ClickPipe) in ClickHouse Credits (CHC) and a cost-by-category split (compute, storage, backup, data transfer). Accepts `from_date` (required, `YYYY-MM-DD`), `to_date` (required, `YYYY-MM-DD`, inclusive) and an optional `filters` array of resource-tag filters |
| **clickhouse_query** | Run read-only SQL against the configured ClickHouse service and return the rows as a table. Use `SHOW DATABASES` / `SHOW TABLES FROM <db>` to discover schema, `DESCRIBE <db>.<table>` / `EXISTS TABLE <db>.<table>` to inspect, and `SELECT …` (e.g. `SELECT count() FROM <db>.<table>`) to count or aggregate. Accepts `sql` (required) and an optional `row_limit` (1..10000, default 1000). Non-read-only statements are rejected |

### Parameters

```json
{
  "from_date": "2024-12-01",
  "to_date": "2024-12-31",
  "filters": ["tag:Environment=Production"]
}
```

- **from_date / to_date** are ISO-8601 dates evaluated in **UTC**. `to_date` is
  inclusive and may be at most **30 days after** `from_date` (a maximum 31-day
  window). Wider windows are rejected client-side before the request is sent.
- **filters** is optional and currently supports **resource-tag** filters only,
  e.g. `tag:Environment=Production`. Omit it for the whole organization.

## Example Slack Commands

```
/ovad what did we spend on ClickHouse last week
/ovad show ClickHouse usage cost for December 2024
/ovad which ClickHouse service is driving cost this month
/ovad ClickHouse spend from 2024-12-01 to 2024-12-31 filtered to tag:Environment=Production
/ovad in ClickHouse, list the databases
/ovad in ClickHouse, how many rows does the findings table in the acme database have
```

## Limitations

- **Billing tool = usage cost only.** `clickhouse_usage_cost` wraps the billing
  usage-cost endpoint only; other ClickHouse Cloud APIs (services, keys,
  members, …) are not exposed.
- **Read-only.** `clickhouse_usage_cost` issues a single `GET …/usageCost`;
  `clickhouse_query` refuses any statement that is not clearly read-only and is
  best paired with a SELECT-only database user. Neither ever creates, modifies
  or deletes anything.
- **31-day window.** For `clickhouse_usage_cost`, `to_date` may be at most 30
  days after `from_date`. Split a longer period into multiple calls.
- **Cost unit is CHC.** Amounts are reported in ClickHouse Credits, not a fiat
  currency. The grand total, per-entity totals and category rollup are all in
  CHC.
- **Query result cap.** `clickhouse_query` returns at most `row_limit` rows
  (default 1000, max 10000); larger result sets are capped server-side and
  marked truncated.
