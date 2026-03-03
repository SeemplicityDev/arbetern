# arbetern

[![Stars](https://img.shields.io/github/stars/justmike1/arbetern?style=social)](https://github.com/justmike1/arbetern/stargazers)
[![Go](https://img.shields.io/badge/Go-1.25+-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![License](https://img.shields.io/github/license/justmike1/arbetern)](LICENSE)
[![Docker](https://img.shields.io/badge/Docker-ready-2496ED?logo=docker&logoColor=white)](Dockerfile)

*Yiddish for "workers."*

An orchestration platform for AI agents in the enterprise. Each agent lives in its own directory under `agents/`, with dedicated prompts and a defined professional scope. Arbetern provides the runtime, routing, UI, and integrations — agents bring the expertise.

## Current Agents

| Agent | Profession | Description |
|---|---|---|
| **ovad** | DevOps & SRE Engineer | Debugs CI/CD failures, reads/modifies repo files, opens PRs — all from a Slack slash command |
| **agent-q** | QA & Test Engineer | Analyzes test failures, reviews test coverage, suggests test cases, and triages flaky tests |
| **goldsai** | Security Researcher | Assesses CVE impact on your codebase, audits dependencies, reviews code for vulnerabilities, and recommends remediation |
| **seihin** (製品) | Sr. Technical Product Manager | Reviews and refines Jira tickets, rewrites descriptions with PM best practices, manages ticket quality at scale |

## Quick Start

### Prerequisites

- Go 1.25+
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
| `JIRA_URL` | no | Jira instance URL (e.g. `https://yourorg.atlassian.net`) |
| `JIRA_EMAIL` | no | Jira service account email |
| `JIRA_API_TOKEN` | no | Jira API token |
| `JIRA_PROJECT` | no | Default Jira project key (e.g. `ENG`) |
| `APP_URL` | no | Public app URL (used for Jira ticket stamps) |
| `UI_ALLOWED_CIDRS` | no | Comma-separated CIDRs allowed to access the UI |
| `SLACK_APP_TOKEN` | no | Slack app-level token (`xapp-...`) for Socket Mode — enables thread follow-ups without slash commands (see [docs/SLACK_BOT.md](docs/SLACK_BOT.md#socket-mode-thread-follow-ups)) |
| `THREAD_SESSION_TTL` | no | Duration a thread session stays active (default: `3m`). Go duration format, e.g. `5m`, `2m30s` |
| `MAX_TOOL_ROUNDS` | no | Max LLM tool-call rounds per request (default: `50`). Increase for complex multi-file tasks |
| `NVD_API_KEY` | no | NVD (National Vulnerability Database) API key for CVE lookups. Get one free at <https://nvd.nist.gov/developers/request-an-api-key>. Without a key, requests are rate-limited (~5 req/30s vs ~50 req/30s with a key) |
| `CUSTOM_PROMPTS_DIR` | no | Directory containing custom prompt YAML files that are **appended** to built-in agent prompts. Used for org-specific context via Kubernetes ConfigMap. Set automatically by the Helm chart when `customPrompts` is configured |
| `UI_HEADER` | no | Custom header text for the web UI (default: `arbetern`) |

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

## Project Structure

```
main.go              # entrypoint, HTTP server, API
agents/              # agent definitions (one directory per agent)
  ovad/
    prompts.yaml     # DevOps & SRE agent prompts
  agent-q/
    prompts.yaml     # QA & Test Engineering agent prompts
  goldsai/
    prompts.yaml     # Security Research agent prompts
  seihin/
    prompts.yaml     # Sr. Technical Product Manager agent prompts
  prompts.yaml       # global prompts shared by all agents (e.g. security)
config/              # env var loading
commands/            # intent routing, debug/general handlers
github/              # GitHub API client + Models/Azure API client
jira/                # Jira Cloud REST API client
nvd/                 # NVD (National Vulnerability Database) CVE API client
slack/               # Slack webhook handler + response helpers
prompts/             # YAML prompt loader + agent discovery
ui/                  # embedded web UI (agent manager)
helm/                # Helm chart
docs/                # setup guides (Slack, GitHub PAT, Jira)
```

## Customizing Prompts

Edit any `agents/<name>/prompts.yaml` to change LLM behavior without recompiling. Keys: `intro`, `security`, `classifier`, `debug`, `general`.

Global prompts (e.g. `security`) are defined in `agents/prompts.yaml` and inherited by all agents. Agent-specific prompts override globals.

## Integrations

| Integration | Documentation | Required By |
|---|---|---|
| Slack | [docs/SLACK_BOT.md](docs/SLACK_BOT.md) | All agents |
| GitHub | [docs/GITHUB_PAT.md](docs/GITHUB_PAT.md) | ovad, agent-q, goldsai |
| Jira | [docs/JIRA.md](docs/JIRA.md) | seihin, ovad, agent-q, goldsai |
| NVD | [NVD API](https://nvd.nist.gov/developers) | goldsai |

## Contributing

Contributions are welcome! Please open an issue or submit a pull request.

1. Fork the repository
2. Create your feature branch (`git checkout -b feature/amazing-feature`)
3. Commit your changes (`git commit -m 'Add amazing feature'`)
4. Push to the branch (`git push origin feature/amazing-feature`)
5. Open a Pull Request

## Author & Maintainer

**Mike Joseph** — [@justmike1](https://github.com/justmike1)

## License

This project is licensed under the Apache License 2.0 — see the [LICENSE](LICENSE) file for details.

---

If you find this project useful, please consider giving it a ⭐!
