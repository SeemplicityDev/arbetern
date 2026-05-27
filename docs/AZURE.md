# Azure Integration

Arbetern integrates with the **Azure Cost Management** REST API so agents and
scheduled workflows can report on Azure spend, project forward, and break
costs down by service / resource group / region / subscription — symmetrical
to the AWS Cost Explorer integration in [AWS.md](AWS.md).

Cost queries are scoped at the **management group** level, defaulting to
the **tenant root management group** (its ID equals the tenant GUID). That
gives tenant-wide reporting from a single service principal: one cost query
rolls up every subscription nested under the MG. Set
`AZURE_MANAGEMENT_GROUP_ID` to scope to a narrower management group.

Only Cost Management is wired up today. Authentication uses an AAD service
principal (client-credentials flow); the same client can later be extended
to other ARM-based Azure services with no new auth plumbing.

## Required Credentials

Authentication uses the **OAuth 2.0 client-credentials grant** against
`login.microsoftonline.com`. Create an AAD application + service principal,
grant it the `Cost Management Reader` role at the tenant root management
group (or a narrower MG), then expose:

| Environment Variable | Required | Description |
|---|---|---|
| `AZURE_TENANT_ID` | yes | AAD tenant the service principal lives in (GUID). Also the default management-group scope (tenant root MG ID == tenant GUID) |
| `AZURE_CLIENT_ID` | yes | Service principal application (client) ID (GUID) |
| `AZURE_CLIENT_SECRET` | yes | Service principal client secret |
| `AZURE_MANAGEMENT_GROUP_ID` | no | Management-group scope to query costs against. Defaults to `AZURE_TENANT_ID` (tenant root MG — tenant-wide reporting across every subscription) |
| `AZURE_AUTHORITY_HOST` | no | AAD authority. Defaults to `https://login.microsoftonline.com`. Override for Azure Government / China |
| `AZURE_MANAGEMENT_HOST` | no | ARM endpoint. Defaults to `https://management.azure.com`. Override for sovereign clouds |

> **These are distinct from `AZURE_OPEN_AI_ENDPOINT` / `AZURE_API_KEY`,**
> which configure Azure OpenAI as an LLM backend. Cost Management uses its
> own credentials.

Arbetern enables the Azure tools only when the three required values are
set. On startup the server fetches a token to verify the credentials work;
a failure is logged with:

```
Azure integration misconfigured (tools will be unavailable): ...
```

## Required RBAC

Assign the service principal one of these built-in roles **at the tenant
root management group** (recommended — gives tenant-wide cost visibility),
or at a narrower management group you set in `AZURE_MANAGEMENT_GROUP_ID`,
or at the subscription level if you only need one subscription:

- **Cost Management Reader** (preferred — read-only)
- Billing Reader
- Reader

A custom role works too, as long as it grants
`Microsoft.CostManagement/query/action`,
`Microsoft.CostManagement/forecast/action`, and
`Microsoft.CostManagement/dimensions/read`.

```bash
# Tenant-wide: tenant root management group ID equals the tenant GUID.
az role assignment create \
  --assignee <client-id> \
  --role "Cost Management Reader" \
  --scope /providers/Microsoft.Management/managementGroups/<tenant-id>

# Or scope to a narrower management group:
az role assignment create \
  --assignee <client-id> \
  --role "Cost Management Reader" \
  --scope /providers/Microsoft.Management/managementGroups/<management-group-id>
```

> **Tenant root MG access is a deliberately elevated grant.** Make sure
> your security model is comfortable with one service principal seeing
> cost data across every subscription. If not, scope the role assignment
> to a narrower MG and set `AZURE_MANAGEMENT_GROUP_ID` to that MG's ID.

## Helm Deployment

Static credentials via the chart's secret values:

```yaml
createSecret: true

secretValues:
  azure-tenant-id: "00000000-0000-0000-0000-000000000000"
  azure-client-id: "00000000-0000-0000-0000-000000000000"
  azure-client-secret: "your-client-secret"
  # Optional: scope to a narrower management group. Leave empty / unset
  # for tenant-wide reporting (tenant root MG ID == tenant ID).
  # azure-management-group-id: ""

env:
  # Only set these for sovereign clouds (Azure Government / China).
  # AZURE_AUTHORITY_HOST: "https://login.microsoftonline.us"
  # AZURE_MANAGEMENT_HOST: "https://management.usgovcloudapi.net"
```

Or create the secret manually:

```bash
kubectl create secret generic arbetern-secrets \
  --from-literal=azure-tenant-id=... \
  --from-literal=azure-client-id=... \
  --from-literal=azure-client-secret=...
  # Optionally:
  # --from-literal=azure-management-group-id=...
```

The Helm chart only emits the `AZURE_*` env vars when `azure-tenant-id`
is non-empty in `secretValues`, so leaving the block unset cleanly
disables the integration. `AZURE_MANAGEMENT_GROUP_ID` is only emitted
when `azure-management-group-id` is set.

## Available Tools

| Tool | Description |
|---|---|
| **azure_get_cost_and_usage** | Query Cost Management for cost over a date range across every subscription nested under the configured management group (tenant-wide by default). Granularity: `Daily` / `Monthly` / `None`. Metric: `ActualCost` (default, matches Azure portal) or `AmortizedCost` (spreads Reservation / Savings Plan up-front charges across their commitment term). Group by any Cost Management dimension (`ServiceName`, `ResourceGroupName`, `ResourceLocation`, `MeterCategory`, `ChargeType`, `SubscriptionName`, `SubscriptionId`, `ResourceId`, …). Use `group_by=SubscriptionName` to break tenant-wide spend down per subscription. Optional exact-match `service_filter` |
| **azure_get_cost_forecast** | Project future spend using Cost Management's forecast endpoint. Default window: today → +30 days, Daily granularity. Scope is the configured management group (tenant-wide by default) |
| **azure_list_dimension_values** | Enumerate values for a dimension across the configured management group. Useful for discovering exact `ServiceName` strings — Azure expects `"Virtual Machines"`, not `"VM"`; `"Azure Kubernetes Service"`, not `"AKS"` — or for listing every subscription the SP can see (dimension=`SubscriptionName`) |

All three tools cap windows at 90 days per call and validate date format
(`YYYY-MM-DD`; `end` is exclusive, matching arbetern's AWS convention —
arbetern normalises to Cost Management's inclusive `to`).

## Example Slack Commands

```
/ovad what did we spend on Azure yesterday
/ovad break down yesterday's Azure spend by ServiceName
/ovad break down yesterday's Azure spend by SubscriptionName
/ovad how much did AKS cost us last week
/ovad show me Azure cost by resource group for the last 7 days
/ovad forecast our Azure spend for the next 30 days
/ovad list the exact Azure service names that had cost in the last 30 days
```

## Example Workflow: Daily Azure Cost Summary

Mirror of the AWS daily-cost workflow described in [AWS.md](AWS.md):

```
Daily Azure cost summary — 08:00 UTC

1. Call azure_get_cost_and_usage with the last 8 days at Daily granularity,
   ActualCost metric, no group_by. Extract the per-day totals.

2. Call azure_get_cost_and_usage for the most recent completed day
   (start = yesterday, end = today) with group_by=ServiceName. Sort
   services by spend descending.

3. Compute:
   - Yesterday's total
   - % change vs the average of the 7 prior days
   - Per-service delta vs each service's own 7-day average share

4. post_slack_message to <channel-id> in mrkdwn:
   - Header: ":azure: Azure Daily Cost Summary <date>"
   - ↑/↓ arrow + % change line
   - Monospaced 8-day totals table
   - "Trend analysis" bullet list grouped into
     Significant (>10%), Moderate (5-10%), Minor (<5%)
   - One-line net delta summary
```

## Troubleshooting

**`Azure integration misconfigured (tools will be unavailable)`**
- Token endpoint rejected the service principal. Verify `AZURE_TENANT_ID`,
  `AZURE_CLIENT_ID`, `AZURE_CLIENT_SECRET` are correct and the secret has
  not expired (AAD client secrets expire — rotate on schedule).

**`ARM returned 403 Forbidden`**
- The service principal is missing the `Cost Management Reader` role at
  the configured scope. Re-run the `az role assignment create` above
  against the tenant root management group (`/providers/Microsoft.Management/managementGroups/<tenant-id>`)
  or whichever MG you set in `AZURE_MANAGEMENT_GROUP_ID`.

**Empty result with zero rows**
- No subscription nested under the configured management group recorded
  cost in the requested window. Confirm with `az consumption usage list`
  or the Cost Management portal. If you only see one subscription's data
  when you expected tenant-wide, double-check the SP has Cost Management
  Reader at the **tenant root MG**, not just one subscription.
