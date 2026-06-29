# GitOps sync (workflows + dashboards)

arbetern can keep both its workflow and dashboard registries in sync with a
directory of JSON descriptors checked into a GitHub repository. Once
enabled, the running pod polls the configured remote on a fixed interval
and reconciles every JSON file under `<basePath>/<agent>/<id>.json` into
the local registry — creating new entries, applying edits to existing
ones, and (optionally) pruning entries that have been removed from git.

The two syncers are independent — you can enable workflows-only,
dashboards-only, or both, against the same or different repositories.
GitOps-managed entries are flagged in the UI: the edit button is hidden
on workflows, both views show a banner linking back to the source file,
and the API rejects edits/deletes on gitops-managed objects (toggling a
workflow's `enabled` flag is the one allowed mutation).

This turns workflow management into a normal pull-request flow: someone
opens a PR adding `arbetern/workflows/ovad/aws-daily-cost.json`, it gets
reviewed and merged, and within one poll interval (default 5 minutes) the
running arbetern instance picks it up and starts ticking the workflow.

---

## Repository layout

```
arbetern/
├── workflows/
│   ├── ovad/
│   │   ├── aws-daily-cost.json
│   │   └── jira-bug-autofix.json
│   └── seihin/
│       └── seihin-application-triage.json
└── dashboards/
    ├── ovad/
    │   └── infra-overview.json
    └── pulse/
        └── account-acme.json
```

- Top-level paths default to `arbetern/workflows` and `arbetern/dashboards`
  but can be relocated via `basePath` / `*_GITOPS_BASE_PATH`. The two
  syncers can target different base paths and even different repos.
- Each immediate subdirectory is the agent ID (`ovad`, `seihin`,
  `goldsai`, …). Files outside an agent subdirectory are ignored.
- Each `*.json` file inside an agent directory is one workflow or
  dashboard. The file *name* is purely cosmetic — the syncer keys off
  the JSON `id` field.

## File format

### Workflows

Each workflow JSON has the same shape as the on-disk registry descriptor,
minus run-history fields (which are ignored on read and preserved across
reconciles).

```json
{
  "id": "c8df2f8d27e3d875",
  "agent": "ovad",
  "name": "Daily AWS cost report",
  "short_name": "aws-daily-cost",
  "description": "Posts a daily AWS Cost Explorer summary to Slack at 05:00 UTC.",
  "cron": "0 5 * * *",
  "prompt": "Daily report — post to Slack channel C0123456789. ...",
  "trigger": { "type": "schedule" },
  "enabled": true,
  "created_by": "U0123456789"
}
```

| Field         | Required          | Notes                                                                                              |
|---------------|-------------------|----------------------------------------------------------------------------------------------------|
| `id`          | yes               | Stable identifier (also used as the on-disk filename inside the pod). Hex-ish: `[a-z0-9-]{1,63}`.  |
| `agent`       | recommended       | Must match the parent directory name. The directory wins on conflict.                              |
| `name`        | yes               | Human-readable title shown in `/arbetern list workflows` and the UI.                               |
| `short_name`  | optional          | Slugified from `name` when omitted.                                                                |
| `description` | optional          | One-line description for the UI.                                                                   |
| `cron`        | yes for schedules | Standard 5-field UTC cron expression (e.g. `0 9 * * *`) or `@daily` / `@hourly` / `@every 1h`.     |
| `prompt`      | yes\*             | Natural-language prompt; `\*` either `prompt`, `prompt_path`, or `tasks` is required.              |
| `prompt_path` | optional          | Load the prompt from a companion file. See [Prompt files](#prompt-files) below. Wins over `prompt` if both are set. |
| `tasks`       | yes\*             | Ordered list of `{name, prompt}` for multi-step (subflows) workflows.                              |
| `trigger`     | optional          | `{ "type": "schedule" }` (default), `manual`, `on_success`/`on_failure` (with `"ref":"agent/id"`). |
| `enabled`     | optional          | Defaults to `true`.                                                                                |
| `created_by`  | optional          | Slack user ID; surfaced in the UI for attribution.                                                 |

Run-history fields like `last_run`, `runs`, `consecutive_failures`,
`disabled_reason` are silently ignored on read — feel free to leave them
out, or to commit a registry-exported file as-is.

#### Prompt files

For long-form prompts it can be cleaner to keep the markdown next to the
descriptor instead of escaping it into a JSON string. Set `prompt_path`
to one of:

- **Relative to the descriptor** — `"./jira-bug-autofix-prompt.md"`,
  `"prompt.md"`, `"../shared/triage.md"`. Resolved against the directory
  the descriptor lives in (`arbetern/workflows/<agent>/`).
- **Repo-absolute** — `"/arbetern/workflows/_shared/prompt.md"`. Treated
  as a path from the source repo's root.
- **Cross-repo URL** — `"https://github.com/<owner>/<repo>/blob/<branch>/<path>"`.
  The same `GITHUB_TOKEN` is used to fetch it, so it must have read
  access to the target repo. Branch names containing slashes are not
  supported.

```jsonc
{
  "id": "a1b2c3d4e5f60718",
  "agent": "ovad",
  "name": "Jira Bug Auto-Fix",
  "short_name": "jira-bug-autofix",
  "description": "Polls Jira every hour for open ENG tickets ...",
  "cron": "0 * * * 0-4",
  "prompt_path": "./jira-bug-autofix-prompt.md",
  "trigger": { "type": "schedule" },
  "enabled": true
}
```

When the file is fetched, its contents become the workflow prompt and
are persisted into the orchestrator's local `data.json` under the usual
`prompt` field — `prompt_path` is purely a source-of-truth indirection
for git, never written back to local state. If both `prompt` and
`prompt_path` appear in the same descriptor, `prompt_path` wins and a
warning is logged. Resolution failures (missing file, 404, escapes repo
root) skip the workflow on that reconcile and surface in the syncer's
last-error status.

This indirection is **gitops-only**. Workflows created or edited via the
UI / HTTP API continue to use the inline `prompt` field exactly as
before.

### Dashboards

Dashboard files have the dashboards-registry shape, with runtime fields
(`data`, `last_sync`, `last_error`, `account`) ignored on read.

```json
{
  "id": "infra-overview",
  "agent": "ovad",
  "name": "Infra overview",
  "short_name": "infra-overview",
  "description": "Cluster + cost rollup, refreshed every 15 minutes.",
  "kind": "sources",
  "sync_interval": "15m",
  "sources": [
    { "name": "aws_cost", "type": "aws_cost_explorer", "args": { "granularity": "DAILY" } },
    { "name": "github_prs", "type": "github_open_prs", "args": { "repo": "arbetern" } }
  ],
  "created_by": "U0123456789"
}
```

| Field           | Required    | Notes                                                                              |
|-----------------|-------------|------------------------------------------------------------------------------------|
| `id`            | yes         | Stable identifier; same character set as workflows.                                |
| `agent`         | recommended | Must match the parent directory name. Directory wins on conflict.                  |
| `name`          | yes         | Human-readable title.                                                              |
| `short_name`    | optional    | Slugified from `name` when omitted.                                                |
| `description`   | optional    | One-line description for the UI.                                                   |
| `kind`          | optional    | `"sources"` (default) or `"account"` for account-health dashboards.                |
| `sync_interval` | optional    | Go duration (`5m`, `15m`, `1h`, …). Defaults to the registry default.              |
| `sources`       | yes         | Array of `{name, type, args}` data sources to refresh on every sync.               |

## Reconcile semantics

Every poll the syncer:

1. Lists `<basePath>/<agent>/` for every agent subdirectory.
2. Reads every `*.json`, parses it, and keys the result by `(agent, id)`.
3. For each desired workflow, calls `Registry.Upsert` — either creating a
   new workflow or applying any field that differs (a content fingerprint
   skips no-op writes so the on-disk JSON and tick goroutine are not
   bounced when nothing changed).
4. If `prune` is enabled, finds every locally-stored workflow that was
   previously synced from this source (`source = "gitops"`) but is no
   longer present in git, and deletes it. With `prune` disabled (default),
   missing-from-git workflows are left in place and a warning is logged.

Workflows created by the syncer are tagged `source = "gitops"` and
`source_ref = "<owner>/<repo>@<branch>:<path-in-repo>"`. The UI surfaces
this so reviewers know which workflows are git-managed.

> **Local edits will be overwritten.** If you edit a gitops-managed
> workflow's prompt/cron via the UI or `/agent` slash commands, the
> syncer will revert it on the next tick. Toggling `enabled` is the one
> exception — the syncer respects whatever the file in git says, but if
> you set `enabled: true` in git you can still pause the workflow from
> the UI. Re-enabling matters because re-enabling clears the
> auto-disable banner. The recommended pattern is: edit the JSON in
> git, open a PR, merge.

## Configuration

Each syncer has its own block under `values.yaml`. The two are independent
— enable just one or both. Env-var equivalents follow the same pattern:
swap the `WORKFLOWS_` prefix for `DASHBOARDS_` to address the dashboard
syncer.

| Helm key                                  | Env var                          | Default                 | Notes                                                                |
|-------------------------------------------|----------------------------------|-------------------------|----------------------------------------------------------------------|
| `workflows.gitops.enabled`                | (implicit)                       | `false`                 | Enables the workflows syncer when `repo` is also set.                |
| `workflows.gitops.owner`                  | `WORKFLOWS_GITOPS_OWNER`         | bot's resolved owner    | Org or user that owns the repo.                                      |
| `workflows.gitops.repo`                   | `WORKFLOWS_GITOPS_REPO`          | (required)              | Repository name (no owner).                                          |
| `workflows.gitops.branch`                 | `WORKFLOWS_GITOPS_BRANCH`        | repo default branch     | Branch to read.                                                      |
| `workflows.gitops.basePath`               | `WORKFLOWS_GITOPS_BASE_PATH`     | `arbetern/workflows`    | Top-level path inside the repo.                                      |
| `workflows.gitops.interval`               | `WORKFLOWS_GITOPS_INTERVAL`      | `5m`                    | Go duration. Minimum 30 s.                                           |
| `workflows.gitops.prune`                  | `WORKFLOWS_GITOPS_PRUNE`         | `true`                  | Delete locally-managed workflows that disappear from git.            |
| `dashboards.gitops.enabled`               | (implicit)                       | `false`                 | Enables the dashboards syncer when `repo` is also set.               |
| `dashboards.gitops.owner`                 | `DASHBOARDS_GITOPS_OWNER`        | bot's resolved owner    |                                                                      |
| `dashboards.gitops.repo`                  | `DASHBOARDS_GITOPS_REPO`         | (required)              |                                                                      |
| `dashboards.gitops.branch`                | `DASHBOARDS_GITOPS_BRANCH`       | repo default branch     |                                                                      |
| `dashboards.gitops.basePath`              | `DASHBOARDS_GITOPS_BASE_PATH`    | `arbetern/dashboards`   |                                                                      |
| `dashboards.gitops.interval`              | `DASHBOARDS_GITOPS_INTERVAL`     | `5m`                    | Go duration. Minimum 30 s.                                           |
| `dashboards.gitops.prune`                 | `DASHBOARDS_GITOPS_PRUNE`        | `true`                  | Delete locally-managed dashboards that disappear from git.           |

Both syncers reuse the same `GITHUB_TOKEN` as the rest of the app, so it
needs `contents:read` on the source repo(s). No extra credentials.

## Helm example

```yaml
workflows:
  enabled: true
  gitops:
    enabled: true
    repo: <your-workflows-repo>
    basePath: arbetern/workflows
    interval: 5m
    prune: true

dashboards:
  enabled: true
  gitops:
    enabled: true
    repo: <your-dashboards-repo>      # may be the same repo as workflows
    basePath: arbetern/dashboards
    interval: 5m
    prune: true
```

## ConfigMap example (raw kubernetes deploy)

If you're not using the Helm chart, the same configuration as a
`ConfigMap` consumed via `envFrom`:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: arbetern-gitops
data:
  WORKFLOWS_GITOPS_REPO: "<your-workflows-repo>"
  WORKFLOWS_GITOPS_BASE_PATH: "arbetern/workflows"
  WORKFLOWS_GITOPS_INTERVAL: "5m"
  WORKFLOWS_GITOPS_PRUNE: "true"
  DASHBOARDS_GITOPS_REPO: "<your-dashboards-repo>"
  DASHBOARDS_GITOPS_BASE_PATH: "arbetern/dashboards"
  DASHBOARDS_GITOPS_INTERVAL: "5m"
  DASHBOARDS_GITOPS_PRUNE: "true"
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: arbetern
spec:
  template:
    spec:
      containers:
        - name: arbetern
          envFrom:
            - configMapRef:
                name: arbetern-gitops
            - secretRef:
                name: arbetern-secrets   # provides GITHUB_TOKEN, etc.
```

## Operational notes

- Each syncer logs one summary line per reconcile under its own prefix:
  `[gitopssync]` for workflows, `[dashboards/gitopssync]` for dashboards.
- A failed read of an individual file (bad JSON, agent dir typo, etc.)
  does not abort the rest of the tick — that file is logged and skipped.
- The first reconcile runs synchronously at startup so any wiring problem
  shows up in the pod's startup logs immediately.
- Health: there is no `/healthz` integration yet; failures surface only
  in logs. Watch for `[gitopssync] reconcile failed` in your log
  pipeline.
