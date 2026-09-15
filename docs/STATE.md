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
| `chat/<agent>/<id>.json` | UI chat transcripts. Each carries an `owner` field — the signed-in email that created it — and every read and write is scoped to it, so one viewer never sees another's threads. An empty `owner` means the conversation was created with no identity source (no auth proxy) and is reachable only by an equally unidentified caller |
| `skills/<id>.json` | Custom skills |
| `mcp/<id>.json` | MCP connectors |
| `billing/usage-YYYY-MM.json`, `billing/recent.json` | Usage & billing ledger |
| `metrics/perf-YYYY-MM.json`, `metrics/recent.json` | Performance series: turn, model-call and tool latency histograms and outcome counts. Carries no identity — keyed by agent, entry path, model, backend and tool only |
| `queue/<topic>/<task>.json` | Deferred work waiting to run (see below). A `user-context` task carries the turn it is about to write, so until it is processed the same conversation text lives here as well as under `user-context/` |
| `user-context/<agent>/<user>.json` | Per-user rolling context (`{"entries":[{id,at,c,q,a}]}`, `c` = channel) |
| `user-profiles/<identity>.json` | Aggregated per-person context, derived from the documents above: one copy per identity that person is recorded under, each holding the same merged view. Derived state — safe to delete, rebuilt on the hour |
| `sessions/<channel>/<thread>.json`, `sessions/_stats.json` | Slack thread sessions and their counters |
| `gitops/<kind>.json` | Status of the last GitOps reconcile, shared with every replica |
| `catalog/manifest.json` | What the catalog search index currently holds |
| `locks/scheduler`, `locks/workflows/…`, `locks/dashboards/…`, `locks/sessions/…` | Leases (see below) |

## Browsing the state

The console's **Backend** page (`/ui/backend`, API `/api/backend`) is a
read-only browser over these prefixes: the objects laid out as folders, an
object viewer that masks secret-looking values, and a sample of the vector
index. Access is limited to the Slack user groups in `BACKEND_VIEW_TEAMS` and
the emails or domains in `BACKEND_VIEW_EMAILS`.

It fails closed: with both unset the page and every `/api/backend` route answer
403, so the view stays off until you deliberately name who may open it. Reads
are capped at 1 MiB per object and served with `Cache-Control: no-store`.

One prefix has a second, unrestricted door. The **Your context** page
(`/ui/context`, API `/api/me/context`) aggregates `user-context/<agent>/<user>.json`
across every agent for the person making the request — the Slack ID and the
email they are recorded under — and nothing else. It is keyed by the identity
the request authenticated as rather than by an allow-list, so anyone signed in
can read their own remembered turns without being able to reach anybody else's.

## Aggregated per-person context

`user-context/<agent>/<user>.json` answers "what does *this agent* remember of
*this key*". Two things it cannot answer: what a person has been doing across
the other agents, and that the Slack member ID the bot records and the email
the console records are the same person. `user-profiles/<identity>.json` is the
aggregate that does, and because it is one object it is also what the console
page and the prompt path read instead of walking the whole prefix.

**Identity.** A person is a group of identity keys. The only resolvable link is
the Slack direction — `users.info` gives a member ID's email, and the email
gives the key the console records them under — because the stored email key is
a one-way hash. Groups are therefore built by union, not by expanding each key
on its own, so a person comes out as one group whichever of their keys is seen
first. The same aggregate is written under every key in the group, so a lookup
from any source is a single GET with no indirection.

**When it is built.** Three layers, each covering the one before it:

| Layer | When | What it does |
|---|---|---|
| Incremental | On every recorded turn, in the `user-context` queue task | Merges the new turn into the person's cached profile inside a conditional write, so the aggregate is current within seconds. Two replicas recording turns for the same person cannot lose one another's |
| Rebuild | At start and every hour, on the replica holding the scheduling lease, after the TTL sweep and the index repair | Re-derives every profile from the per-agent documents, writes only the ones whose content actually changed, and deletes the ones whose documents are gone |
| On read | A `/api/me/context` request that finds no cached profile | Builds the turns and metrics live and caches them in the background, so a person who has never been aggregated still gets an answer. The written profile is not produced here: a page load must never wait on a model call |

**Self-healing.** The per-agent documents stay the source of truth and the
profile is derived, so anything that goes wrong with it is repaired rather than
carried: a missed or failed incremental update, a profile half-written by a
replica that died, one left behind after the TTL sweep removed its documents,
and one written before two identities were known to belong to the same person
are all corrected by the next rebuild. The prose is carried forward across the
rebuilds that do not rewrite it, so a rebuild never costs a person their
profile. Deleting the whole `user-profiles/` prefix is a supported operation;
it costs one rebuild, plus one model call per person over the passes that
follow.

**What is in it.** Three things, in rising order of cost. The *turns*
themselves, merged and ordered. *Metrics* counted from them rather than
inferred — the channels and GitHub repositories the turns keep touching, the
first and last turn, active days, turns a week, the busiest hour — recomputed
on every aggregation, free. And the *written profile*: a markdown description
of what this person works on, where, and how they use each agent, produced by
a model call over their own turns and the metrics above.

**Naming is best-effort.** Channel IDs are resolved to names last, after the
counting, and only for the channels that made the list. A channel the workspace
will not name — the app missing `channels:read` / `groups:read`, a channel the
bot was never in, a DM — is counted like any other and carried with its ID; the
metrics, the profile and the written summary are produced around it, and the
number left unnamed travels with the metrics so the console can say so rather
than leaving a silent gap. Nothing about a profile waits on Slack answering.

**Cost of the written profile.** It is the only part of the aggregation that
calls a model, so it is confined to the hourly pass and rationed there: at most
25 summaries per pass, and a profile is only rewritten when its turns have
actually moved on *and* its prose is at least 6 hours old. A cold start
converges over a few passes instead of summarising everyone at once. The call
is recorded in the usage ledger under source `profile` and agent `platform` —
it is platform overhead, not an agent answering anyone. With no model client
wired, turns and metrics are still aggregated; only the prose is missing.

**Caps.** A profile holds at most 300 turns and 256 KiB, dropping the oldest —
larger than one agent's document because it merges all of them, bounded so the
prompt-side read stays one small object. Storage is roughly the user-context
prefix again, times the number of identities a person has.

**What the agents get from it.** On every turn the agent's own document remains the
authority for its own turns. The profile adds two things: the turns this person
had with *this* agent under another identity, merged into the same working
memory and semantic pools so Slack and the console are one conversation; and a
short background block of what they recently asked the *other* agents —
questions only, never answers, capped at six lines.

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
- **Billing and metrics merges.** Usage events and performance samples are
  buffered in memory and merged into their stored month aggregate with a
  conditional write — every 15 seconds for usage, every 20 for performance.
  Several replicas recording at once fold their samples into the same object
  without losing each other's turns. Latency is kept as a histogram precisely
  because it merges by addition, so percentiles survive both the merge and the
  month rollup.

  Merging on flush only updates the replica doing the flushing, so these two
  reconcile on the same 30-second cycle as every other registry: each replica
  re-reads the current and previous month (older aggregates can no longer
  change) plus the recent feed, and re-applies whatever it still has buffered.
  Without that a replica serving only the console would report zero while its
  siblings reported the truth, and which one answered decided what the Usage and
  Performance tabs showed.

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
- `locks/sessions/<channel>/<thread>` — held while a replica answers a message
  in a Slack thread, so a second reply arriving on another replica is ignored
  instead of answered twice. It expires two minutes after a crash.

Followers still accept every write (a workflow created from Slack, a
dashboard rendered from the UI, a GitOps `sync now`); the leader picks the
change up on its next reconciliation and starts or stops the corresponding
runner, firing the first tick of a freshly created workflow just as a
single-replica deployment would.

Slack thread sessions live in the bucket too: the replica that answers a slash
command writes `sessions/<channel>/<thread>.json`, any replica that receives a
reply in that thread reads it (a short per-replica cache keeps busy threads
cheap), and the leader expires sessions nobody touched for the TTL and posts
the expiry notice. The follow-up conversation itself is read back from the
user's context entries of the last ten minutes in that channel, so nothing
about a thread depends on which replica served the previous message. The
chart therefore defaults to two replicas.

The GitOps reconcile runs only on the leader; it writes its status to
`gitops/<kind>.json` after every run, and the status endpoint of every
replica serves that object, so the Workflows and Dashboards pages show the
same sync state everywhere. A `running` flag older than ten minutes is
treated as stale.

## Deferred work

Anything a requester should not wait for is handed to a queue kept in the same
bucket under `queue/<topic>/`. One task is one object; the key starts with the
enqueue timestamp, so a plain list is oldest-first.

A worker runs on **every** replica, not only the leader. Claiming a task is a
conditional write on the task object itself: the worker reads it, stamps its
own instance and a lease expiry, and writes it back with `If-Match`. Exactly
one replica's write can succeed, so the claim *is* the lock — no leader, no
broker, and the backlog spreads across the fleet instead of piling onto one pod.
A task whose lease expires (the replica died mid-handler) becomes claimable
again. Enqueuing also nudges the local worker, so a task usually starts within
milliseconds rather than at the next poll.

One listing covers the whole `queue/` prefix, so adding a topic costs no extra
requests, and an idle queue backs its polling off from 5 to 30 seconds — the
common state of this queue is empty, and a fixed fast poll across a fleet costs
far more in requests than it saves in latency. A task enqueued on another
replica therefore waits at most one poll.

Each topic chooses its delivery mode:

| Topic | Mode | What it defers |
|---|---|---|
| `user-context` | at-least-once, 4 attempts | Writing a finished turn into the user's rolling context, indexing its vector, and folding it into that person's aggregated profile. Retries are safe: the entry ID travels with the task, so a redelivered task recognises the write it already made instead of appending the turn twice. A failed profile update is logged rather than retried — the hourly rebuild repairs it |
| `workflow-run` | at-most-once | Manual "Run now" and event-triggered (`on_success` / `on_failure`) runs. The task is dropped the moment it is claimed: a tick opens pull requests and posts to Slack, so a run lost to a crash is far cheaper than one replayed after it. The per-workflow lease still prevents two replicas running the same workflow at once |

A failed at-least-once task is rescheduled with exponential backoff (30s
doubling to 15 minutes) and dropped once its attempt budget is spent. A task
that cannot be decoded at all is dropped on sight rather than re-read on every
poll, and a task for a topic this binary does not know is left alone, so a
rolling deploy hands it to the replica that does. The
Performance page shows the pending depth and the age of the oldest task per
topic; a backlog that keeps growing means the fleet is behind.

Moving the user-context write off the reply path is what it is for: the turn's
memory is not read again until the *next* turn, so the requester's answer no
longer waits on a conditional read-modify-write plus an embedding call.

## Lists and detail pages

The list endpoints (`/api/workflows`, `/api/dashboards`) return descriptors
without run histories, fetched data and rendered reports; the page of one
workflow or dashboard fetches the full object. Only that page polls, and it
does so adaptively.

## Semantic user context (S3 Vectors)

Optionally, the per-user context becomes a semantic memory:

```
S3_VECTORS_INDEX_ARN=arn:aws:s3vectors:eu-central-1:123456789012:bucket/acme-arbetern-vectors/index/user-context
EMBEDDING_MODEL=amazon.titan-embed-text-v2:0   # default when the index is set
EMBEDDING_DIMENSIONS=1024                      # defaults per model; must equal the index dimension
```

Every completed turn — Slack command, thread reply or web chat — is embedded
and written to the index keyed `<agent>/<user>/<entry-id>` with filterable
metadata `{agent, user, at}`; the text itself stays in the user's document,
so the index carries no payload and no metadata size limit applies. Web chat
users are keyed by a slug of their signed-in email plus a short hash.

On the next request the question is embedded and the closest entries of that
user from the last 90 days are fetched. A match is kept only when its cosine
distance is at most 0.6 and within 0.2 of the best match, so an unrelated
question brings no stale context along; the two most recent entries are
always added, and the result is rendered chronologically into the system
prompt after the working memory (the same-channel turns of the last ten
minutes). The document keeps up to 200 entries (instead of 50) because only
the relevant ones reach the prompt. Any failure of the embedding model or the
index falls back to the recency window for that request.

Agents with `shared_memory: true` in their `config.yaml` also receive up to
four related turns of *other* users of that agent (distance at most 0.45),
rendered without names. Enable it only where answers are not personal.

The scheduling replica repairs the index every hour: entries whose vector is
missing (an indexing call that failed, or turns recorded while the index was
down) are embedded, and vectors whose entry no longer exists are deleted.
That needs `s3vectors:GetVectors` and `s3vectors:ListVectors` on the index.

## Catalog search

The same index also holds one vector per workflow and dashboard, keyed
`registry/<kind>/<agent>/<id>` and built from the name, description, schedule
and prompt or sources. The leader re-embeds changed descriptors every five
minutes (a manifest of content hashes avoids re-embedding unchanged ones) and
removes deleted ones. The agents get `find_workflow` and `find_dashboard`
tools, and the Workflows and Dashboards pages search by meaning through
`/api/workflows/_search` and `/api/dashboards/_search`; without an index both
fall back to plain text matching.

Embedding models: Amazon Titan (`amazon.titan-embed-text-v2:0`, 1024 dims;
`v1`, 1536) through Bedrock, or any `text-embedding-3-*` / `ada-002`
deployment on Azure OpenAI or GitHub Models — the backend is chosen from the
model name and the credentials already configured for inference. Create the
index with the cosine metric and the same dimension; boot fails the vector
index (and logs why) when they differ.
