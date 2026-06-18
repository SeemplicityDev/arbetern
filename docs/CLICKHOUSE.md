# ClickHouse Cloud Integration

Arbetern integrates with the **ClickHouse Cloud API** so the **ovad** agent can
pull an organization's **usage-cost report** — a per-day, per-entity breakdown
of spend in **ClickHouse Credits (CHC)** across services, data warehouses and
ClickPipes — and summarise it in Slack.

> **Scope: this integration is restricted to the `ovad` agent only.** The
> `clickhouse_usage_cost` tool is advertised exclusively to ovad and the
> dispatch layer rejects the call from any other agent, even if its model
> fabricates the tool name. The allowlist lives in one place —
> `restrictedIntegrations` in [`commands/helpers.go`](../commands/helpers.go)
> (`"clickhouse": {"ovad"}`). To expose ClickHouse to additional agents, add
> their IDs there; both tool registration and dispatch read the same map.

Each call hits the read-only billing endpoint
`GET /v1/organizations/{organizationId}/usageCost` and returns the grand total
plus the daily per-entity cost records for the requested window.

## Required Credentials

Authentication uses **HTTP Basic auth** with a ClickHouse Cloud **API key** (a
key ID as the username and a key secret as the password). Create the key in the
ClickHouse Cloud console and note your **organization ID**, then expose:

| Environment Variable | Required | Description |
|---|---|---|
| `CLICKHOUSE_KEY_ID` | yes | Cloud API key ID (HTTP Basic username) |
| `CLICKHOUSE_KEY_SECRET` | yes | Cloud API key secret (HTTP Basic password) |
| `CLICKHOUSE_ORGANIZATION_ID` | yes | Organization ID the usage-cost report covers (a UUID) |

Arbetern enables the `clickhouse_usage_cost` tool only when **all three** values
are set. A lightweight connectivity check (a minimal single-day `usageCost`
call — the same endpoint and permission the tool uses) is attempted in the
background and retried, so the tool becomes available once the first
authenticated call succeeds; until then it is not advertised to the model.
Startup logs:

```
ClickHouse integration enabled (organization: 00000000-0000-0000-0000-000000000000)
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

## Helm Deployment

Static credentials via the chart's secret values:

```yaml
createSecret: true

secretValues:
  clickhouse-key-id: "your-clickhouse-key-id"
  clickhouse-key-secret: "your-clickhouse-key-secret"
  clickhouse-organization-id: "00000000-0000-0000-0000-000000000000"
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
integration.

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
```

This provisions `arbetern-ovad-secrets`, mounts it at
`/etc/arbetern/agent-credentials/ovad/`, and the app overlays those keys on top
of the global config for ovad only.

## Available Tool

| Tool | Description |
|---|---|
| **clickhouse_usage_cost** | Retrieve the organization usage-cost report for a date range. Returns the grand total plus a per-day, per-entity breakdown (service / data warehouse / ClickPipe) in ClickHouse Credits (CHC) and a cost-by-category split (compute, storage, backup, data transfer). Accepts `from_date` (required, `YYYY-MM-DD`), `to_date` (required, `YYYY-MM-DD`, inclusive) and an optional `filters` array of resource-tag filters |

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
```

## Limitations

- **Usage cost only.** This first iteration wraps the billing usage-cost
  endpoint only; other ClickHouse Cloud APIs (services, keys, members, …) are not
  exposed.
- **Read-only.** The connector issues a single `GET …/usageCost`; it never
  creates, modifies or deletes anything.
- **31-day window.** `to_date` may be at most 30 days after `from_date`. Split a
  longer period into multiple calls.
- **Cost unit is CHC.** Amounts are reported in ClickHouse Credits, not a fiat
  currency. The grand total, per-entity totals and category rollup are all in
  CHC.
