# AWS Integration

Arbetern uses AWS in four ways:

- **State backend (required)** — every piece of service state lives in the
  S3 bucket named by `S3_BACKEND_ARN`; optionally an S3 Vectors index turns
  the per-user context into a semantic memory. See [STATE.md](STATE.md).
- **Cost Explorer** — report on spend, project forward, and break costs down
  by service / account / region.
- **S3 tools** — read, write, and list objects from agents and workflows (for
  example persisting a daily CSV report, or reading back a manifest).
- **Athena tools** — read-only SQL over the Cost and Usage Report (and any
  other table in the Glue catalog), for the cost attribution Cost Explorer
  cannot do: per Kubernetes namespace, workload, pod, or resource tag.

All of them reuse the standard SDK credential chain, so adding more services
later (CloudWatch, EC2, …) reuses the same auth plumbing.

> **Bedrock is separate.** AWS **Bedrock** can also serve the agents' underlying
> LLM (Claude), but that is an *inference backend*, not one of the integration
> tools above. It is enabled with `BEDROCK_REGION` and authenticates either with
> a Bedrock API key (`AWS_BEARER_TOKEN_BEDROCK`) or, if that is unset, with this
> same SigV4 credential chain (needing only `bedrock:InvokeModel`). See
> [LLM backends](../README.md#llm-backends). The `AWS_REGION` and IAM
> permissions documented here are for the cost / S3 / Athena tools; Bedrock
> uses its own `BEDROCK_REGION`.

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

**Athena needs no configuration of its own.** Credentials are the only AWS
setting in the deployment: the region, workgroup, catalog and database are
arguments of each tool call, supplied by the agent's prompt or Skill or
discovered at run time, and the IAM policy is what bounds where a query can
actually run. Region defaults to `AWS_REGION` when a call doesn't name one.

The state backend resolves credentials at boot and refuses to start without
them. The AWS *tools* (Cost Explorer, S3 **and** Athena) are only enabled when
at least one of these looks present (`AWS_ACCESS_KEY_ID`, `AWS_PROFILE`,
`AWS_WEB_IDENTITY_TOKEN_FILE`, `AWS_ROLE_ARN`,
`AWS_SHARED_CREDENTIALS_FILE`). On startup the server calls
`Credentials.Retrieve()` so misconfiguration fails loudly with a log line
like:

```
AWS integration misconfigured (tools will be unavailable): ...
```

## Required IAM Permissions

Attach the following IAM policy to the user / role arbetern runs as. The
first two statements are required (state backend); the rest depend on the
features you use.

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "StateBucket",
      "Effect": "Allow",
      "Action": ["s3:ListBucket"],
      "Resource": "arn:aws:s3:::acme-arbetern-state"
    },
    {
      "Sid": "StateObjects",
      "Effect": "Allow",
      "Action": ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"],
      "Resource": "arn:aws:s3:::acme-arbetern-state/prod/*"
    },
    {
      "Sid": "SemanticUserContext",
      "Effect": "Allow",
      "Action": [
        "s3vectors:GetIndex",
        "s3vectors:PutVectors",
        "s3vectors:GetVectors",
        "s3vectors:ListVectors",
        "s3vectors:QueryVectors",
        "s3vectors:DeleteVectors"
      ],
      "Resource": "arn:aws:s3vectors:eu-central-1:123456789012:bucket/acme-arbetern-vectors/index/user-context"
    },
    {
      "Sid": "EmbeddingModel",
      "Effect": "Allow",
      "Action": ["bedrock:InvokeModel"],
      "Resource": "arn:aws:bedrock:eu-central-1::foundation-model/amazon.titan-embed-text-v2:0"
    },
    {
      "Sid": "CostExplorer",
      "Effect": "Allow",
      "Action": [
        "ce:GetCostAndUsage",
        "ce:GetCostForecast",
        "ce:GetDimensionValues"
      ],
      "Resource": "*"
    },
    {
      "Sid": "AthenaWorkgroup",
      "Effect": "Allow",
      "Action": [
        "athena:StartQueryExecution",
        "athena:StopQueryExecution",
        "athena:GetQueryExecution",
        "athena:GetQueryResults",
        "athena:GetWorkGroup",
        "athena:GetDataCatalog",
        "athena:GetDatabase",
        "athena:ListDatabases",
        "athena:GetTableMetadata",
        "athena:ListTableMetadata"
      ],
      "Resource": [
        "arn:aws:athena:us-east-1:123456789012:workgroup/your-athena-workgroup",
        "arn:aws:athena:us-east-1:123456789012:datacatalog/AwsDataCatalog"
      ]
    },
    {
      "Sid": "AthenaDiscovery",
      "Effect": "Allow",
      "Action": ["athena:ListWorkGroups", "athena:ListDataCatalogs"],
      "Resource": "*"
    },
    {
      "Sid": "AthenaGlueCatalogRead",
      "Effect": "Allow",
      "Action": [
        "glue:GetDatabase",
        "glue:GetDatabases",
        "glue:GetTable",
        "glue:GetTables",
        "glue:GetPartition",
        "glue:GetPartitions"
      ],
      "Resource": [
        "arn:aws:glue:us-east-1:123456789012:catalog",
        "arn:aws:glue:us-east-1:123456789012:database/your-cur-database",
        "arn:aws:glue:us-east-1:123456789012:table/your-cur-database/*"
      ]
    },
    {
      "Sid": "AthenaSourceData",
      "Effect": "Allow",
      "Action": ["s3:GetObject", "s3:ListBucket", "s3:GetBucketLocation"],
      "Resource": [
        "arn:aws:s3:::your-cur-bucket",
        "arn:aws:s3:::your-cur-bucket/*"
      ]
    },
    {
      "Sid": "AthenaQueryResults",
      "Effect": "Allow",
      "Action": [
        "s3:GetObject",
        "s3:PutObject",
        "s3:ListBucket",
        "s3:GetBucketLocation",
        "s3:AbortMultipartUpload",
        "s3:ListMultipartUploadParts"
      ],
      "Resource": [
        "arn:aws:s3:::your-athena-results-bucket",
        "arn:aws:s3:::your-athena-results-bucket/your-athena-workgroup/*"
      ]
    },
    {
      "Sid": "S3Tools",
      "Effect": "Allow",
      "Action": ["s3:GetObject", "s3:PutObject"],
      "Resource": "arn:aws:s3:::your-bucket/*"
    },
    {
      "Sid": "S3ToolsList",
      "Effect": "Allow",
      "Action": ["s3:ListBucket"],
      "Resource": "arn:aws:s3:::your-bucket"
    }
  ]
}
```

> **State bucket:** match `StateBucket` / `StateObjects` to `S3_BACKEND_ARN`
> — the object statement covers the key prefix (or `/*` for the bucket root).
> Conditional writes (`If-Match`, `If-None-Match`) need no extra actions.
> Drop `SemanticUserContext` and `EmbeddingModel` when `S3_VECTORS_INDEX_ARN`
> is unset.

> **Billing note:** every Cost Explorer API call costs **$0.01**. Scheduled
> workflows that grouped- or filter-pivot aggressively can rack up real
> dollars — prefer one grouped call over N filtered calls, and avoid
> looping over services.

> **Athena scope:** the four Athena statements are only needed if agents use
> the `aws_athena_*` tools. Pin the workgroup ARN to the one workgroup
> arbetern may use — the workgroup is what decides where results are written,
> so pinning it plus the result prefix bounds every write the integration can
> make. `s3:ListBucket` on the source and result buckets can be narrowed
> further with an `s3:prefix` condition. Arbetern itself never issues a write
> statement (see *Read-only enforcement* below), but the policy should not
> rely on that.

> **S3 tools scope:** `S3Tools` / `S3ToolsList` are only needed if agents use
> the `aws_s3_*` tools. Replace `your-bucket` with the bucket(s) they should
> read/write — `s3:ListBucket` is granted on the bucket ARN, while
> `s3:GetObject` / `s3:PutObject` are granted on the objects ARN (`/*`).
> Grant only the buckets you actually need.

## Helm Deployment

### Preferred: IRSA (EKS)

Create an IAM role with the policy above and trust relationship for your
cluster's OIDC provider, then annotate the arbetern service account:

```yaml
serviceAccount:
  create: true
  annotations:
    eks.amazonaws.com/role-arn: arn:aws:iam::123456789012:role/arbetern

state:
  s3:
    arn: arn:aws:s3:::acme-arbetern-state/prod

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
| **aws_s3_put_object** | Write (upload) text content to an S3 object, creating or overwriting it at the given key — e.g. persist a daily CSV report. Accepts a bucket name, an `s3://bucket/key` URI, or an `arn:aws:s3:::bucket/key` ARN. S3 objects are immutable, so "append" means get + concatenate + put |
| **aws_s3_get_object** | Read (download) the text content of an S3 object. Returns the body plus size and last-modified time; bodies over 1 MiB are truncated. A `NoSuchKey` error is the normal signal the object doesn't exist yet |
| **aws_s3_list_objects** | List objects in a bucket, optionally under a key prefix, sorted by key (so date-stamped filenames come back chronologically). Single page, capped at 1000 keys |
| **aws_athena_query** | Run read-only SQL (Trino dialect) in a named workgroup and database, returning the rows inline. Blocks until the query finishes, then reports the rows plus how many bytes it scanned. 500 rows by default, 5000 max; cancelled after 3 minutes. Optional `region` and `output_location` |
| **aws_athena_schema** | Browse the Glue catalog: databases, a database's tables, or one table's columns **and partition keys**. The model is told to call this before writing a query rather than guessing column names |
| **aws_athena_catalogs** | List the Athena workgroups and data catalogs in a region — how an agent finds the `workgroup` a query needs when its prompt doesn't name one |

The three Cost Explorer tools cap time windows at 90 days per call and
validate date format (`YYYY-MM-DD`; `end` is exclusive, matching Cost
Explorer's convention). The S3 tools auto-detect each bucket's region — so a
bucket in `eu-central-1` works even though Cost Explorer signs in
`us-east-1` — and cap a single `get` / `list` response at 1 MiB / 1000 keys
respectively.

## Cost Attribution with Athena

Cost Explorer aggregates away the detail that answers "which team is this
bill?". The Cost and Usage Report does not: every line item keeps its resource
id and its resource tags, so a CUR table in Glue can attribute EKS spend to a
namespace, a workload, or a pod. That is what the Athena tools are for.

Three tools come with the AWS integration, and none of them are configured:

- `aws_athena_catalogs` — which workgroups and data catalogs exist in a region.
- `aws_athena_schema` — databases, a database's tables, or one table's columns
  and partition keys.
- `aws_athena_query` — the query itself, naming a workgroup and database.

An agent gets the workgroup and database from its prompt or its console Skill
when they are known, and discovers them otherwise. The workgroup carries the
result location, so arbetern writes nowhere else — the query tool's
`output_location` argument can override it but normally should not be passed.

Only the workgroups the IAM role is permitted to use will actually run a
query; `aws_athena_catalogs` lists what Athena reports in the region, which is
a superset when `athena:ListWorkGroups` is granted on `*`.

**Partition filters are not optional.** Athena bills per byte scanned, and a
CUR table holds every line item of every account. A query without a filter on
the table's partition columns scans the whole report; with one it scans a
single billing period. `aws_athena_schema` returns a table's partition keys
precisely so the model can filter on them, and the query tool's description
tells it to call that first and to aggregate in SQL rather than pulling raw
line items back. A result is capped at 500 rows (5000 max) and a query is
cancelled after 3 minutes — an abandoned scan keeps reading S3 and keeps
billing, so arbetern stops it rather than leaving it running.

### Read-only enforcement

Two independent layers, because either alone is a single point of failure:

1. **IAM** — the policy above grants no write action anywhere except the
   workgroup's own result prefix, which Athena needs to return rows at all.
2. **Statement validation** — every statement is tokenized and rejected unless
   it starts with `SELECT` / `WITH` / `SHOW` / `DESCRIBE` / `EXPLAIN` /
   `VALUES` and contains no write verb anywhere. `INSERT`, `CREATE TABLE AS`,
   `UNLOAD`, `MSCK REPAIR`, `ALTER`, `DROP` and friends are refused before the
   query is submitted, including when hidden behind a read-looking prefix such
   as a `WITH … INSERT` CTE. Keywords inside string literals, quoted
   identifiers and comments are ignored, so they cannot be used to smuggle one
   past.

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
/ovad what did EKS cost per namespace last month
/ovad break down last month's EKS spend by workload for the platform namespace
/ovad which pods drove the biggest cost increase week-over-week
/ovad what columns does the CUR table have
/hermes list the athena catalogs and databases
/hermes what partition columns does the events table have in athena
/hermes count rows per day in the events table for the last 7 days
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

Create it from Slack with the built-in workflow tools, or edit it from the
console (the **Edit** button on `/ui/<agent>/workflow/<id>`).

## Troubleshooting

**`AWS integration misconfigured (tools will be unavailable)`**
- Credentials did not resolve. Check `AWS_ACCESS_KEY_ID` / `AWS_PROFILE` /
  IRSA annotations. On EKS, run
  `kubectl exec <pod> -- env | grep AWS_` to confirm the SDK sees
  `AWS_WEB_IDENTITY_TOKEN_FILE` and `AWS_ROLE_ARN`.

**`AccessDeniedException: ... is not authorized to perform: ce:GetCostAndUsage`**
- The IAM policy is missing one of the three required actions. Verify with
  `aws iam simulate-principal-policy`.

**`state backend: resolve region of bucket …` or `AccessDenied` at boot**
- The pod cannot reach the state bucket. Check `S3_BACKEND_ARN` and that the
  role has `s3:ListBucket` on the bucket plus `s3:GetObject` /
  `s3:PutObject` / `s3:DeleteObject` on the prefix. The process exits until
  this is fixed — it never runs without its state.

**`AccessDenied` from an `aws_s3_*` tool**
- The IAM policy is missing `s3:GetObject` / `s3:PutObject` (objects ARN
  `arn:aws:s3:::your-bucket/*`) or `s3:ListBucket` (bucket ARN
  `arn:aws:s3:::your-bucket`), or the bucket name in the policy doesn't
  match the bucket you're addressing.

**`AccessDeniedException` from `aws_athena_query`**
- The role is missing one of the Athena, Glue, or S3 statements above, or the
  workgroup named in the call is not the one the policy pins. Note Athena
  needs *three* kinds of access to run one query: the workgroup, the Glue
  catalog entries, and the S3 objects behind the table — plus write access to
  the result prefix.

**`workgroup is required` / `database is required` from `aws_athena_query`**
- Nothing is configured server-side by design. Name them in the agent's prompt
  or Skill, or have the agent call `aws_athena_catalogs` / `aws_athena_schema`
  first.

**`statement starting with "…" is not allowed`**
- The model tried a write (CTAS, `INSERT`, `UNLOAD`, `MSCK REPAIR`). Arbetern
  refuses these by design; rephrase the request as a `SELECT`.

**Athena query cancelled after 3 minutes, or a huge `scanned` figure**
- The query is missing a partition filter and is scanning the whole report.
  Call `aws_athena_schema` for the table's partition keys and constrain them
  in the `WHERE` clause.

**`Rate exceeded` from Cost Explorer**
- Cost Explorer is rate-limited per account (default 1 request / sec). If
  several workflows hit the API simultaneously, stagger their schedules or
  consolidate.

**Wrong region / empty results**
- `AWS_REGION` only affects the SigV4 signer, **not** the data returned.
  Cost Explorer is a global service. If results look wrong, check
  `group_by`, `service_filter`, and the `start` / `end` window.

[chain]: https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/configure-gosdk.html#specifying-credentials
