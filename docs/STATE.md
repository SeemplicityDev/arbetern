# State backend (S3)

arbetern keeps no state on local disk. Everything a restart must survive
lives in one S3 bucket named by `S3_BACKEND_ARN`, and each pod holds only an
in-memory cache of it. Any replica can serve any Slack command, chat turn or
UI request; a replica that dies loses nothing, and a new one is ready as
soon as it has loaded the bucket.

```
S3_BACKEND_ARN=arn:aws:s3:::acme-arbetern-state/prod
```

The value may be a bucket ARN, an `s3://bucket/prefix` URI or `bucket/prefix`.
The bucket's region is detected at boot. Credentials come from the default
AWS chain (IRSA on EKS, static keys, profile) — see [AWS.md](AWS.md) for the
IAM policy.

## Layout

| Prefix | Contents |
|---|---|
| `workflows/<agent>/<id>.json` | Workflow descriptors including run history |
| `dashboards/<agent>/<id>.json` | Dashboard descriptors including the latest data / markdown |
| `chat/<agent>/<id>.json` | UI chat transcripts |
| `skills/<id>.json` | Custom skills |
| `mcp/<id>.json` | MCP connectors |
| `billing/usage-YYYY-MM.json`, `billing/recent.json` | Usage & billing ledger |
| `user-context/<agent>/<user>.json` | Per-user rolling context (`{"entries":[{id,at,q,a}]}`) |
| `locks/scheduler`, `locks/workflows/…`, `locks/dashboards/…` | Leases (see below) |

## How the cache works

The design follows the shape of ClickHouse's distributed S3 cache: object
storage is the only source of truth, each node keeps a hot copy in memory,
and immutability plus ETags replace explicit invalidation.

- **Write-through.** Every mutation is written to the bucket first and only
  then applied to the cache, so a crash between the two leaves the bucket
  correct and the cache merely stale.
- **Conditional writes.** Writes carry the cached ETag (`If-Match`) and new
  documents use `If-None-Match: *`. A write that loses a race against another
  replica fails with a conflict; the registry re-reads the document and
  re-applies its change, so an edit made in the UI while a workflow tick was
  recording its run log is never overwritten.
- **Reconciliation.** Every 30 seconds each replica lists its prefixes (one
  request per registry, ETags included), fetches only the documents whose
  ETag changed and evicts the ones that disappeared. S3 list and read
  operations are strongly consistent, so a change made on one replica is
  visible on the others within that window. Objects whose key does not have
  the descriptor shape (`<agent>/<id>.json` or `<id>.json`) are ignored, and
  an object that fails to decode is logged once and left alone until its
  ETag changes.
- **Billing merges.** Usage events are buffered in memory and merged into the
  stored month aggregate with a conditional write every 15 seconds. Several
  replicas recording at once fold their events into the same object without
  losing each other's turns; the merged result becomes the local view, so the
  Usage tab on any replica converges to the global numbers.

## Replicas and leases

Scheduled work must run exactly once, so it is gated by a lease object held
with conditional writes:

- `locks/scheduler` — one replica at a time runs workflow tickers, dashboard
  sync tickers, GitOps reconciles and retention sweeps. It renews the lease
  every 10 seconds; if it stops renewing for 30 seconds another replica takes
  over, and a graceful shutdown releases the lease immediately.
- `locks/workflows/<agent>/<id>` and `locks/dashboards/<agent>/<id>` — taken
  for the duration of one run or sync, so a manual "Run now" handled by one
  replica cannot overlap a scheduled tick on the leader. They expire five
  minutes after a crash.

Followers still accept every write (a workflow created from Slack, a
dashboard rendered from the UI, a GitOps `sync now`); the leader picks the
change up on its next reconciliation and starts or stops the corresponding
runner, firing the first tick of a freshly created workflow just as a
single-replica deployment would.

Two things remain per replica and are not shared through the bucket: Slack
thread sessions (the 7-minute follow-up window) and the 10-minute
conversation memory. With more than one replica, a thread reply may reach a
replica that did not open the session; keep `replicaCount: 1` until those
move into the store, or route Slack traffic to a single replica.

## Semantic user context (S3 Vectors)

Optionally, the per-user context becomes a semantic memory:

```
S3_VECTORS_INDEX_ARN=arn:aws:s3vectors:eu-central-1:123456789012:bucket/acme-arbetern-vectors/index/user-context
EMBEDDING_MODEL=amazon.titan-embed-text-v2:0   # default when the index is set
EMBEDDING_DIMENSIONS=1024                      # defaults per model; must equal the index dimension
```

Every completed turn is embedded and written to the index keyed
`<agent>/<user>/<entry-id>` with filterable metadata `{agent, user, at}`; the
text itself stays in the user's document, so the index carries no payload and
no metadata size limit applies. On the next request the question is embedded,
the eight closest entries of that user are fetched (filtered by agent and
user), the two most recent entries are added, and the result is rendered
chronologically into the system prompt. The document keeps up to 200 entries
(instead of 50) because only the relevant ones reach the prompt, which cuts
prompt tokens compared with the recency window while surfacing older but
related turns. Any failure of the embedding model or the index falls back to
the recency window for that request.

Embedding models: Amazon Titan (`amazon.titan-embed-text-v2:0`, 1024 dims;
`v1`, 1536) through Bedrock, or any `text-embedding-3-*` / `ada-002`
deployment on Azure OpenAI or GitHub Models — the backend is chosen from the
model name and the credentials already configured for inference. Create the
index with the cosine metric and the same dimension; boot fails the vector
index (and logs why) when they differ.
