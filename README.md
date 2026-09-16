<p align="center">
  <img src="assets/logo-full-text.png" alt="Arbetern — AI Agent Orchestration" width="400">
</p>

<p align="center">
  <a href="https://github.com/justmike1/arbetern/stargazers"><img src="https://img.shields.io/github/stars/justmike1/arbetern?style=social" alt="Stars"></a>
  <a href="https://go.dev"><img src="https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go&logoColor=white" alt="Go"></a>
  <a href="LICENSE"><img src="https://img.shields.io/github/license/justmike1/arbetern" alt="License"></a>
  <a href="Dockerfile"><img src="https://img.shields.io/badge/Docker-ready-2496ED?logo=docker&logoColor=white" alt="Docker"></a>
</p>

<p align="center"><em>Yiddish for "workers." (with a typo, but it's cooler)</em></p>

An orchestration platform for AI agents in the enterprise. Each agent lives in its own directory under `agents/`, with dedicated prompts and a defined professional scope. Arbetern provides the runtime, routing, UI, and integrations — agents bring the expertise.

### [Read about it here.](https://medium.com/posts/c996daeb17d5)

### Screenshots

UI screenshots — overview, integrations, MCP & connectors, agents, performance — are in [screenshots/SCREENSHOTS.md](screenshots/SCREENSHOTS.md).

### Architecture — [Bernoulli Naive Bayes](https://en.wikipedia.org/wiki/Naive_Bayes_classifier) by Design

Arbetern's request pipeline is a chain of independent binary decisions — no MCP,
no external orchestrator, no shared state bus. Each stage observes one feature
and picks a class without influencing the next:

1. **Agent dispatch (prior).** Slack routes `/ovad`, `/pulse`, … to a dedicated
   HTTP handler. The agent ID picks the prompt set, RBAC policy, and tool
   palette before any content is read.
2. **Intent classification (binary scan).** Keyword lists fire independently
   (`isIntroIntent`, `isDebugIntent`); `requiresAction` acts as a conditional
   exclusion. First match wins.
   A message carrying a Slack permalink always takes the tool loop, since only
   the general handler can run `fetch_thread_context` to read the linked thread.
3. **Tool loop (posterior update).** The general handler iterates LLM → tool
   calls → results until the model stops calling tools. The tool palette is
   feature-gated: each integration's `Ready()` flag toggles its tools in/out
   of the LLM's function list at request time.
4. **Model switch.** Detecting a code-related tool call dynamically swaps the
   general model for `CODE_MODEL` mid-inference, without restarting the loop.
5. **Thread sessions (temporal memory).** After the first reply a session is
   registered on the Slack thread and stored in the state bucket, so a
   follow-up may be answered by any replica; it re-enters the same router
   with accumulated history (see [Conversation Context](#conversation-context)).
   If the message anchoring the thread is deleted, the session ends quietly:
   no expiry notice is posted and no reply is redirected to the channel.

Every layer is an independent binary decision — no sequential boosting, no
ensemble voting, no external orchestration. The system is the product of
independent feature states, which is the core assumption of Bernoulli Naive
Bayes.

## Current Agents

| Agent | Profession | Description |
|---|---|---|
| **ovad** | DevOps & SRE Engineer | Debugs CI/CD failures, reads/modifies repo files, opens PRs, searches Datadog logs/monitors/infrastructure, runs read-only SQL on a Databricks warehouse, queries ClickHouse databases/tables read-only, and reports ClickHouse Cloud usage cost — all from a Slack slash command |
| **agent-q** | QA & Test Engineer | Analyzes test failures, reviews test coverage, suggests test cases, and triages flaky tests |
| **goldsai** | Security Researcher | Assesses CVE impact on your codebase, audits dependencies, reviews code for vulnerabilities, and recommends remediation |
| **seihin** (製品) | Sr. Technical Product Manager | Reviews and refines Jira tickets, rewrites descriptions with PM best practices, manages ticket quality at scale |
| **pulse** | Customer Success Engineer | Tracks account health, surfaces renewal signals from Salesforce, analyzes call intelligence and deal momentum from Chorus, reads Freshworks support tickets, chats and CRM records, searches and reads Document360 knowledge-base articles, reads files and appends to Google Sheets in shared Drive folders, manages CS workflows, and coordinates with Jira |

## Quick Start

### Prerequisites

- Go 1.26.8+ (the container image builds with Go 1.27)
- A Slack app with a slash command pointing to `/<agent>/webhook` (see [docs/SLACK_BOT.md](docs/SLACK_BOT.md))
- A GitHub PAT with repo access (see [docs/GITHUB_PAT.md](docs/GITHUB_PAT.md))
- (Optional) Azure OpenAI credentials or an AWS Bedrock region for LLM inference

### Environment Variables

The core variables you'll set on day one:

| Variable | Required | Description |
|---|---|---|
| `SLACK_BOT_TOKEN` | yes | Slack bot OAuth token (`xoxb-...`) |
| `SLACK_SIGNING_SECRET` | yes | Slack app signing secret |
| `GITHUB_TOKEN` | yes\* | GitHub PAT (\*or use Azure OpenAI / AWS Bedrock for inference) |
| `GENERAL_MODEL` | yes | Model ID for the active backend — e.g. `openai/gpt-4o` (GitHub), a deployment name (Azure), or a Bedrock model / inference-profile ID. **Required; there is no default** |
| `CODE_MODEL` | no | Separate model for code-related tasks. Optional — falls back to `GENERAL_MODEL` when unset |
| `AZURE_OPEN_AI_ENDPOINT` / `AZURE_API_KEY` | no | Azure OpenAI credentials (alternative to GitHub Models) |
| `BEDROCK_REGION` | no | Selects **AWS Bedrock** as the LLM backend, e.g. `us-east-1` (see [LLM backends](#llm-backends)) |
| `APP_URL` | no | Public app URL (used for Jira ticket stamps and Slack links) |
| `PORT` | no | HTTP port (default: `8080`) |

### LLM backends

Arbetern speaks to one LLM backend at a time, selected by which credentials are
present. When more than one is configured, precedence is **Bedrock → Azure OpenAI
→ GitHub Models**:

| Backend | Selected by | Model ID form (`GENERAL_MODEL` / `CODE_MODEL`) |
|---|---|---|
| **GitHub Models** (default) | `GITHUB_TOKEN` | `openai/gpt-4o`, `meta/llama-3.1-405b-instruct`, … |
| **Azure OpenAI** | `AZURE_OPEN_AI_ENDPOINT` + `AZURE_API_KEY` | your deployment name (`gpt-4o`, `gpt-5.x`, `claude-*` for Foundry) |
| **AWS Bedrock** | `BEDROCK_REGION` | Bedrock model / inference-profile ID, e.g. `anthropic.claude-opus-5` or the cross-region profile `us.anthropic.claude-opus-5` |

**AWS Bedrock** serves Claude models through the same Anthropic Messages
protocol the app already uses for Azure Foundry, so prompt caching
(`LLM_PROMPT_CACHE`), usage/billing, and Headroom compression all work
unchanged. Set `BEDROCK_REGION` to a region where the model is available and
`GENERAL_MODEL` to its Bedrock ID (most accounts need the cross-region inference
profile, e.g. `us.anthropic.claude-opus-5`). `BEDROCK_REGION` is independent
of `AWS_REGION` (which only signs Cost Explorer calls).

Authentication is one of two schemes, and the target principal/key needs
`bedrock:InvokeModel` on the model or inference profile either way:

| Auth | How to enable |
|---|---|
| **Bedrock API key** (bearer token, `ABSK…`) | Set `AWS_BEARER_TOKEN_BEDROCK` (chart secret `bedrock-api-key`). No AWS credential chain is consulted. |
| **SigV4** (default) | Leave the API key unset; credentials resolve through the standard AWS SDK chain — static keys (`AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`), `AWS_PROFILE`, or EKS IRSA (`AWS_WEB_IDENTITY_TOKEN_FILE` + `AWS_ROLE_ARN`) — the same chain the AWS cost tools use. |

<details>
<summary><b>Runtime tuning</b> — sessions, tool rounds, UI access</summary>

| Variable | Description |
|---|---|
| `SLACK_APP_TOKEN` | Slack app-level token (`xapp-...`) for Socket Mode — enables thread follow-ups without slash commands (see [docs/SLACK_BOT.md](docs/SLACK_BOT.md#socket-mode-thread-follow-ups)) |
| `THREAD_SESSION_TTL` | Duration a thread session stays active (default `7m`, Go duration). Also controls the channel-context cache TTL |
| `MAX_TOOL_ROUNDS` | Max LLM tool-call rounds per request (default `200`) |
| `LLM_PROMPT_CACHE` | Enable Anthropic prompt caching of the static prefix (tool schemas + system prompt) and the rolling conversation tail, so long tool-loops re-read shared context at the provider's ~0.1x cache rate instead of full price. Quality-neutral. Default `true`; set `false` as a kill-switch |
| `SHOW_USAGE_STAMP` | Append model/token usage metadata to Slack replies. Default `true` |
| `UI_ALLOWED_CIDRS` | Comma-separated CIDRs allowed to access the UI |
| `TRUSTED_PROXY_CIDRS` | Peers whose `X-Auth-Request-Email` and `X-Forwarded-For` headers are believed, e.g. `10.0.0.0/8`. **Set this whenever an auth proxy is in front.** Without it the app cannot tell a proxy from any other caller, so anything able to reach the pod can name itself an admin and pass every allow-list; startup logs a warning when an allow-list is configured and this is not. When set, `UI_ALLOWED_CIDRS` also resolves the real client from the rightmost untrusted hop instead of the spoofable first entry |
| `MCP_ADMIN_TEAMS` / `MCP_ADMIN_EMAILS` | Who may add, edit, test or delete MCP connectors from the UI: comma-separated Slack user group IDs, and email addresses or domains matched like an agent's `allowed_emails`. Both empty = every UI user. Set from the chart's `mcp.adminTeams` / `mcp.adminEmails`; needs oauth2-proxy so the viewer's email is known. Everyone else sees connectors read-only and the API answers 403 to every verb but GET |
| `MCP_ALLOWED_ENV` | Extra environment variables an MCP connector header may expand with `${NAME}`. The `MCP_*` namespace is always available and nothing else is, so a connector cannot be pointed at an attacker URL with `${SLACK_BOT_TOKEN}` in a header. Saving a connector that references a variable outside the allow-list is rejected |
| `BACKEND_VIEW_TEAMS` / `BACKEND_VIEW_EMAILS` | Who may open the read-only backend state view at `/api/backend` — Slack user group IDs, and emails or domains matched like an agent's `allowed_emails`. **Both empty disables the view entirely**, so it fails closed if you never set them |
| `AGENTS_DIR` | Directory holding the per-agent `config.yaml` / `prompts.yaml` (default `agents`) |
| `AZURE_BILLING_ACCOUNT_ID` | Azure billing account scope for cost queries; falls back to the subscription scope when unset |
| `WORKFLOW_RUN_HISTORY` | How many past runs each workflow keeps in its history |
| `UI_HEADER` | Custom header text for the web UI (default `arbetern`) |
| `HEADROOM_PROXY_URL` | Base URL of a [Headroom](docs/HEADROOM.md) compression sidecar (e.g. `http://localhost:8787`). When set, each conversation is compressed via its `/v1/compress` endpoint before every LLM call — cutting tokens across **all** backends (GitHub Models, Azure OpenAI, Azure Foundry/Claude, AWS Bedrock). Set automatically by Helm when `headroom.enabled: true` |
| `HEADROOM_COMPRESS_TIMEOUT` | Go duration bounding a single `/v1/compress` round-trip before the app falls back to sending the conversation uncompressed (fail-open). Default `90s`; raise for very large contexts. Set via Helm `headroom.compressTimeout` |

</details>

<details>
<summary><b>State</b> — S3 backend, semantic user context</summary>

Every stateful feature (workflows, dashboards, chat, billing, skills, MCP connectors, per-user context) lives in one S3 bucket; pods keep only an in-memory cache. See [docs/STATE.md](docs/STATE.md) for the layout, caching and leases.

| Variable | Description |
|---|---|
| `S3_BACKEND_ARN` | **Required.** Bucket holding all service state, optionally with a key prefix: `arn:aws:s3:::acme-arbetern-state/prod`, `s3://acme-arbetern-state/prod` or `acme-arbetern-state/prod`. The bucket's region is detected at boot; credentials come from the default AWS chain (IRSA, static keys, profile) |
| `S3_VECTORS_INDEX_ARN` | Optional S3 Vectors index (`arn:aws:s3vectors:<region>:<account>:bucket/<vector-bucket>/index/<index>`). When set, every completed turn (Slack or web chat) is embedded and the per-user context shown to the model is the set of prior turns closest to the current question plus the latest few, instead of a plain recency window; the same index powers `find_workflow` / `find_dashboard` and the search boxes of the console — see [docs/STATE.md](docs/STATE.md) |
| `EMBEDDING_MODEL` | Embedding model for the vector index. Default `amazon.titan-embed-text-v2:0` (Bedrock, 1024 dims); `amazon.titan-embed-text-v1`, or a `text-embedding-3-*` / `ada-002` deployment on Azure OpenAI or GitHub Models also work — the backend follows the model name and the inference credentials already configured |
| `EMBEDDING_DIMENSIONS` | Vector size; must equal the index dimension. Defaults per model, required for models the app does not know |
| `CHAT_RETENTION` | How long a UI chat conversation is kept after its last activity before a background sweeper deletes it (applies to all agents). Go duration; defaults to `168h` (1 week). The sweeper runs hourly |
| `PRICE_SOURCE_URL` | Single source of truth for per-token prices, synced on boot and every 24h (default: LiteLLM's public price file, ~2900 models). The billing tab shows the live source, model count, and last-sync time. Set empty to rely solely on `LLM_PRICE_OVERRIDES`. Price changes only affect future turns — recorded costs are frozen at record time |
| `LLM_PRICE_OVERRIDES` | Optional JSON map of model → `{"in":<usd_per_1M>,"out":<usd_per_1M>}` layered on top of the synced feed (wins over it) for negotiated/Azure rates. A model matched by neither is recorded at $0 and flagged `unpriced` |
| `CUSTOM_CONFIG_DIR` | Directory of per-agent config overrides (`<agent-id>.yaml`, a full `config.yaml` overlay — e.g. `chat_enabled`, `allowed_teams`, `allowed_emails`). Set automatically by the chart when `customConfigs` is configured |
| `AGENT_CREDENTIALS_DIR` | Directory of per-agent credential overrides (`<agent-id>/<secret-key>` files). Set automatically by the chart when `customCredentials` is configured. See [Per-Agent Credentials](#per-agent-credentials-integration-overrides) |

</details>

<details>
<summary><b>Atlassian (Jira + Confluence)</b></summary>

| Variable | Description |
|---|---|
| `ATLASSIAN_URL` | Atlassian instance URL (e.g. `https://yourorg.atlassian.net`) |
| `ATLASSIAN_EMAIL` / `ATLASSIAN_API_TOKEN` | Basic Auth credentials |
| `ATLASSIAN_CLIENT_ID` / `ATLASSIAN_CLIENT_SECRET` | OAuth 2.0 client-credentials (alternative to Basic Auth) |
| `JIRA_PROJECT` | Default Jira project key (e.g. `ENG`) |

</details>

<details>
<summary><b>Other integrations</b> — NVD, Salesforce, Chorus, Datadog, AWS, Azure, Databricks, ClickHouse, Freshworks, Document360, Google Drive / Sheets</summary>

| Variable | Description |
|---|---|
| `NVD_API_KEY` | NVD API key for CVE lookups. Free at <https://nvd.nist.gov/developers/request-an-api-key>. Without one, requests are rate-limited (~5 vs ~50 req/30s) |
| `SF_CONSUMER_KEY` / `SF_CONSUMER_SECRET` | Salesforce Connected App credentials (OAuth 2.0 client credentials flow) |
| `SF_LOGIN_URL` | Salesforce login URL (default `https://login.salesforce.com`; use `https://test.salesforce.com` for sandbox) |
| `CHORUS_API_TOKEN` | Chorus (ZoomInfo) API token. Generated in Chorus → Personal Settings |
| `CHORUS_BASE_URL` | Chorus API base URL (default `https://chorus.ai`) |
| `DD_API_KEY_US` / `DD_APP_KEY_US` | Datadog US (datadoghq.com) API + Application keys |
| `DD_API_KEY_EU` / `DD_APP_KEY_EU` | Datadog EU (datadoghq.eu) API + Application keys |
| `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` | AWS static creds. `AWS_PROFILE` and EKS IRSA (`AWS_WEB_IDENTITY_TOKEN_FILE` + `AWS_ROLE_ARN`) also work. Enables the Cost Explorer, S3 **and** Athena tools — credentials are the only AWS setting, since each Athena call names its own region / workgroup / catalog / database. The IAM principal needs `ce:GetCostAndUsage`, `ce:GetCostForecast`, `ce:GetDimensionValues`, plus `s3:GetObject` / `s3:PutObject` / `s3:ListBucket` on any bucket the S3 tools touch, and for Athena the `athena:*` / `glue:Get*` / CUR-and-results S3 permissions in [docs/AWS.md](docs/AWS.md). Each CE API call costs $0.01 |
| `AWS_REGION` | Region used to sign Cost Explorer SigV4 calls (default `us-east-1` — the only region hosting the CE endpoint), and the fallback for an Athena call that names no region. S3 auto-detects each bucket's own region, so it is unaffected by this value |
| `AZURE_TENANT_ID` / `AZURE_CLIENT_ID` / `AZURE_CLIENT_SECRET` | AAD service-principal credentials for the Azure Cost Management tools. Service principal needs `Cost Management Reader` at the tenant root management group (or a narrower MG) for tenant-wide cost reporting across every subscription. Distinct from `AZURE_OPEN_AI_ENDPOINT` / `AZURE_API_KEY` (Azure OpenAI as LLM backend) |
| `AZURE_MANAGEMENT_GROUP_ID` | Optional. Management-group scope for cost queries. Defaults to `AZURE_TENANT_ID` (tenant root MG — covers every subscription in the tenant) |
| `AZURE_AUTHORITY_HOST` / `AZURE_MANAGEMENT_HOST` | Optional sovereign-cloud overrides (Azure Government, China). Default to the public-cloud endpoints |
| `DATABRICKS_HOST` / `DATABRICKS_CLIENT_ID` / `DATABRICKS_CLIENT_SECRET` / `DATABRICKS_WAREHOUSE_ID` | Databricks SQL warehouse + OAuth M2M service-principal credentials. Enables the read-only `databricks_query` tool for the **ovad and pulse agents**. SP needs `CAN USE` on the warehouse + `SELECT` on the target catalogs/schemas. Optional `DATABRICKS_ALLOWED_HOSTS` lets one query target another workspace (e.g. a second region) via the tool's `host`/`warehouse_id` arguments. See [docs/DATABRICKS.md](docs/DATABRICKS.md) |
| `CLICKHOUSE_KEY_ID` / `CLICKHOUSE_KEY_SECRET` / `CLICKHOUSE_ORGANIZATION_ID` | ClickHouse Cloud API key (HTTP Basic key ID + secret) and organization ID. Enables the read-only `clickhouse_usage_cost` billing tool for the **ovad agent only**. See [docs/CLICKHOUSE.md](docs/CLICKHOUSE.md) |
| `CLICKHOUSE_QUERY_ENDPOINT` / `CLICKHOUSE_QUERY_USER` / `CLICKHOUSE_QUERY_PASSWORD` | ClickHouse service HTTPS endpoint (e.g. `https://…clickhouse.cloud:8443`) + a read-only database user. Enables the read-only `clickhouse_query` SQL tool for the **ovad agent only** (SELECT/SHOW/DESCRIBE/EXISTS; mutations rejected). Queries are tagged with the `arbetern` User-Agent in `system.query_log`. See [docs/CLICKHOUSE.md](docs/CLICKHOUSE.md) |
| `FRESHDESK_DOMAIN` / `FRESHDESK_API_KEY` | Freshdesk host (e.g. `acme.freshdesk.com`) + API key. Enables the Freshdesk ticket tools for the **pulse and seihin agents** — read, plus private notes and tags on a ticket (the key's agent role must allow editing tickets). See [docs/FRESHWORKS.md](docs/FRESHWORKS.md) |
| `FRESHCHAT_URL` / `FRESHCHAT_API_TOKEN` | Freshchat API base incl. `/v2` (e.g. `https://acme-123.freshchat.com/v2`) + Bearer token. Enables the read-only Freshchat conversation tools for the **pulse and seihin agents** |
| `FRESHWORKS_CRM_DOMAIN` / `FRESHWORKS_CRM_API_KEY` | Freshworks CRM host (e.g. `acme.myfreshworks.com`) + API key. Enables the read-only CRM search/contact/deal tools for the **pulse and seihin agents**. See [docs/FRESHWORKS.md](docs/FRESHWORKS.md) |
| `DOCUMENT360_API_KEY` | Document360 scoped API key (`d360_sk_…`, sent as `X-API-Key`). Enables the read-only `document360_list_workspaces`, `document360_search`, `document360_list_categories`, `document360_list_articles` and `document360_get_article` tools for the **pulse agent only**. Give the key a read-only content role. Optional `DOCUMENT360_PROJECT_ID` (only when the key sees several projects) and `DOCUMENT360_REGION` (`eu` default, `us`, `ca`). See [docs/DOCUMENT360.md](docs/DOCUMENT360.md) |
| `GOOGLE_CREDENTIALS_JSON` | Google service-account key, base64 of the JSON key file. The only required value — access is granted by **sharing a Drive folder** with the service account's email (Editor to allow appends), which the connector discovers on its own. Enables `drive_list_folders`, `drive_find_file`, `drive_read_file`, `sheets_get_spreadsheet_info`, `sheets_read_range` and the batched `sheets_append_row` for the **pulse agent only**. Auth is the JWT-bearer grant (headless — no interactive OAuth, no domain-wide delegation). Optional `GOOGLE_DRIVE_FOLDER_IDS` confines it to specific folders; optional `GOOGLE_SCOPES` overrides the default `spreadsheets` + `drive.readonly` (adding `.../auth/drive` also enables `drive_copy_file`, which provisions a sheet by copying a template). See [docs/GOOGLE.md](docs/GOOGLE.md) |

</details>

<details>
<summary><b>GitOps sync</b> — workflows + dashboards from a git repo</summary>

See [docs/GITOPS.md](docs/GITOPS.md). All variables reuse `GITHUB_TOKEN`.

| Variable | Description |
|---|---|
| `WORKFLOWS_GITOPS_REPO` | Enables sync: poll `<owner>/<repo>` for `<basePath>/<agent>/<id>.json` |
| `WORKFLOWS_GITOPS_OWNER` | Repo owner (defaults to bot's resolved owner) |
| `WORKFLOWS_GITOPS_BRANCH` | Branch (defaults to repo default) |
| `WORKFLOWS_GITOPS_BASE_PATH` | Base path inside the repo (default `arbetern/workflows`) |
| `WORKFLOWS_GITOPS_INTERVAL` | Poll interval (Go duration, default `5m`, minimum `30s`) |
| `WORKFLOWS_GITOPS_PRUNE` | When `true`, locally-managed workflows that disappear from git are deleted (default `true`) |
| `DASHBOARDS_GITOPS_*` | Same semantics as the `WORKFLOWS_GITOPS_*` knobs above. Default base path `arbetern/dashboards` |

</details>

### Run Locally

```bash
export SLACK_BOT_TOKEN=xoxb-...
export SLACK_SIGNING_SECRET=...
export GITHUB_TOKEN=ghp_...
go run .
```

### Docker

```bash
docker build -t arbetern .
docker run -e SLACK_BOT_TOKEN -e SLACK_SIGNING_SECRET -e GITHUB_TOKEN arbetern
```

### Helm

```bash
cp deploy.example.values.yaml deploy.local.values.yaml
# Edit deploy.local.values.yaml with your secrets
helm upgrade --install arbetern ./helm -f deploy.local.values.yaml
```

## Web UI

Visit `/ui/` for the management console. A top bar carries the logo, title and
theme toggle; a collapsible side rail switches between pages, each with its own
URL:

| Page | URL | What it shows |
|------|-----|---------------|
| Overview | `/ui/` | Audit dashboard: who asks which agent (user × agent matrix, with each person's most-used agent marked), requests over time, per-agent and per-user leaderboards, where requests come from (Slack / chat / workflow / dashboard), live Slack thread sessions, fleet health and recent activity |
| Integrations | `/ui/integrations` | Every connector with its live permission / auth state, scopes and tools |
| MCP & Connectors | `/ui/mcp` | Registered Model Context Protocol servers: add, test (handshake + tool discovery), scope to agents, enable or disable — see [docs/MCP.md](docs/MCP.md) |
| Agents | `/ui/agents` | The roster — open a card for its prompts (read-only), or chat where `chat_enabled` |
| Chats | `/ui/chats` | Conversations of every chat-enabled agent: open, start, rename or delete them (same access rules as the chat itself) |
| Skills | `/ui/skills` | Instruction blocks the agents follow: the built-in ones from the prompt files (read-only) plus custom skills written here and appended to the system prompts of the agents they target |
| Workflows | `/ui/workflows` | Every workflow across agents with schedule, status, last run, run / delete actions and GitOps sync state; each opens at `/ui/<agent>/workflow/<id>` with its flow diagram, prompt or tasks, run history and editor. "Run now" and "Sync now" both return immediately and report through the shared state the page polls, so neither holds a browser request open for the minutes they take |
| Dashboards | `/ui/dashboards` | Every dashboard across agents (source dashboards, prompt templates, rendered reports) with sync state; each opens at `/ui/<agent>/dashboard/<id>` |
| Pull requests | `/ui/pulls` | Open pull requests the agents authored, found by the marker every arbetern-written PR body carries: agent, requester, entry source (Slack / chat / workflow) and age, filterable by agent; ready-for-review PRs are listed first, drafts last with a draft label |
| Tickets | `/ui/tickets` | Unresolved Jira issues assigned to the account behind the Atlassian integration: type, status, priority, reporter, labels and age, filterable by project |
| Backend | `/ui/backend` | Read-only browser of the state bucket laid out as folders, with an object viewer that masks secret-looking values, plus a sample of the vector index; visible only to the Slack user groups or emails in `backendView` (closed when none are set) |
| Your context | `/ui/context` | A profile of the signed-in person written from their own turns: what they work on, the channels and repositories they keep touching, how they use each agent, and counted metrics — with the turns it was written from behind a History tab. Aggregated across every agent and every identity they are recorded under (Slack ID, email) by the background pass, prose and all, so the page is one object read. Reached from the user button, not the rail, and open to anyone signed in: it reads only what is keyed to the identities the request authenticated as, so it needs no allow-list. See [docs/STATE.md](docs/STATE.md#aggregated-per-person-context) |
| Changelog | `/ui/changelog` | Latest commits to the arbetern repository |
| Performance | `/ui/performance` | Recorded statistics: response-time percentiles and their distribution, time to the model's first round, the split between model and tool time, rounds and tool calls per turn, how turns end, provider round-trip latency with retries and rate limits, per-tool latency, the slowest recent turns, the deferred-work backlog, and which optional services are currently being skipped |
| Usage & Billing | `/ui/billing` | Estimated LLM spend by agent, model, source, user and workflow (`/billing` redirects here) |

Overview, Performance and Usage & Billing share one time window (7 / 30 / 90
days / all). Overview and billing read the usage ledger
(`/api/billing/summary`), which also aggregates per user × agent
(`by_user_agent`). Slack user IDs are resolved to display names in the
background via `users.info` (needs the `users:read` scope); until a name is
cached the raw ID is shown. Chat turns are keyed by the proxy-verified email.

Optional services that sit alongside a turn — the Headroom compression sidecar,
the embeddings backend — are guarded by a breaker: each runs before the model on
every round, so one that accepts connections but never answers would otherwise
cost its full timeout per round and spend a whole turn's budget on a step whose
result is optional. One timeout takes it out of the path (the timeout has
already been paid); a couple of cheap failures are tolerated first. It is probed
once per cooldown, starting at 30s and doubling to 10 minutes, and the current
state is on the Performance page under *Optional services*. See
[docs/HEADROOM.md](docs/HEADROOM.md).

The Performance page reads a separate series (`/api/metrics/summary`) that
records **no identity at all** — a sample is keyed by agent, entry path, model,
backend and tool name, never by the person, channel or prompt behind the turn.
That keeps it safe to retain and to point a monitor at. Overview carries a
**Response time** strip with the same numbers at a glance: median and p95, time
to the first model round, the share of turns that produced an answer, tool calls
per turn, and a 30-day p95 trend. Latency is stored as a histogram rather than
raw samples, so percentiles survive both the merge across replicas and the
month rollup.

- The top-right user button shows who is signed in (`/api/me`): the email verified by the SSO proxy, resolved to the Slack profile (name, handle, title, time zone) via `users.lookupByEmail`, and to the Atlassian account (name, account ID) when that integration is connected. Without a proxy it reads "Not signed in".
- The same menu opens **Your context** (`/api/me/context`): how many turns the agents currently remember of you, and the written profile built from them.
- Drop a `logo.png` into `ui/` to replace the default icon
- Set `UI_HEADER` env var to customize the top-bar title
- Agents with `chat_enabled` expose a full-screen chat at `/ui/<agent>/chat` — a deep-linkable, reload-safe URL you can bookmark or share
- Chat replies are produced in the background: sending returns at once, the thread shows elapsed time and tool activity while the agent works, and a reload or another replica picks the in-flight turn up from the stored transcript
- **Conversations are private to the person who created them.** Each transcript records its owner (the proxy-verified email) and every list, open, rename and delete is scoped to that owner, so the Chats page shows only your own threads. A deployment with no auth proxy has no identity to scope by, and there every caller shares one ownerless set — which is also where conversations created before ownership existed live, so they stop appearing once you put a proxy in front
- The side rail's collapsed state, the theme and the time window are remembered per browser

### Authentication (SSO)

The Helm chart bundles the [oauth2-proxy](https://github.com/oauth2-proxy/manifests) subchart (disabled by default) to put Google/GitHub/etc. SSO in front of the browser UI. Enable it in your values:

```yaml
oauth2-proxy:
  enabled: true
  config:
    clientID: "<oauth-client-id>"
    clientSecret: "<oauth-client-secret>"
    cookieSecret: "<openssl rand -base64 32 | tr -- '+/' '-_'>"
```

When enabled, the chart automatically rewires the `ingress` backend to the proxy, so external traffic is authenticated before reaching the app. Only `/ui/` and `/api/` are gated — Slack webhooks (`/<agent>/webhook`) and `/healthz` stay public via `skip_auth_routes` (Slack can't complete an OAuth login), and Slack Socket Mode needs no inbound rule.

- Register `https://<your-host>/oauth2/callback` as an authorized redirect URI in your OAuth provider.
- With a single provider configured, the interstitial sign-in page is skipped and users go straight to the provider.
- This is independent of `UI_ALLOWED_CIDRS`; you can use either or both.
- The proxy passes the verified identity to the app as `X-Auth-Request-Email` (via `set_xauthrequest`). arbetern uses this to enforce per-agent chat access by email — see [Chat access by email](#chat-access-by-email-ui).
- Narrow `email_domains` to your own domain. The chart ships `["*"]` because that is the only value that works unconfigured, but it lets anyone the provider will authenticate reach the console shell. `configFile` is a single opaque string, so setting just that one key in your own values does nothing — copy the whole `configFile` block across and edit the line.

**Set `TRUSTED_PROXY_CIDRS` when you enable the proxy.** The identity header is
just a header: on its own the app cannot tell the proxy from any other caller,
so anything that can reach the pod — another workload in the cluster, an
ingress path that skips the proxy — could send `X-Auth-Request-Email` and be
treated as that person. Name the address range the proxy connects from and the
app ignores the header from anywhere else:

```yaml
env:
  TRUSTED_PROXY_CIDRS: "10.0.0.0/8"   # the pod network your proxy runs on
```

Leave it unset only where nothing but the proxy can reach the Service. Startup
logs a warning if any allow-list is configured without it. Pair it with a
NetworkPolicy that admits only the proxy pod for defence in depth.

## Adding a New Agent

1. Create a directory under `agents/`:
   ```
   agents/my-agent/prompts.yaml
   ```
2. Define prompts in the YAML file (keys like `security`, `classifier`, `general`, `debug`, etc.)
3. Rebuild and deploy — the agent will appear in the UI and get a webhook at `/<agent-name>/webhook`
4. Create a Slack slash command pointing to `https://<your-host>/<agent-name>/webhook`

> **Note:** Each agent directory under `agents/` is automatically discovered at startup and registered with its own webhook route (`/<agent>/webhook`). Create a Slack slash command per agent pointing to the corresponding path.

## Conversation Context

Every Slack-driven request — DMs, channel mentions, slash commands, and in-thread follow-ups — is grounded in a layered context that the router composes into the LLM system prompt. Each layer has its own scope, retention, and size cap so the prompt stays useful without growing unbounded.

| Layer | Scope | Retention | Size cap |
| --- | --- | --- | --- |
| **Agent prompt** | Per agent, static | File on disk (read-only) | Whatever you author in `agents/<id>/prompts.yaml`, plus the enabled skills |
| **Slack user profile** | Per request | Refetched every turn via `users.info` | A few hundred bytes (Slack ID, real name, display name, email, title) |
| **Channel context** | Per channel/DM | In-memory cache, TTL = `THREAD_SESSION_TTL` (default 7m). Background sweeper evicts stale entries; hard cap of 4096 channels with oldest-first eviction | Up to 50 most recent Slack messages (no per-message char cap) |
| **Working memory** | Per `(agent, user, channel)` | The user-context entries of the last 10 minutes in the same channel, read from the state bucket on every request so any replica sees the same conversation | Up to 10 turns, each capped like a user-context entry |
| **User context (persistent)** | Per `(agent, user)`, shared across DMs, channels and web chat | Document in the state bucket at `user-context/<agent>/<user>.json`. 30-day TTL on inactivity (refreshed on every append) | Each entry capped at 800 chars (question) + 1200 chars (answer). Recency mode: up to 50 entries and 96 KiB, all injected. Semantic mode (`S3_VECTORS_INDEX_ARN`): up to 200 entries stored; the closest matches from the last 90 days (cosine distance ≤ 0.6 and within 0.2 of the best) plus the 2 latest injected, capped at 24 KiB |
| **Shared memory** (opt-in) | Per agent with `shared_memory: true` | Semantic mode only: up to 4 related turns of other users of the agent, rendered without names | 8 KiB |
| **Cross-agent profile** | Per person, not per identity: the Slack member ID the bot records and the email the console records are folded into one | Aggregate at `user-profiles/<identity>.json`, refreshed as each turn is recorded and rebuilt hourly from the per-agent documents (see [docs/STATE.md](docs/STATE.md#aggregated-per-person-context)) | Contributes two things to the prompt: this person's turns with *this* agent under their other identities, merged into the working memory and semantic pools, and the last 6 questions they asked the *other* agents — questions only, 2 KiB |

### How it flows

1. **Read on every request.** All layers are assembled before the LLM is called. The user-context document is read for DMs, channels and web chat — `channelID` is *not* part of its key, so every turn of a user merges into the same per-user document; each entry records the channel it came from, which is what scopes the working memory. In semantic mode the question is embedded and the closest prior turns are selected from the vector index.
2. **Aggregate follows the person, not the key.** The agent's own document stays the authority for its own turns; the person's cached profile supplies the rest, in one GET. That is what makes a question asked in the console a continuation of one asked in Slack, and what lets an agent see, as background, what its colleagues are being asked by the same person.
3. **Append on every completed turn.** When the model finishes, a compact `(question, answer, channel)` entry is appended to the user-context document (and embedded into the index in semantic mode). Web chat turns are keyed by the signed-in email. Scheduled workflow ticks (`ExecuteHeadless`) intentionally skip persistence, and so do turns with no answer and turns the loop ended by giving up on a model that kept writing a tool call as text — a give-up reply keeps the question it failed on, which would otherwise win the semantic match against the real answer to the same question later.
4. **Cache reuse.** The channel-history cache TTL is wired to `THREAD_SESSION_TTL`, so a multi-turn thread reuses the same cached 50-message window for the entire session window without re-hitting Slack.

### Knobs

- **`THREAD_SESSION_TTL`** — controls both the thread-session lifetime *and* the channel-context cache TTL.
- **`S3_VECTORS_INDEX_ARN`** — switches the persistent layer from a recency window to semantic retrieval (see [docs/STATE.md](docs/STATE.md#semantic-user-context-s3-vectors)).
- **`shared_memory: true`** in an agent's `config.yaml` — lets that agent ground a user's question in related questions other users asked it.
- All other size caps are constants in [commands/user_context.go](commands/user_context.go) and [commands/context.go](commands/context.go) — adjust there if you need a different envelope.

## Org-Specific Context

Deployment-specific instructions (your GitHub org, default repos, naming
conventions, escalation rules) are written as **skills** in the console rather
than baked into the release: open **Skills → New skill**, pick the agents it
applies to and save. Skills live in the state bucket, take effect on the next
turn, and can be edited or disabled without a redeploy. See [Skills](#skills).

## Agent RBAC (Team-Based Access Control)

Restrict which Slack user groups (teams) can access each agent. When `allowed_teams` is set for an agent, only members of those Slack user groups can invoke it, and only they can add, change or delete that agent's skills in the console (a skill that applies to all agents needs access to every restricted agent). Empty list = open to everyone.

### Default Config (`agents/<id>/config.yaml`)

Each agent's `config.yaml` has an `allowed_teams` field:

```yaml
name: Pulse
allowed_teams:
  - S0123456789   # CS team user group ID
```

### Override via Helm (Kubernetes ConfigMap)

Use the generic `customConfigs` mechanism to override `config.yaml` at deploy
time. Each key is an agent ID; the value is a (possibly partial) copy of that
agent's `config.yaml`. Because the override is a full config file, only the keys
you set take effect and everything else falls through to the baked-in
`config.yaml`:

```yaml
customConfigs:
  pulse:
    allowed_teams:
      - S0123456789   # CS team
  ovad:
    allowed_teams:
      - S0123456789   # CS team
      - S0987654321   # DevOps team
```

The Helm chart creates a ConfigMap, mounts it, and sets `CUSTOM_CONFIG_DIR`
automatically. The same block is also how you toggle other per-agent settings
such as `chat_enabled`.

### Override via Environment Variable (local / Docker)

Set `CUSTOM_CONFIG_DIR` to a directory containing `<agent-id>.yaml` files (a
full `config.yaml` overlay):

```bash
export CUSTOM_CONFIG_DIR=/path/to/custom-config
# Create /path/to/custom-config/pulse.yaml:
# allowed_teams:
#   - S0123456789
```

### How it Works

1. On each slash command, arbetern checks if the agent has `allowed_teams` configured
2. If yes, it calls the Slack `usergroups.users.list` API to check if the user is a member of any allowed group
3. Group memberships are cached for 5 minutes to avoid API spam
4. Denied users see an ephemeral "Access denied" message
5. Deploy overrides (`customConfigs` / `CUSTOM_CONFIG_DIR`) **replace** (not merge) the `config.yaml` value

> **Slack scopes required:** `usergroups:read` for team membership, plus
> `users:read.email` if you use `allowed_teams` as the chat-UI fallback (to
> resolve an authenticated email to its Slack user). Add these to your Slack
> app's OAuth scopes.

### Chat access by email (UI)

`allowed_teams` gates Slack slash commands by Slack user group. The browser
**chat** (`/ui/<agent>/chat` and the underlying `/api/chat` endpoints) is gated
per agent by **two layers, evaluated in order**:

1. **`allowed_emails` (primary).** The address oauth2-proxy verified
   (`X-Auth-Request-Email`) is matched case-insensitively against the list —
   either an exact address or a whole domain. A match grants access.
2. **`allowed_teams` (fallback).** If the email is not in `allowed_emails`,
   arbetern resolves it to a Slack user (`users.lookupByEmail`) and checks
   membership in the agent's Slack user groups — the same `allowed_teams` used
   for slash commands. A match grants access.
3. **Otherwise → `403`.** If neither layer matches the request is denied.

This lets one `allowed_teams` list cover both Slack slash commands and the chat
UI: a user already in an authorized Slack team gets chat access without being
listed individually in `allowed_emails`. When **both** lists are empty the
agent's chat is unrestricted.

Layer 1 requires the [oauth2-proxy SSO](#authentication-sso) in front of the
app: the proxy authenticates the user with Google (or another provider) and
passes the verified address as `X-Auth-Request-Email`, which it also strips from
inbound requests so it can't be spoofed. Layer 2 additionally requires the
`users:read.email` and `usergroups:read` Slack scopes; it fails closed (no
access granted) if the email can't be resolved to a Slack user.

```yaml
customConfigs:
  pulse:
    chat_enabled: true
    allowed_emails:
      - solutions@acme.com   # exact address
      - acme.com             # any address in this domain
    allowed_teams:
      - S0123456789          # fallback: members of this Slack team also get in
```

When access is denied the chat API returns `403` and the UI shows a friendly
"you don't have access" message with the message composer hidden (no dead Send
button). Lookups (email→Slack-ID and team membership) are cached for 5 minutes
to stay within Slack's rate limits.

The same oauth2-proxy-verified email is used to attribute **Jira tickets created
from the chat UI**: the Reporter is set to that user's Jira account, matching the
behavior of Slack commands. See
[Reporter attribution](docs/ATLASSIAN.md#reporter-attribution).

## Per-Agent Credentials (Integration Overrides)

Each agent normally shares the same integration credentials (Salesforce, Atlassian, Chorus, Datadog, NVD, Azure cost). When a single agent needs its own Salesforce app / Atlassian tenant / Datadog account / etc. you can override individual keys *for that agent only* — every key you do not override falls through to the global value.

Overrides use the same kebab-case keys as the chart's `secretValues` map (e.g. `sf-consumer-key`, `atlassian-api-token`, `chorus-api-token`, `dd-api-key-us`, `azure-client-secret`, ...).

### Via Helm (per-agent Secret)

Add a `customCredentials` section to your values file:

```yaml
customCredentials:
  ovad:
    sf-consumer-key: "REPLACE_ME"
    sf-consumer-secret: "REPLACE_ME"
  pulse:
    chorus-api-token: "REPLACE_ME"
    dd-api-key-us: "REPLACE_ME"
    dd-app-key-us: "REPLACE_ME"
```

For every entry the chart provisions a Secret named `arbetern-<agent>-secrets` and mounts it at `/etc/arbetern/agent-credentials/<agent>/` (one file per key). `AGENT_CREDENTIALS_DIR` is set automatically.

When `createSecret: false` (recommended for production) the chart skips creating the Secret resources — provision them yourself with the matching name and the chart will still mount them:

```bash
kubectl create secret generic arbetern-ovad-secrets \
  --from-literal=sf-consumer-key=3MVG9... \
  --from-literal=sf-consumer-secret=...
```

Leave the corresponding `customCredentials.<agent>` map present (even if empty values) so the chart adds the volume mount.

### Via Environment Variable (local / Docker)

Set `AGENT_CREDENTIALS_DIR` to a directory containing one subdirectory per agent. Each file inside the subdirectory is treated as a single override value whose filename matches a kebab-case secret key:

```bash
export AGENT_CREDENTIALS_DIR=/path/to/agent-credentials
# /path/to/agent-credentials/ovad/sf-consumer-key
# /path/to/agent-credentials/ovad/sf-consumer-secret
```

### How it Works

1. At startup each agent's router is built with a per-agent copy of the config (`Config.ForAgent(agentID)` in [config/agent_credentials.go](config/agent_credentials.go))
2. Only integration clients whose credentials actually differ from the global config get rebuilt ([integrations_agent.go](integrations_agent.go)). Everything else reuses the shared global client — no extra connections
3. Supported keys: every kebab-case key from the chart's `secretValues` schema (Atlassian, Salesforce, Chorus, Datadog US + EU, NVD, Azure Cost service-principal, Azure OpenAI, GitHub, Slack). Unknown keys are ignored
4. **Limitation:** AWS credentials are resolved via the SDK chain (ambient env / IRSA), so `aws-*` overrides under `customCredentials` are *not* applied to the AWS client — use the global secret or a per-pod service account instead

## Dashboards

Agents can create **recurring data dashboards** on demand. Ask the agent in Slack:

```
/pulse create dashboard to show me all the details you have from your integrations regarding acme customer, make it sync every 5 minutes
```

The agent composes a dashboard from its allow-listed read-only integration sources
(`jira_search`, `salesforce_query`, `chorus_list_conversations`, `datadog_search_logs`,
`datadog_list_monitors`, `confluence_search`, `github_list_prs`), saves it as JSON at
`dashboards/<agent>/<dashboard-id>.json` in the state bucket, and spins up a background goroutine
that re-runs every source on the requested interval.

**Viewing:** each dashboard has its own page in the console at `/ui/<agent>/dashboard/<id>`
(self-refreshing, inside the top bar and side rail), with the raw JSON at
`/<agent>/dashboard/<id>/data.json`. Every agent card
in the Web UI also shows an **Available dashboards** section listing its short-name
chips — click to open, `×` to delete.

**Lifecycle tools** (exposed to the LLM):

| Tool | Purpose |
|------|---------|
| `create_dashboard` | Compose a dashboard with name, short_name, sync_interval, and a list of sources. |
| `list_dashboards` | List the agent's active dashboards (ids, short names, last-sync times). |
| `delete_dashboard` | Stop the sync goroutine and remove the stored JSON. |

**Configuration:**

- Descriptors and their latest data live at `dashboards/<agent>/<id>.json` in the state bucket (see [State](#state--s3-backend-semantic-user-context)).
- Sync interval is clamped to `[30s, 24h]`; the default is `5m`.
- Each source type maps 1:1 to an existing integration client and is read-only.

**Global cross-agent command:** in any Slack channel, run `/arbetern list dashboards` to
get a single list of every active dashboard across every agent, with clickable view
links. No agent slash-command or LLM round-trip is involved — it reads straight from
the registry.

## Prompt-Driven Dashboards (templates + inputs)

In addition to LLM-composed source dashboards, an agent can own a **prompt
dashboard template** — defined like a workflow, with a natural-language prompt
describing how the report is assembled. Any `{{VAR}}` placeholder in the prompt
becomes a declared **input** (e.g. `{{TENANT}}`). The template is registered via
GitOps at `arbetern/dashboards/<agent>/<id>.json` with `kind: "prompt"`.

This is a generic, **agent-agnostic** framework: any agent can own prompt
dashboards (Pulse's per-account summary is just one example). An instance renders
through the owning agent's own tool-loop, so e.g. an `ovad` prompt dashboard can
compile a per-service reliability report from Datadog + GitHub, a `seihin` one a
per-feature adoption brief, etc. — the use case lives entirely in the external
prompt.

Rendering is driven from the management UI (there is no slash command):

1. Open the template's dashboard page. It shows a **Render** form — one field per
   detected `{{VAR}}` input, plus an optional refresh interval — and a list of
   previously rendered instances.
2. Fill in the inputs and press **Render**. The server substitutes the values,
   runs the prompt through the agent's headless LLM tool-loop (same tools as a
   normal command — Jira, Freshdesk, …), and stores the model's **Markdown**
   report as a per-input **instance** at a slug-stable URL
   (`/ui/<agent>/dashboard/<template-id>-<slug>`).
3. The instance page renders the Markdown (headings, tables, lists, links) and
   **auto-refreshes** on its schedule (the template's `sync_interval`, or the
   per-instance override entered in the form), re-running the prompt each tick.

Rendering is fire-and-forget (like the workflow **run now** button): the render
endpoint returns immediately and the instance page polls until the first report
lands. A prompt dashboard with **no** `{{VAR}}` placeholders is self-contained
and renders in place on its schedule.

From an instance page you can **▶ re-render** it in place (reuses its saved
inputs and the current template prompt) or **⬇ PDF** to export the report via the
browser's print → *Save as PDF* (a print-optimised, document-style layout).

Because the report is produced by the LLM, its exact structure and the
integrations it consults live entirely in the external prompt — no per-tenant
logic is baked into the binary.

## Example `create_dashboard` Prompts per Agent

Every agent with `create_dashboard` available can also build **custom, sync-on-a-
timer dashboards** from its integration sources. Copy a prompt below verbatim as
the body of a slash command.

<details>
<summary><code>/pulse</code> — Customer Success</summary>

```
create dashboard "Acme 360" short-name acme-360 that syncs every 10m with:
- jira_search of JQL `project = ENG AND labels = acme AND resolution = Unresolved ORDER BY priority DESC`
- salesforce_query SOQL `SELECT Id, Name, StageName, CloseDate, Amount FROM Opportunity WHERE Account.Name = 'Acme' AND IsClosed = false`
- chorus_list_conversations with participants_email = @acme.com over the last 30 days, with_trackers: true
```

</details>

<details>
<summary><code>/ovad</code> — DevOps & SRE</summary>

```
create dashboard "Prod incidents today" short-name prod-today, every 5m, with:
- datadog_search_logs query `env:prod status:error` over the last 24h, limit 25
- datadog_list_monitors query `status:alert` limit 25
- github_list_prs for repo infra-live, state open, limit 15
```

</details>

<details>
<summary><code>/agent-q</code> — QA & Test</summary>

```
create dashboard "Flaky tests board" short-name flaky-tests that refreshes every 15m:
- jira_search JQL `labels in (flaky, test-failure) AND statusCategory != Done ORDER BY updated DESC`
- github_list_prs for repo api-service state open limit 20 (to cross-reference open test fixes)
```

</details>

<details>
<summary><code>/goldsai</code> — Security Research</summary>

```
create dashboard "Open vulns this quarter" short-name secops-q, every 30m, with:
- jira_search JQL `project = SEC AND labels = cve AND resolution = Unresolved ORDER BY priority DESC`
- confluence_search cql `label = "security-advisory" AND lastModified > now("-90d")`
```

</details>

<details>
<summary><code>/seihin</code> — Sr. Technical PM</summary>

```
create dashboard "PM intake queue" short-name pm-intake that syncs every 15m:
- jira_search JQL `project = PROD AND status = "Intake" ORDER BY created ASC` max_results 30
- confluence_search cql `label = "pm-rfc" AND lastModified > now("-14d")`
```

</details>

**Tips:** always provide a `short_name` (keeps the agent-card chip compact);
quote JQL/SOQL/CQL exactly as you'd type it in the native UI; start with a
5–15m `sync_interval` (clamped to `[30s, 24h]`); ask the agent
`what integrations do you have` if unsure which sources will resolve.

## Workflows

Agents can also own **recurring or event-triggered workflows** — scheduled
agent invocations that go beyond read-only dashboards. A workflow can open
PRs, post Slack messages, transition Jira tickets, etc., because each tick
re-enters the owning agent's full tool-loop (headless) with a pinned prompt.

Example prompt (the one that kicked off this feature):

```
/ovad create for me a workflow which polls every 5 minutes from jira open bug
tickets from the engineering project with the label "arbetern", reads and executes
upon the bug in github, and sends a slack message to channel C0123456789 that
a PR is created — when you create the PR, set claude as the assignee too so it
will review.
```

The owning agent synthesises a complete, credentialless prompt (channel IDs,
repo names, labels, assignees — everything needed so the tick is reproducible),
persists a JSON descriptor at `workflows/<agent>/<id>.json` in the state
bucket, and starts a goroutine that ticks on the requested cron schedule. Each run's result + error
is appended to the descriptor; the workflow's page in the console
(`/ui/<agent>/workflow/<id>`) renders the run history and auto-refreshes
adaptively (every 3 seconds while a tick is in flight, every 30 seconds
otherwise). The status pill shows `running` while a tick is executing, and the
**Run now** button is disabled until it completes.

### Boot behaviour

On server startup, both the workflow registry and the dashboard registry
**load all descriptors into memory and start their tickers, but do NOT fire
an immediate tick / sync**. Scheduled ticks run on their normal cadence;
user-initiated work (Create, the Run-now button, manual API calls) still
executes immediately. This keeps deploys quiet — a pod roll won't blast
every upstream the moment it comes up.

### GitOps sync (managing workflows + dashboards from a git repo)

Both workflows and dashboards can be **declared in a separate GitHub
repository** and auto-reconciled into their registries by a built-in
poller. Set `workflows.gitops.enabled=true` and/or
`dashboards.gitops.enabled=true` in `values.yaml` (or
`WORKFLOWS_GITOPS_REPO=...` / `DASHBOARDS_GITOPS_REPO=...` on the
deployment) and arbetern will poll the configured repo every 5 minutes
(configurable) for files matching `<basePath>/<agent>/<id>.json`:

```
arbetern/
├── workflows/
│   ├── ovad/
│   │   ├── aws-daily-cost.json
│   │   └── jira-bug-autofix.json
│   └── seihin/
│       └── seihin-application-triage.json
└── dashboards/
    └── ovad/
        └── infra-overview.json
```

Each file uses the same shape as the corresponding on-disk registry
descriptor; runtime fields (run history for workflows, source data for
dashboards) are ignored on read so committing a file pulled from
`/api/workflows/...` or `/api/dashboards/...` Just Works. Items synced
this way are tagged `source = "gitops"` and become read-only in the UI
— the edit button is hidden, a banner links back to the source file in
git, and the API rejects non-`enabled` mutations on workflows /
deletes on either kind. See [docs/GITOPS.md](docs/GITOPS.md) for the
full reference, configuration matrix, and ConfigMap example.

### Cron scheduling (`cron`)

Scheduled workflows fire on a standard 5-field UTC cron expression. The
`cron` field is required for `trigger.type = schedule`; it defaults to
`@every 5m` when omitted on create.

```
0 5 * * *        # daily at 05:00 UTC
0 5 * * 0-4      # Sun–Thu at 05:00 UTC (skip Fri/Sat)
*/15 * * * *     # every 15 minutes
0 9,17 * * 1-5   # weekdays at 09:00 and 17:00 UTC
@every 1h        # every 1 hour from runner start
@daily           # midnight UTC
@hourly          # top of every hour
```

The runner sleeps until the next match of the expression (computed in UTC
via [`robfig/cron/v3`](https://github.com/robfig/cron)) and then fires the
tick in its own goroutine, so a slow tick never blocks the next scheduled
fire of the same workflow. Overlapping ticks are skipped via a per-workflow
busy try-lock so a long-running tick can't double-post or double-PR.

**Restart-resilient catch-up.** On server boot, if the most recent expected
fire time is after the workflow's recorded `last_run`, the runner fires a
single catch-up tick immediately (logged as `schedule:catchup`). This means
a daily 05:00 UTC report is delivered even when the pod was rolled at
05:30 UTC. Fresh creates also fire one catch-up tick because their
`last_run` is empty; manual edits do NOT (saving the form does not trigger
an extra run).

Every workflow prompt is also prefixed with the **real current UTC time**
before being sent to the LLM (`Current UTC time: YYYY-MM-DD HH:MM:SS…`),
so date arithmetic in scheduled reports doesn't rely on the model's
training cutoff.

### PR-writing tools

The `modify_file`, `create_file`, and `regex_replace_file` tools all accept
an optional `pr_body` argument. When supplied, the LLM-authored Markdown is
used verbatim as the PR description (with a single-line `_Automated via
Slack by Real Name (<@user>)_` attribution footer appended); when omitted, a
generic template is used as a fallback. The name comes from the requester's
Slack profile (`users.info`, real name first, then display name) and is
resolved once per run — GitHub does not render `<@U123>`, so without the name
a reviewer only sees an opaque ID. The raw mention stays alongside it as the
stable key back to the Slack profile; when the lookup fails or there is no
Slack identity (a web-chat turn), the footer degrades to the bare mention.

Every body, supplied or fallback, also ends with an invisible HTML comment —
`<!-- arbetern agent=<agent> source=<slack|chat|workflow|dashboard> user=<slack-id> -->`,
with unknown attributes left out. It marks the PR as arbetern-written whatever
entry path opened it, and is how the console's Pull requests page finds the
agents' open PRs; bodies written before it existed are recognised by the
attribution phrases above.

Only the write call that opens a PR establishes its body — later calls
grouped into that same PR ignore their `pr_body` argument.

The same three tools also accept optional `branch_name` and `pr_title`
arguments for prompts that need to enforce a naming convention (e.g.
`<JIRA-KEY>/<agent>-<slug>` head branches and `<JIRA-KEY>/<agent>: …` PR
titles for ticket-driven workflows). When omitted (the default), the
platform auto-generates a unique head branch (`<agent-id>/patch-<unix-ts>`)
and uses `<agent-id>: <description>` as the PR title — unchanged from prior
behavior. When provided, both values are used VERBATIM (no agent-name
prefix is added). They are only honored on the call that opens a PR;
later calls grouped into that same PR ignore them.

`branch_name` is also what controls grouping. Writes that omit it are added
to the PR most recently opened for that repo, so a change spanning several
files lands in one PR. A write that names a *different* branch opens its own
branch and PR instead, cut fresh from the base — that is how a workflow
fixing several unrelated issues in one repo ships one reviewable PR per fix
rather than a single PR for the whole tick. Files belonging to one fix must
therefore be written consecutively, before the next fix's branch is started.

To change a pull request that is **already open** — review feedback, requested
changes, a follow-up ask — the same three tools take `pr_number` (or `pr_url`).
The write is read from and committed onto that PR's head branch, so each round
of feedback updates the existing PR instead of opening another one. The PR must
still be open; a merged or closed one is rejected with the instruction to open a
new PR. Naming an existing remote branch in `branch_name` does the same thing:
the branch is committed onto rather than cut again, and its open PR (if any) is
adopted as the repo's active PR for the rest of the run. This works from any
entry point — a later Slack thread, a web-chat turn, or a scheduled tick — since
it reads the branch and PR from GitHub rather than from in-process state, which
only ever covered PRs opened in the same run or live thread session.

The duplicate guard reinforces this: when a write would open a PR equivalent to
one already open, it is refused with that PR's number and head branch so the
model can retry the same write onto it.

Every PR opened by these tools also requests **GitHub Copilot as a reviewer**
best-effort: a REST attempt with the magic `Copilot` login, falling back to
a GraphQL `requestReviews` mutation that resolves the Copilot bot via
`pullRequest.suggestedReviewers`. Failures are logged and swallowed — PR
creation never fails because Copilot couldn't be added (e.g. repos without
Copilot code review enabled on the plan).

### Threaded Slack replies

The `post_slack_message` tool accepts an optional `thread_ts` argument so
workflows can post one top-level message and then thread follow-ups under
it (e.g. an AWS cost report with the day's GitHub digest threaded
underneath). Capture the `ts` returned by the first `post_slack_message`
call and pass it as `thread_ts` on subsequent calls.

### Design patterns

Workflows are modelled after the four Prefect flow-composition patterns,
adapted for arbetern's LLM-tool-loop execution model:

| Pattern | Coupling | When to use |
|---|---|---|
| **Monoflow** | tight (one LLM call per tick) | Simple recurring tasks: "poll X, do Y". |
| **Flow of subflows** (`tasks`) | medium (sequential, in-process) | Multi-step runs where each step benefits from a smaller, bounded LLM context. |
| **Flow of deployments** (`call_workflow`) | loose (workflow ↔ workflow via tool call) | Composing specialist workflows across agents. |
| **Event-triggered** (`trigger: on_success/on_failure`) | loose (reactive) | Run workflow B whenever workflow A finishes (or fails). |

```
tight coupling                                          loose coupling
───────────────                                          ───────────────
 Monoflow  ──▶  Flow of subflows  ──▶  Flow of deployments  ──▶  Event-triggered
 one prompt     ordered tasks          call_workflow tool        on_success of X
```

**Monoflow** is the default. Supply a single `prompt` — the agent re-runs the
exact same instruction on every cron tick.

**Flow of subflows** splits a workflow into an ordered `tasks` list. Each
task's output is compacted and fed forward as context for the next task's
prompt. Keeps individual LLM calls small and recoverable.

**Flow of deployments** is expressed via the `call_workflow` tool: a parent
workflow's prompt instructs the agent to synchronously invoke one or more
child workflows (possibly owned by different agents) and chain their results.
Child workflows remain independently scheduled.

**Event-triggered** workflows do not tick on their own — they listen. When
any workflow finishes, the registry fires listener workflows whose
`trigger.type` is `on_success` / `on_failure` and whose `trigger.ref`
matches `<agent>/<id>` of the just-finished run. Set `trigger.type: manual`
to disable auto-execution entirely; such workflows only run via the manual
API endpoint.

### Lifecycle tools (exposed to the LLM)

| Tool | Purpose |
|------|---------|
| `create_workflow` | Register a new workflow with name, short_name, cron, and either `prompt` (monoflow) or `tasks` (subflows), and optional `trigger`. |
| `list_workflows` | List this agent's workflows with their ids, patterns, cron schedules, and last-run timestamps. |
| `delete_workflow` | Stop the background execution and remove the stored JSON. |
| `call_workflow` | Run another workflow once and return its final result. The canonical "flow of deployments" primitive. |

Inside a headless workflow tick only `call_workflow` and `list_workflows`
stay available — `create_workflow` / `delete_workflow` are suppressed so a
workflow cannot recursively spawn more workflows.

### Manual triggering

Every workflow has a manual-run endpoint behind the UI IP whitelist:

```bash
curl -X POST https://<host>/api/workflows/<agent>/<id>/run
# → 202 { "accepted": true, "agent": "...", "id": "...", "message": "workflow run queued; …" }
```

This is also how event-triggered and `trigger: manual` workflows are kicked
off the first time.

The run is not executed by the replica that took the request. It is written to
the durable queue in the state bucket and claimed by whichever replica has
capacity, so it survives a pod restart between the request and the run and
spreads the load instead of pinning it to one pod. Event-triggered
(`on_success` / `on_failure`) listeners go through the same queue. Poll
`GET /api/workflows/<agent>/<id>` — the run appears in its history when it
finishes. Delivery is deliberately at-most-once: a tick opens pull requests and
posts to Slack, so a run lost to a crash is cheaper than one replayed after it.
See [docs/STATE.md](docs/STATE.md#deferred-work).

### Configuration

- Descriptors and run history live at `workflows/<agent>/<id>.json` in the
  state bucket (see [State](#state--s3-backend-semantic-user-context)).
- Schedule is a standard 5-field UTC cron expression (`@every 1h`, `@daily`,
  `@hourly` descriptors also accepted); default `@every 5m` when omitted.
- Only a workflow's own agent can create / delete / list it via the LLM tools;
  `call_workflow` can target any agent.

### Helm / state

The chart needs the state bucket and nothing else to persist:

```yaml
state:
  s3:
    arn: arn:aws:s3:::acme-arbetern-state/prod   # required
  vectors:                                       # optional semantic user context
    indexArn: arn:aws:s3vectors:eu-central-1:123456789012:bucket/acme-arbetern-vectors/index/user-context
    embeddingModel: amazon.titan-embed-text-v2:0
```

Grant the pod's IAM role access to the bucket as described in
[docs/AWS.md](docs/AWS.md#required-iam-permissions). The chart deploys a
plain Deployment with no volumes and two replicas by default (with a pod
disruption budget and node spreading); ticks, syncs and GitOps reconciles run
on the replica holding the scheduling lease, so a rolling update never
double-fires a workflow, and Slack thread sessions are shared through the
bucket so a follow-up may be answered by either replica.

### Cross-agent list command

In any Slack channel, run `/arbetern list workflows` to get a single list of
every active workflow across every agent, with clickable view links and
per-workflow pattern labels. This reads directly from the registry — no agent
round-trip, no LLM call.

## Project Structure

```
main.go              # entrypoint, HTTP server, API
middleware.go        # HTTP middleware (IP whitelist, CIDR parsing)
agents/              # agent definitions (one directory per agent)
  prompts.yaml       # global prompts shared by all agents (e.g. security)
  agent-q/
    config.yaml      # agent metadata + RBAC config
    prompts.yaml     # QA & Test Engineering agent prompts
  goldsai/
    config.yaml
    prompts.yaml     # Security Research agent prompts
  ovad/
    config.yaml
    prompts.yaml     # DevOps & SRE agent prompts
  pulse/
    config.yaml
    prompts.yaml     # Customer Success Engineering agent prompts
  seihin/
    config.yaml
    prompts.yaml     # Sr. Technical Product Manager agent prompts
commands/            # intent routing, debug/general handlers, per-user context store
config/              # env var loading
internal/store/      # S3 state backend: cached documents, conditional writes, leases
internal/queue/      # durable work queue in the same bucket (claims by conditional write)
internal/vectors/    # S3 Vectors index client (semantic user context)
github/              # GitHub REST API client (repos, PRs, files, workflows)
llm/                 # LLM inference client + tool types (GitHub Models, Azure OpenAI, AWS Bedrock)
atlassian/           # Atlassian Cloud REST API client (Jira + Confluence)
nvd/                 # NVD (National Vulnerability Database) CVE API client
salesforce/          # Salesforce REST API client (SOQL queries, OAuth 2.0)
chorus/              # Chorus (ZoomInfo) REST API client (call intelligence, deal momentum)
google/              # Google Drive + Sheets client (service-account JWT, shared-folder discovery, streaming reads, batched writes)
document360/         # Document360 v3 client (read-only knowledge-base search, categories, articles)
slack/               # Slack webhook handler + response helpers
prompts/             # YAML prompt loader + agent discovery
dashboards/          # dashboard registry, sync runner, executor + CRUD API
workflows/           # workflow engine (monoflow / subflows / event-triggered) + CRUD API
billing/             # usage & billing ledger (per agent / model / source / user / workflow) + summary API
metrics/             # performance series (turn / model-call / tool latency, outcomes) + summary API
skills/              # custom skill registry (instruction blocks appended to agent prompts) + API
mcp/                 # MCP connector registry, Streamable HTTP JSON-RPC client, tool exposure + API
ui/                  # embedded management console (index.html shell, app.css, app.js)
helm/                # Helm chart
docs/                # setup guides (Slack, GitHub PAT, Atlassian)
```

## Customizing Prompts

Edit any `agents/<name>/prompts.yaml` to change LLM behavior without recompiling. Keys: `intro`, `security`, `classifier`, `debug`, `general`.

Global prompts are defined in `agents/prompts.yaml` and inherited by every agent. Each key there is joined into every system prompt the agent builds, so a rule added to that file applies to all agents from every entry point — Slack commands, thread replies, the chat UI, scheduled workflow ticks and dashboard renders. Agent-specific prompts override globals by key.

Keys prefixed `output_` are the exception: they are per-destination, and only the one matching where the turn's answer is going is injected. The same answer is read by Slack, which renders no Markdown, by the console's chat view, which renders GitHub-flavoured Markdown including tables, and by a stored dashboard report — so the destination, not the agent, decides the syntax.

| Global key | What it governs |
|---|---|
| `security` | Scope policy, prompt-injection and secret-handling rules |
| `user_identity` | How the pre-resolved requester is referenced across integrations |
| `confluence_tools` | The Confluence tools and the format their bodies take |
| `output_slack` | Slack mrkdwn rules: `*bold*`, `<url\|text>`, no headings and no Markdown tables — tabular data goes out as a list or a fixed-width code block |
| `output_chat` | GitHub-flavoured Markdown for the console's chat view: headings, `[text](url)`, fenced code, and Markdown tables for anything tabular |
| `output_workflow` | A scheduled tick's result, read in Slack and stored in run history: Slack mrkdwn, lead with what changed |
| `output_dashboard` | The answer *is* the page: Markdown with tables, figures first, no greeting or sign-off |
| `action_first_response` | No pre-action acknowledgements; report completed results |
| `code_comments` | Comment policy for every code change the agent authors |
| `repo_agent_instructions` | Reading a repository's own CLAUDE.md / AGENTS.md before changing it |
| `pull_request_updates` | Committing follow-up work onto the existing PR instead of opening another |
| `plan_then_batch` | Plan the whole change set first, ask only where the plan forks, execute in batches, verify before handing off |
| `clarify_before_expedition` | When an open-ended ask must pause for one round of questions |

Every `output_` block also says the same thing about tool output: structured data a tool or MCP connector returns — JSON, a Markdown table, HTML, a CSV — is input, not an answer, and must be re-rendered for the destination rather than pasted through. MCP payloads additionally carry that reminder inline, prefixed to the tool result itself, because that is the last thing the model reads before it writes.

The `plan_then_batch` block exists because an unplanned agent run spreads one
logical change across dozens of single-file commits, sometimes undoing its own
earlier steps. It requires the agent to settle the change set before the first
write, group commits by logical change rather than by file, leave no scratch or
self-reverting commits, and confirm checks are green before asking for review.

**Where the plan shows up.** The agent writes it as the text accompanying its
first batch of tool calls, which costs no extra message and no round-trip. That
text is kept in the turn — so the model can still see its own plan on round 30
of a long loop — and is surfaced in the in-flight progress note once a turn
passes 45 seconds, as a blockquote under the status line in Slack and under the
pending bubble in the chat UI. A turn that finishes sooner shows nothing, so
short tasks stay silent and the agent never has to judge whether the work is
"big enough" to announce. The user can read the plan while the work runs and
interrupt a wrong one early, which is the point: no approval step, no waiting.

## Skills

A skill is an instruction block appended to an agent's system prompt. The
**Skills** page lists two kinds:

- **Built-in** — every block of `agents/prompts.yaml` (applies to all agents)
  and every agent-specific block or override in `agents/<id>/prompts.yaml`.
  Read-only; change them in the prompt files.
- **Custom** — written in the UI (`POST /api/skills`), stored as JSON at
  `skills/<id>.json` in the state bucket, and appended after the agent's own
  prompt on every Slack, chat and workflow turn. A skill can target specific agents or all of them,
  and can be disabled without deleting it. Adding, changing or deleting a skill
  follows the targeted agents' `allowed_emails` / `allowed_teams`: the requester
  must be allowed to use every agent the skill applies to, and the console shows
  the other agents' skills read-only.

## MCP & Connectors

Model Context Protocol servers can be registered from the **MCP & Connectors**
page or via `POST /api/mcp`. Testing a connector performs the MCP handshake and
`tools/list`; the discovered tools are then exposed to the allowed agents as
`mcp_<connector>_<tool>` in every tool loop, and calls are proxied through
`tools/call`. Header values may reference environment variables as `${NAME}`
so tokens stay in the Secret rather than in the state bucket; stored literal
values are masked in API responses. Set `mcp.adminTeams` in the chart to limit
who may add or change connectors to members of those Slack user groups. See [docs/MCP.md](docs/MCP.md) for the supported
transport, limits and roadmap.

## Integrations

| Integration | Documentation | Required By |
|---|---|---|
| Slack | [docs/SLACK_BOT.md](docs/SLACK_BOT.md) | All agents |
| GitHub | [docs/GITHUB_PAT.md](docs/GITHUB_PAT.md) | ovad, agent-q, goldsai |
| Atlassian (Jira + Confluence) | [docs/ATLASSIAN.md](docs/ATLASSIAN.md) | seihin, ovad, agent-q, goldsai, pulse |
| NVD | [NVD API](https://nvd.nist.gov/developers) | goldsai |
| Salesforce | [docs/SALESFORCE.md](docs/SALESFORCE.md) | pulse |
| Chorus / ZoomInfo | [docs/CHORUS.md](docs/CHORUS.md) | pulse |
| AWS Cost Explorer + S3 + Athena | [docs/AWS.md](docs/AWS.md) | ovad (and any agent running AWS cost workflows) |
| Azure Cost Management | [docs/AZURE.md](docs/AZURE.md) | ovad (and any agent running Azure cost workflows) |
| Databricks SQL | [docs/DATABRICKS.md](docs/DATABRICKS.md) | ovad, pulse |
| ClickHouse Cloud | [docs/CLICKHOUSE.md](docs/CLICKHOUSE.md) | ovad only |
| Freshworks (Freshdesk + Freshchat + CRM) | [docs/FRESHWORKS.md](docs/FRESHWORKS.md) | pulse, seihin |
| Document360 | [docs/DOCUMENT360.md](docs/DOCUMENT360.md) | pulse only |
| Google Drive / Sheets | [docs/GOOGLE.md](docs/GOOGLE.md) | pulse only |
| Headroom (LLM compression) | [docs/HEADROOM.md](docs/HEADROOM.md) | Optional infra — all backends |

## Contributing

Pull requests are disabled on this repository. Contributions are accepted through issues only — please [open an issue](https://github.com/justmike1/arbetern/issues) to report bugs, request features, or propose changes. For anything else, contact the maintainer directly via [@justmike1](https://github.com/justmike1).

## Author & Maintainer

**Mike Joseph** — [@justmike1](https://github.com/justmike1)

## License

This project is licensed under the Apache License 2.0 — see the [LICENSE](LICENSE) file for details.

---

If you find this project useful, please consider giving it a ⭐!
