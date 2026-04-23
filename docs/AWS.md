# AWS Integration

Arbetern integrates with the AWS Cost Explorer API so agents and scheduled
workflows can report on spend, project forward, and break costs down by
service / account / region without leaving Slack.

Only Cost Explorer is wired up today. The AWS client uses the standard SDK
credential chain, so adding more services later (CloudWatch, S3, EC2, …)
reuses the same auth plumbing.

## Required Credentials

Authentication is resolved by the [AWS SDK v2 default credential chain][chain],
in this order:

1. Environment variables (`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`,
   optionally `AWS_SESSION_TOKEN`).
2. Shared config / credentials file (`~/.aws/credentials`, `AWS_PROFILE`).
3. **EKS IRSA** — the projected service-account token at
   `AWS_WEB_IDENTITY_TOKEN_FILE` + `AWS_ROLE_ARN`.
4. EC2 IMDS (if running directly on EC2).

For in-cluster deployments, **IRSA is strongly preferred** — no long-lived
credentials on disk, automatic rotation, IAM auditability. See the Helm
section below.

| Environment Variable | Required | Description |
|---|---|---|
| `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` | conditional | Static IAM credentials. Required only if you do not use `AWS_PROFILE` or IRSA |
| `AWS_SESSION_TOKEN` | no | Needed only for STS temporary credentials |
| `AWS_PROFILE` | no | Shared-credentials profile name (local dev) |
| `AWS_WEB_IDENTITY_TOKEN_FILE`, `AWS_ROLE_ARN` | no | Set automatically on EKS when IRSA is configured on the service account |
| `AWS_REGION` | no | Region used to sign Cost Explorer SigV4 calls. Default `us-east-1` (the only region that hosts the CE endpoint). Cost data returned is account-global regardless of this value |

Arbetern only enables the Cost Explorer tools when at least one of these
looks present (`AWS_ACCESS_KEY_ID`, `AWS_PROFILE`,
`AWS_WEB_IDENTITY_TOKEN_FILE`, `AWS_ROLE_ARN`,
`AWS_SHARED_CREDENTIALS_FILE`). On startup the server calls
`Credentials.Retrieve()` so misconfiguration fails loudly with a log line
like:

```
AWS integration misconfigured (tools will be unavailable): ...
```

## Required IAM Permissions

Attach the following IAM policy to the user / role arbetern runs as:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "ce:GetCostAndUsage",
        "ce:GetCostForecast",
        "ce:GetDimensionValues"
      ],
      "Resource": "*"
    }
  ]
}
```

> **Billing note:** every Cost Explorer API call costs **$0.01**. Scheduled
> workflows that grouped- or filter-pivot aggressively can rack up real
> dollars — prefer one grouped call over N filtered calls, and avoid
> looping over services.

## Helm Deployment

### Preferred: IRSA (EKS)

Create an IAM role with the policy above and trust relationship for your
cluster's OIDC provider, then annotate the arbetern service account:

```yaml
serviceAccount:
  create: true
  annotations:
    eks.amazonaws.com/role-arn: arn:aws:iam::123456789012:role/arbetern-cost-explorer

env:
  # optional — default us-east-1
  AWS_REGION: "us-east-1"

# Do NOT set aws-access-key-id/secret when using IRSA.
secretValues: {}
```

The AWS SDK automatically picks up the projected token injected by the EKS
Pod Identity webhook. No secret is required and no keys sit on disk.

### Fallback: Static Credentials

For non-EKS clusters, populate the secret values:

```yaml
createSecret: true

env:
  AWS_REGION: "us-east-1"

secretValues:
  aws-access-key-id: "AKIA..."
  aws-secret-access-key: "your-aws-secret-access-key"
  # aws-session-token: ""   # only needed for STS temp credentials
```

Or create the secret manually:

```bash
kubectl create secret generic arbetern-secrets \
  --from-literal=aws-access-key-id=AKIA... \
  --from-literal=aws-secret-access-key=...
```

The Helm chart only emits the `AWS_ACCESS_KEY_ID` env var when
`aws-access-key-id` is non-empty in `secretValues`, and only emits
`AWS_SESSION_TOKEN` when `aws-session-token` is non-empty — so switching
between IRSA and static auth just means (un)setting those fields.

## Local Development

```bash
# Option 1: explicit keys
export AWS_ACCESS_KEY_ID="AKIA..."
export AWS_SECRET_ACCESS_KEY="..."
export AWS_REGION="us-east-1"

# Option 2: shared profile
export AWS_PROFILE="my-read-only-profile"

go run .
```

On startup you should see:

```
AWS integration enabled (signing region: us-east-1)
```

If credentials are misconfigured, arbetern logs the reason and leaves the
tools disabled; any agent call trying `aws_*` tools returns a clear
"integration is not configured" error.

## Available Tools

| Tool | Description |
|---|---|
| **aws_get_cost_and_usage** | Query Cost Explorer for cost and usage over a date range. Supports DAILY / MONTHLY / HOURLY granularity, all standard metrics (`UnblendedCost`, `BlendedCost`, `AmortizedCost`, `NetUnblendedCost`, `NetAmortizedCost`, `UsageQuantity`), optional `group_by` dimension (`SERVICE`, `LINKED_ACCOUNT`, `REGION`, `USAGE_TYPE`, `INSTANCE_TYPE`, `OPERATION`, `PURCHASE_TYPE`, `RECORD_TYPE`, `AVAILABILITY_ZONE`, `PLATFORM`, `TENANCY`, `DATABASE_ENGINE`), and optional exact-match `service_filter`. Default window: last 8 days, DAILY, `AmortizedCost` (spreads Reserved Instance / Savings Plan up-front charges across their commitment term for accurate daily trend analysis) |
| **aws_get_cost_forecast** | Project future spend using Cost Explorer's forecast model. `start` must be >= today; `end` within 12 months. Default window: tomorrow → +30 days, DAILY |
| **aws_list_dimension_values** | Enumerate values for a dimension (e.g. list every SERVICE that accrued cost in the last 30 days). Useful for discovering the exact service strings AWS expects as `service_filter` — e.g. `"Amazon Elastic Compute Cloud - Compute"`, not `"EC2"` |

All three tools cap time windows at 90 days per call and validate date
format (`YYYY-MM-DD`; `end` is exclusive, matching Cost Explorer's
convention).

## Date Handling

Cost Explorer treats the `end` date as **exclusive**. To report spend
through April 21 2026 inclusive, pass `end=2026-04-22`. The default
8-day window means a workflow running on 2026-04-22 morning UTC will see:

- 7 fully-completed days (Apr 15 → Apr 21)
- Today-in-progress (Apr 22)

which is what you want for a "daily cost summary" posted at morning UTC.

## Example Slack Commands

```
/ovad what did we spend on AWS yesterday
/ovad break down yesterday's AWS spend by service
/ovad how much did EKS cost us last week
/ovad show me AWS cost by linked account for the last 7 days
/ovad forecast our AWS spend for the next 30 days
/ovad which services grew the most week-over-week
/ovad list the exact service names that had cost in the last 30 days
```

## Example Workflow: Daily Cost Summary

A scheduled monoflow workflow for a morning cost report:

```
Daily AWS cost summary — 08:00 UTC

1. Call aws_get_cost_and_usage with the last 8 days at DAILY granularity,
   AmortizedCost metric, no group_by. Extract the per-day totals.

2. Call aws_get_cost_and_usage for the most recent completed day (start =
   yesterday, end = today) with group_by=SERVICE. Sort services by spend
   descending.

3. Compute:
   - Yesterday's total
   - % change vs the average of the 7 prior days
   - Per-service delta vs each service's own 7-day average share

4. post_slack_message to <channel-id> in mrkdwn:
   - Header: ":aws: AWS Daily Cost Summary <date>"
   - ↑/↓ arrow + % change line
   - Monospaced 8-day totals table
   - "Trend analysis" bullet list grouped into
     Significant (>10%), Moderate (5-10%), Minor (<5%)
     with :red_circle: / :large_yellow_circle: / :large_blue_circle:
   - One-line net delta summary
```

Create it from Slack with the built-in workflow tools, or edit from the
web UI (the pencil button on `/<agent>/workflow/<id>`).

## Troubleshooting

**`AWS integration misconfigured (tools will be unavailable)`**
- Credentials did not resolve. Check `AWS_ACCESS_KEY_ID` / `AWS_PROFILE` /
  IRSA annotations. On EKS, run
  `kubectl exec <pod> -- env | grep AWS_` to confirm the SDK sees
  `AWS_WEB_IDENTITY_TOKEN_FILE` and `AWS_ROLE_ARN`.

**`AccessDeniedException: ... is not authorized to perform: ce:GetCostAndUsage`**
- The IAM policy is missing one of the three required actions. Verify with
  `aws iam simulate-principal-policy`.

**`Rate exceeded` from Cost Explorer**
- Cost Explorer is rate-limited per account (default 1 request / sec). If
  several workflows hit the API simultaneously, stagger their schedules or
  consolidate.

**Wrong region / empty results**
- `AWS_REGION` only affects the SigV4 signer, **not** the data returned.
  Cost Explorer is a global service. If results look wrong, check
  `group_by`, `service_filter`, and the `start` / `end` window.

[chain]: https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/configure-gosdk.html#specifying-credentials
