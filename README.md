# arbetern

[![Stars](https://img.shields.io/github/stars/justmike1/arbetern?style=social)](https://github.com/justmike1/arbetern/stargazers)
[![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![License](https://img.shields.io/github/license/justmike1/arbetern)](LICENSE)
[![Docker](https://img.shields.io/badge/Docker-ready-2496ED?logo=docker&logoColor=white)](Dockerfile)

*Yiddish for "workers." (with a typo, but it's cooler)*

An orchestration platform for AI agents in the enterprise. Each agent lives in its own directory under `agents/`, with dedicated prompts and a defined professional scope. Arbetern provides the runtime, routing, UI, and integrations — agents bring the expertise.

### Screenshots

UI screenshots — home, dashboards, workflow grid, workflow editor — are in [screenshots/SCREENSHOTS.md](screenshots/SCREENSHOTS.md).

### Architecture — [Bernoulli Naive Bayes](https://en.wikipedia.org/wiki/Naive_Bayes_classifier) by Design

Arbetern's end-to-end request resolution pipeline maps naturally to a **Bernoulli Naive Bayes** model. A user message enters as raw text and exits as a fully resolved response through a series of independent binary feature evaluations — no model-context protocol (MCP), no external orchestrator, no shared state bus.

**1. Agent dispatch (prior selection).** Slack routes each slash command (`/ovad`, `/pulse`, `/goldsai`, …) to a dedicated HTTP handler. The agent ID acts as the **class prior** — it determines which prompt set, RBAC policy, and tool palette apply before any content is evaluated. Each agent is an isolated classifier with its own feature weights (prompts) and feature space (available tools).

**2. Intent classification (binary feature scan).** The router inspects the lowercased message against keyword lists — each keyword is a **binary Bernoulli feature** (present = 1, absent = 0). Features are evaluated independently: `isIntroIntent` checks one feature set, `isDebugIntent` checks another, and `requiresAction` acts as a **conditional exclusion** (if action keywords fire, the debug class is suppressed). The first matching class wins — the `switch` ordering encodes implicit priors. No feature influences the evaluation of another, mirroring the Naive Bayes independence assumption.

**3. Tool-loop execution (iterative posterior update).** The general handler enters a bounded loop: send the message + available tools to the LLM, receive tool calls, execute them, feed results back. Each iteration refines the response — analogous to **updating the posterior** as new evidence (tool results) arrives. The tool palette itself is feature-gated: each integration client exposes a `Ready()` boolean, and tools only appear in the LLM's function list when their feature is true. The LLM never sees tools for disconnected integrations, so the feature space dynamically shrinks or grows based on runtime state.

**4. Model selection (feature-conditional class switch).** The system starts with the general model and dynamically switches to the code model when code-related tool calls are detected. This is a **conditional class reassignment** — the observation of a specific feature (code tool invocation) triggers a switch to a more specialized classifier mid-inference, without restarting the loop.

**5. Thread sessions (temporal feature memory).** After the initial response, a session is registered on the Slack thread. Follow-up messages bypass the slash command and re-enter the same router with accumulated conversation history. This gives the classifier **temporal features** — prior messages act as additional binary evidence for subsequent classifications within the same session window.

Every layer — agent selection, intent routing, tool availability, model switching, session continuity — operates as an independent binary decision. There is no sequential boosting (each stage does not correct the previous one), no ensemble voting (a single pass decides), and no external orchestration layer. The system is the product of independent feature states, which is the core assumption of Bernoulli Naive Bayes.

## Current Agents

| Agent | Profession | Description |
|---|---|---|
| **ovad** | DevOps & SRE Engineer | Debugs CI/CD failures, reads/modifies repo files, opens PRs, searches Datadog logs/monitors/infrastructure — all from a Slack slash command |
| **agent-q** | QA & Test Engineer | Analyzes test failures, reviews test coverage, suggests test cases, and triages flaky tests |
| **goldsai** | Security Researcher | Assesses CVE impact on your codebase, audits dependencies, reviews code for vulnerabilities, and recommends remediation |
| **seihin** (製品) | Sr. Technical Product Manager | Reviews and refines Jira tickets, rewrites descriptions with PM best practices, manages ticket quality at scale |
| **pulse** | Customer Success Engineer | Tracks account health, surfaces renewal signals from Salesforce, analyzes call intelligence and deal momentum from Chorus, manages CS workflows, and coordinates with Jira |

## Quick Start

### Prerequisites

- Go 1.26+
- A Slack app with a slash command pointing to `/<agent>/webhook` (see [docs/SLACK_BOT.md](docs/SLACK_BOT.md))
- A GitHub PAT with repo access (see [docs/GITHUB_PAT.md](docs/GITHUB_PAT.md))
- (Optional) Azure OpenAI credentials for LLM inference

### Environment Variables

| Variable | Required | Description |
|---|---|---|
| `SLACK_BOT_TOKEN` | yes | Slack bot OAuth token (`xoxb-...`) |
| `SLACK_SIGNING_SECRET` | yes | Slack app signing secret |
| `GITHUB_TOKEN` | yes* | GitHub PAT (*or* use Azure OpenAI) |
| `GENERAL_MODEL` | no | General/default model ID (default: `openai/gpt-4o`) |
| `CODE_MODEL` | no | Model/deployment used for code-related tasks — reading, reviewing, searching, and modifying code in GitHub (default: same as `GENERAL_MODEL`) |
| `AZURE_OPEN_AI_ENDPOINT` | no | Azure OpenAI endpoint URL |
| `AZURE_API_KEY` | no | Azure OpenAI API key |
| `PORT` | no | HTTP port (default: `8080`) |
| `ATLASSIAN_URL` | no | Atlassian instance URL (e.g. `https://yourorg.atlassian.net`) |
| `ATLASSIAN_EMAIL` | no | Atlassian service account email (Basic Auth) |
| `ATLASSIAN_API_TOKEN` | no | Atlassian API token (Basic Auth) |
| `JIRA_PROJECT` | no | Default Jira project key (e.g. `ENG`) |
| `ATLASSIAN_CLIENT_ID` | no | Atlassian OAuth 2.0 client ID (for client-credentials flow — alternative to Basic Auth with `ATLASSIAN_EMAIL`/`ATLASSIAN_API_TOKEN`) |
| `ATLASSIAN_CLIENT_SECRET` | no | Atlassian OAuth 2.0 client secret |
| `APP_URL` | no | Public app URL (used for Jira ticket stamps) |
| `UI_ALLOWED_CIDRS` | no | Comma-separated CIDRs allowed to access the UI |
| `SLACK_APP_TOKEN` | no | Slack app-level token (`xapp-...`) for Socket Mode — enables thread follow-ups without slash commands (see [docs/SLACK_BOT.md](docs/SLACK_BOT.md#socket-mode-thread-follow-ups)) |
| `THREAD_SESSION_TTL` | no | Duration a thread session stays active (default: `3m`). Go duration format, e.g. `5m`, `2m30s` |
| `MAX_TOOL_ROUNDS` | no | Max LLM tool-call rounds per request (default: `50`). Increase for complex multi-file tasks |
| `SHOW_USAGE_STAMP` | no | Appends model/token usage metadata to Slack replies. Enabled by default; set to `false` to hide the usage/cost line. |
| `NVD_API_KEY` | no | NVD (National Vulnerability Database) API key for CVE lookups. Get one free at <https://nvd.nist.gov/developers/request-an-api-key>. Without a key, requests are rate-limited (~5 req/30s vs ~50 req/30s with a key) |
| `SF_CONSUMER_KEY` | no | Salesforce Connected App consumer key (OAuth 2.0 client credentials flow) |
| `SF_CONSUMER_SECRET` | no | Salesforce Connected App consumer secret |
| `SF_LOGIN_URL` | no | Salesforce login URL (default: `https://login.salesforce.com`). Use `https://test.salesforce.com` for sandbox orgs |
| `CHORUS_API_TOKEN` | no | Chorus (ZoomInfo) API token for call intelligence and deal momentum. Generated in Chorus → Personal Settings |
| `CHORUS_BASE_URL` | no | Chorus API base URL (default: `https://chorus.ai`). Override for a custom or on-prem endpoint |
| `DD_API_KEY_US` | no | Datadog US (datadoghq.com) API key. Found in Organization Settings → API Keys |
| `DD_APP_KEY_US` | no | Datadog US Application key. Found in Organization Settings → Application Keys |
| `DD_API_KEY_EU` | no | Datadog EU (datadoghq.eu) API key. Found in Organization Settings → API Keys |
| `DD_APP_KEY_EU` | no | Datadog EU Application key. Found in Organization Settings → Application Keys |
| `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` | no | AWS static credentials. Any of these (or `AWS_PROFILE`, or EKS IRSA via `AWS_WEB_IDENTITY_TOKEN_FILE`+`AWS_ROLE_ARN`) enables the Cost Explorer tools (`aws_get_cost_and_usage`, `aws_get_cost_forecast`, `aws_list_dimension_values`). The IAM principal needs `ce:GetCostAndUsage`, `ce:GetCostForecast`, `ce:GetDimensionValues`. Note: each CE API call costs $0.01 |
| `AWS_REGION` | no | Region used to sign Cost Explorer SigV4 calls (default `us-east-1`, the only region that hosts the CE endpoint). Cost data returned is account-global regardless |
| `CUSTOM_PROMPTS_DIR` | no | Directory containing custom prompt YAML files that are **appended** to built-in agent prompts. Used for org-specific context via Kubernetes ConfigMap. Set automatically by the Helm chart when `customPrompts` is configured |
| `AGENT_RBAC_DIR` | no | Directory containing per-agent RBAC overrides (`<agent-id>.yaml` with `allowed_teams` list). Overrides `config.yaml` allowed_teams at deploy time. Set automatically by the Helm chart when `agentRBAC` is configured |
| `UI_HEADER` | no | Custom header text for the web UI (default: `arbetern`) |
| `DASHBOARDS_DIR` | no | Directory where dashboard JSON snapshots are persisted (default: `./data/dashboards`). Set automatically by the Helm chart when `dashboards.enabled` is true |
| `WORKFLOWS_DIR` | no | Directory where workflow JSON descriptors + run history are persisted (default: `./data/workflows`). Set automatically by the Helm chart when `workflows.enabled` is true |
| `WORKFLOWS_GITOPS_REPO` | no | Enables GitOps sync for workflows: poll `<owner>/<repo>` for `<basePath>/<agent>/<id>.json` descriptors and reconcile them into the registry. Reuses `GITHUB_TOKEN`. See [docs/GITOPS.md](docs/GITOPS.md) |
| `WORKFLOWS_GITOPS_OWNER` | no | Repo owner for workflows GitOps sync. Defaults to the bot's resolved owner |
| `WORKFLOWS_GITOPS_BRANCH` | no | Branch for workflows GitOps sync. Defaults to the repo's default branch |
| `WORKFLOWS_GITOPS_BASE_PATH` | no | Base path inside the repo for workflow JSONs (default: `arbetern/workflows`) |
| `WORKFLOWS_GITOPS_INTERVAL` | no | Poll interval (Go duration, e.g. `5m`, `30s`). Default `5m`, minimum `30s` |
| `WORKFLOWS_GITOPS_PRUNE` | no | When `true`, locally-managed workflows that disappear from git are deleted. Default `false` |
| `DASHBOARDS_GITOPS_REPO` | no | Enables GitOps sync for dashboards (same model as workflows). Default base path: `arbetern/dashboards`. See [docs/GITOPS.md](docs/GITOPS.md) |
| `DASHBOARDS_GITOPS_OWNER` / `DASHBOARDS_GITOPS_BRANCH` / `DASHBOARDS_GITOPS_BASE_PATH` / `DASHBOARDS_GITOPS_INTERVAL` / `DASHBOARDS_GITOPS_PRUNE` | no | Same semantics as the `WORKFLOWS_GITOPS_*` knobs above |

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

Visit `/ui/` to see all registered agents. Click an agent card to view its prompts (read-only). The UI auto-discovers agents from the `agents/` directory.

- Drop a `logo.png` into `ui/` to replace the default icon
- Set `UI_HEADER` env var to customize the navbar title

## Adding a New Agent

1. Create a directory under `agents/`:
   ```
   agents/my-agent/prompts.yaml
   ```
2. Define prompts in the YAML file (keys like `security`, `classifier`, `general`, `debug`, etc.)
3. Rebuild and deploy — the agent will appear in the UI and get a webhook at `/<agent-name>/webhook`
4. Create a Slack slash command pointing to `https://<your-host>/<agent-name>/webhook`

> **Note:** Each agent directory under `agents/` is automatically discovered at startup and registered with its own webhook route (`/<agent>/webhook`). Create a Slack slash command per agent pointing to the corresponding path.

## Custom Prompts (Org-Specific Context)

You can append org-specific context to any agent's prompts without modifying the built-in `agents/*/prompts.yaml` files. Custom prompts are **appended** to existing prompt keys — they never override the originals.

### Via Helm (Kubernetes ConfigMap)

Add a `customPrompts` section to your values file:

```yaml
customPrompts:
  ovad:
    general: |
      Our GitHub org is "acme-corp". Default repo for infra is "infra-live".
      Terraform state is in S3 bucket "acme-tf-state".
      Production cluster is EKS "prod-us-east-1".
  goldsai:
    general: |
      All Python services must use Python >= 3.13.11.
      Container base images are in ECR at 123456789.dkr.ecr.us-east-1.amazonaws.com.
```

The Helm chart creates a ConfigMap, mounts it, and sets `CUSTOM_PROMPTS_DIR` automatically.

### Via Environment Variable (local / Docker)

Set `CUSTOM_PROMPTS_DIR` to a directory containing `<agent-id>.yaml` files:

```bash
export CUSTOM_PROMPTS_DIR=/path/to/custom-prompts
# Create /path/to/custom-prompts/ovad.yaml with prompt key/value pairs
```

## Agent RBAC (Team-Based Access Control)

Restrict which Slack user groups (teams) can access each agent. When `allowed_teams` is set for an agent, only members of those Slack user groups can invoke it. Empty list = open to everyone.

### Default Config (`agents/<id>/config.yaml`)

Each agent's `config.yaml` has an `allowed_teams` field:

```yaml
name: Pulse
allowed_teams:
  - S0A6S3KNNLW   # CS team user group ID
```

### Override via Helm (Kubernetes ConfigMap)

Add an `agentRBAC` section to your values file to override `config.yaml` at deploy time:

```yaml
agentRBAC:
  pulse:
    - S0A6S3KNNLW   # CS team
  ovad:
    - S0A6S3KNNLW   # CS team
    - S0B7T4LOOLX   # DevOps team
```

The Helm chart creates a ConfigMap, mounts it, and sets `AGENT_RBAC_DIR` automatically.

### Override via Environment Variable (local / Docker)

Set `AGENT_RBAC_DIR` to a directory containing `<agent-id>.yaml` files:

```bash
export AGENT_RBAC_DIR=/path/to/rbac
# Create /path/to/rbac/pulse.yaml:
# allowed_teams:
#   - S0A6S3KNNLW
```

### How it Works

1. On each slash command, arbetern checks if the agent has `allowed_teams` configured
2. If yes, it calls the Slack `usergroups.users.list` API to check if the user is a member of any allowed group
3. Group memberships are cached for 5 minutes to avoid API spam
4. Denied users see an ephemeral "Access denied" message
5. Deploy overrides (`AGENT_RBAC_DIR`) **replace** (not merge) the `config.yaml` value

> **Slack scope required:** `usergroups:read` — add this to your Slack app's OAuth scopes.

## Dashboards

Agents can create **recurring data dashboards** on demand. Ask the agent in Slack:

```
/pulse create dashboard to show me all the details you have from your integrations regarding paypal customer, make it sync every 5 minutes
```

The agent composes a dashboard from its allow-listed read-only integration sources
(`jira_search`, `salesforce_query`, `chorus_list_conversations`, `datadog_search_logs`,
`datadog_list_monitors`, `confluence_search`, `github_list_prs`), saves it as JSON at
`<DASHBOARDS_DIR>/<agent>/<dashboard-id>.json`, and spins up a background goroutine
that re-runs every source on the requested interval.

**Viewing:** each dashboard is served at `/<agent>/dashboard/<id>` as a self-refreshing
HTML page, with the raw JSON at `/<agent>/dashboard/<id>/data.json`. Every agent card
in the Web UI also shows an **Available dashboards** section listing its short-name
chips — click to open, `×` to delete.

**Lifecycle tools** (exposed to the LLM):

| Tool | Purpose |
|------|---------|
| `create_dashboard` | Compose a dashboard with name, short_name, sync_interval, and a list of sources. |
| `list_dashboards` | List the agent's active dashboards (ids, short names, last-sync times). |
| `delete_dashboard` | Stop the sync goroutine and remove the stored JSON. |

**Configuration:**

- `DASHBOARDS_DIR` — where JSON snapshots are persisted (default `./data/dashboards`).
- Sync interval is clamped to `[30s, 24h]`; the default is `5m`.
- Each source type maps 1:1 to an existing integration client and is read-only.

**Helm / persistence:**

Dashboards share a single PVC with workflows (see the [Workflows](#workflows) section
for the full persistence block). To enable the feature:

```yaml
dashboards:
  enabled: true
workflows:
  enabled: true

# One PVC, two subdirectories — /var/lib/arbetern/{dashboards,workflows}.
persistence:
  enabled: true
  mountPath: /var/lib/arbetern
  persistentVolumeClaim:
    enabled: true                   # false = emptyDir (rebuilt on each roll)
    size: 2Gi
    storageClass: "gp3"
```

> **Pod security:** the Helm chart sets `podSecurityContext.fsGroup=65532` (matching the
> `distroless/static:nonroot` user) so kubelet chowns the mounted dashboards volume on
> pod start. Without this, the non-root process cannot create `<agent>/` subdirectories
> inside the PVC and every sync fails with `mkdir: permission denied`.

**Global cross-agent command:** in any Slack channel, run `/arbetern list dashboards` to
get a single list of every active dashboard across every agent, with clickable view
links. No agent slash-command or LLM round-trip is involved — it reads straight from
the registry.

## Account Health Dashboards (`/<agent> dashboard <name>`)

In addition to LLM-composed source dashboards, any agent with Salesforce configured
gets a **first-class `dashboard` subcommand** that runs a fixed, deterministic
pipeline and replies with a Block Kit summary + a link to the full HTML view:

```
/pulse dashboard Sprout Social
/pulse dashboard Sprout Social --refresh
```

Pipeline on each invocation:

1. **Resolve** the account via fuzzy Salesforce `LIKE` search (exact match > prefix > contains).
2. **Fan out in parallel** to every configured integration — Salesforce opps, Jira
   open tickets mentioning the account, Chorus engagements (last 45d, filtered by
   account email domain when derivable from `Account.Website`), Datadog monitors
   tagged for the account or currently alerting.
3. **Compute a weighted health score** (0–100) from the signals below.
4. **Persist** a Kind=`account` dashboard under a slug-stable URL
   (`/<agent>/dashboard/acct-<slug>`) and cache the snapshot at
   `<DASHBOARDS_DIR>/_cache/account/<slug>.json`.
5. **Reply** with a Slack Block Kit summary: score badge, top 3 risks, top 3 action
   items, signal breakdown, and an **Open full dashboard →** button.

**Caching:** the first request of the day fetches fresh data and caches it for 24h.
Subsequent requests serve from cache unless `--refresh` (or `-r`) is passed.

**Score bands:** `80+` green · `60–79` yellow · `40–59` orange · `<40` red.

**Signal weights** (sum to 100):

| Signal | Weight | Key penalties |
|---|---|---|
| Ticket Health | 25 | Open P0/P1 (-15 ea), stale >14d (-10 ea), unassigned (-5 ea) |
| Infra Stability | 20 | Monitor in ALERT (-20 ea), WARN (-10 ea) |
| Engagement (Chorus) | 20 | No calls 30d (-20), competitor mentions (-10 ea), overdue items (-5 ea) |
| Comms (Slack/Email) | 15 | *Not yet instrumented — full credit in v1.* |
| License & Commercial | 20 | Renewal <60d with no open opp (-30) |

The HTML view renders a bar chart of signal score-vs-weight via Chart.js (loaded
from CDN), a coloured score badge, and the full source-panel tables underneath.

## Example `create_dashboard` Prompts per Agent

Every agent with `create_dashboard` available can also build **custom, sync-on-a-
timer dashboards** from its integration sources. Copy a prompt below verbatim as
the body of a slash command:

### `/pulse` — Customer Success
```
create dashboard "Paypal 360" short-name paypal-360 that syncs every 10m with:
- jira_search of JQL `project = WAK AND labels = paypal AND resolution = Unresolved ORDER BY priority DESC`
- salesforce_query SOQL `SELECT Id, Name, StageName, CloseDate, Amount FROM Opportunity WHERE Account.Name = 'Paypal' AND IsClosed = false`
- chorus_list_conversations with participants_email = @paypal.com over the last 30 days, with_trackers: true
```

### `/ovad` — DevOps & SRE
```
create dashboard "Prod incidents today" short-name prod-today, every 5m, with:
- datadog_search_logs query `env:prod status:error` over the last 24h, limit 25
- datadog_list_monitors query `status:alert` limit 25
- github_list_prs for repo infra-live, state open, limit 15
```

### `/agent-q` — QA & Test
```
create dashboard "Flaky tests board" short-name flaky-tests that refreshes every 15m:
- jira_search JQL `labels in (flaky, test-failure) AND statusCategory != Done ORDER BY updated DESC`
- github_list_prs for repo api-service state open limit 20 (to cross-reference open test fixes)
```

### `/goldsai` — Security Research
```
create dashboard "Open vulns this quarter" short-name secops-q, every 30m, with:
- jira_search JQL `project = SEC AND labels = cve AND resolution = Unresolved ORDER BY priority DESC`
- confluence_search cql `label = "security-advisory" AND lastModified > now("-90d")`
```

### `/seihin` — Sr. Technical PM
```
create dashboard "PM intake queue" short-name pm-intake that syncs every 15m:
- jira_search JQL `project = PROD AND status = "Intake" ORDER BY created ASC` max_results 30
- confluence_search cql `label = "pm-rfc" AND lastModified > now("-14d")`
```

Tips for good results:
- Always provide a `short_name` so the chip on the agent card stays compact.
- Quote JQL / SOQL / CQL values exactly as you'd type them in the native UI.
- Start with a 5–15m `sync_interval`; the system clamps below 30s and above 24h.
- Each integration must be **configured** — ask the agent `what integrations do you
  have` if you aren't sure which sources will resolve.

## Workflows

Agents can also own **recurring or event-triggered workflows** — scheduled
agent invocations that go beyond read-only dashboards. A workflow can open
PRs, post Slack messages, transition Jira tickets, etc., because each tick
re-enters the owning agent's full tool-loop (headless) with a pinned prompt.

Example prompt (the one that kicked off this feature):

```
/ovad create for me a workflow which polls every 5 minutes from jira open bug
tickets from the wakanda project with the label "arbetern", reads and executes
upon the bug in github, and sends a slack message to channel C02S5BP9LHX that
a PR is created — when you create the PR, set claude as the assignee too so it
will review.
```

The owning agent synthesises a complete, credentialless prompt (channel IDs,
repo names, labels, assignees — everything needed so the tick is reproducible),
persists a JSON descriptor at `<WORKFLOWS_DIR>/<agent>/<id>.json`, and starts
a goroutine that ticks on the requested cron schedule. Each run's result + error
is appended to the descriptor; the viewer at `/<agent>/workflow/<id>` renders
the run history and auto-refreshes adaptively (every 3 seconds while a tick
is in flight, every 30 seconds otherwise). The header badge shows `running…`
(blue, pulsing) while a tick is executing, and the **Run now** button is
disabled until it completes.

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
│   │   └── arbetern-autofix.json
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
Slack by <@user>_` attribution footer appended); when omitted, a generic
template is used as a fallback. Only the FIRST write call per repo per tick
establishes the PR body — subsequent calls grouped into the same PR ignore
their `pr_body` argument.

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
# → { "result": "<final output>" } or { "error": "...", "result": "..." }
```

This is also how event-triggered and `trigger: manual` workflows are kicked
off the first time.

### Configuration

- `WORKFLOWS_DIR` — where descriptors are persisted (default `./data/workflows`).
- Schedule is a standard 5-field UTC cron expression (`@every 1h`, `@daily`,
  `@hourly` descriptors also accepted); default `@every 5m` when omitted.
- Only a workflow's own agent can create / delete / list it via the LLM tools;
  `call_workflow` can target any agent.

### Helm / persistence

Workflows share a single PVC with dashboards — one volume, two sub-directories:

```yaml
dashboards:
  enabled: true
workflows:
  enabled: true

persistence:
  enabled: true
  mountPath: /var/lib/arbetern           # DASHBOARDS_DIR = $mountPath/dashboards
                                         # WORKFLOWS_DIR  = $mountPath/workflows
  persistentVolumeClaim:
    enabled: true                        # false = emptyDir (rebuilt every roll)
    size: 2Gi
    storageClass: "gp3"
```

When `persistentVolumeClaim.enabled=false`, the mount falls back to an
`emptyDir` and both dashboards and workflows are rebuilt from scratch on each
pod roll.

> **Pod security:** the Helm chart sets `podSecurityContext.fsGroup=65532`
> (matching the `distroless/static:nonroot` user) so kubelet chowns the shared
> volume on pod start, letting the non-root process create the `<agent>/`
> sub-directories.

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
commands/            # intent routing, debug/general handlers
config/              # env var loading
github/              # GitHub REST API client (repos, PRs, files, workflows)
llm/                 # LLM inference client + tool types (Azure OpenAI, GitHub Models)
atlassian/           # Atlassian Cloud REST API client (Jira + Confluence)
nvd/                 # NVD (National Vulnerability Database) CVE API client
salesforce/          # Salesforce REST API client (SOQL queries, OAuth 2.0)
chorus/              # Chorus (ZoomInfo) REST API client (call intelligence, deal momentum)
slack/               # Slack webhook handler + response helpers
prompts/             # YAML prompt loader + agent discovery
dashboards/          # dashboard registry, sync runner, executor, embedded HTML viewer
workflows/           # workflow engine (monoflow / subflows / event-triggered) + embedded viewer
ui/                  # embedded web UI (agent manager)
helm/                # Helm chart
docs/                # setup guides (Slack, GitHub PAT, Atlassian)
```

## Customizing Prompts

Edit any `agents/<name>/prompts.yaml` to change LLM behavior without recompiling. Keys: `intro`, `security`, `classifier`, `debug`, `general`.

Global prompts (e.g. `security`) are defined in `agents/prompts.yaml` and inherited by all agents. Agent-specific prompts override globals.

## Integrations

| Integration | Documentation | Required By |
|---|---|---|
| Slack | [docs/SLACK_BOT.md](docs/SLACK_BOT.md) | All agents |
| GitHub | [docs/GITHUB_PAT.md](docs/GITHUB_PAT.md) | ovad, agent-q, goldsai |
| Atlassian (Jira + Confluence) | [docs/ATLASSIAN.md](docs/ATLASSIAN.md) | seihin, ovad, agent-q, goldsai, pulse |
| NVD | [NVD API](https://nvd.nist.gov/developers) | goldsai |
| Salesforce | [docs/SALESFORCE.md](docs/SALESFORCE.md) | pulse |
| Chorus / ZoomInfo | [docs/CHORUS.md](docs/CHORUS.md) | pulse |
| AWS Cost Explorer | [docs/AWS.md](docs/AWS.md) | ovad (and any agent running AWS cost workflows) |

## Contributing

Pull requests are disabled on this repository. Contributions are accepted through issues only — please [open an issue](https://github.com/justmike1/arbetern/issues) to report bugs, request features, or propose changes. For anything else, contact the maintainer directly via [@justmike1](https://github.com/justmike1).

## Author & Maintainer

**Mike Joseph** — [@justmike1](https://github.com/justmike1)

## License

This project is licensed under the Apache License 2.0 — see the [LICENSE](LICENSE) file for details.

---

If you find this project useful, please consider giving it a ⭐!
