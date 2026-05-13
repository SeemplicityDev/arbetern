# GitHub Personal Access Token Setup Guide

## Overview

Arbetern uses a GitHub Personal Access Token (PAT) for two purposes:

1. **GitHub API access** — reading repositories, creating branches, committing files, opening pull requests, and inspecting GitHub Actions workflow runs.
2. **LLM inference** — calling the GitHub Models API (e.g., `openai/gpt-4o`) to power the AI assistant. This is not required when using Azure OpenAI instead.

You can use either a **fine-grained PAT** (recommended) or a **classic PAT**.

---

## Option A: Fine-Grained Personal Access Token (Recommended)

Fine-grained tokens let you grant only the permissions arbetern needs, scoped to specific repositories or an entire organization.

### Step 1: Create the Token

1. Go to https://github.com/settings/tokens?type=beta
2. Click **Generate new token**
3. Set a descriptive name, e.g. `arbetern-bot`
4. Choose an expiration (90 days recommended; set a reminder to rotate)
5. Under **Resource owner**, select the user or organization that owns the target repositories
6. Under **Repository access**, choose **All repositories** or select specific repositories arbetern should operate on

### Step 2: Configure Permissions

Grant the following **repository permissions**:

| Permission | Access | Why arbetern needs it |
|---|---|---|
| **Contents** | Read and Write | Read files, create branches, commit file changes |
| **Pull requests** | Read and Write | Open pull requests for file modifications |
| **Actions** | Read-only | Fetch workflow run status and job details for debugging |
| **Checks** | Read-only | Read check-run annotations to surface CI errors |
| **Metadata** | Read-only | Required by GitHub for all fine-grained tokens (auto-selected) |

If arbetern should discover repositories across an organization, also grant this **organization permission**:

| Permission | Access | Why arbetern needs it |
|---|---|---|
| **Members** | Read-only | List organization repositories |

### Step 3: Generate and Copy

1. Click **Generate token**
2. Copy the token immediately — it will not be shown again

---

## Option B: Classic Personal Access Token

Classic tokens use broader scopes. Use this option only if fine-grained tokens are not available for your organization.

### Step 1: Create the Token

1. Go to https://github.com/settings/tokens
2. Click **Generate new token** > **Generate new token (classic)**
3. Set a descriptive name, e.g. `arbetern-bot`
4. Choose an expiration

### Step 2: Select Scopes

Check the following scopes:

| Scope | Why arbetern needs it |
|---|---|
| `repo` | Full access to repositories — read files, create branches, commit, open PRs, and read Actions/Checks data |
| `read:org` | List organization repositories and resolve which teams have direct access to a repo (powers `list_org_repos` and `list_repo_teams`) |

### Step 3: Generate and Copy

1. Click **Generate token**
2. Copy the token immediately — it will not be shown again

---

## Step 4: Configure Arbetern

Set the token as the `GITHUB_TOKEN` environment variable where arbetern runs:

```
GITHUB_TOKEN=ghp_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
```

If you are using Azure OpenAI instead of GitHub Models for LLM inference, also set `AZURE_OPEN_AI_ENDPOINT` and `AZURE_API_KEY` and you can skip the GitHub Models requirement. Otherwise, `GITHUB_TOKEN` is used for both API access and LLM calls.

See the main [README](../README.md) for the full list of environment variables.

---

## Verifying the Token

After setting `GITHUB_TOKEN`, start arbetern and run a simple command in Slack (using whatever slash command you configured for your agent):

```
/ovad list repos
```

If the token is valid and has the correct permissions, the agent will return a list of repositories.

---

## Rotating the Token

When a token expires or is compromised:

1. Create a new token following the steps above
2. Update the `GITHUB_TOKEN` environment variable
3. Restart arbetern
4. Revoke the old token at https://github.com/settings/tokens

---

## Troubleshooting

**"401 Unauthorized" or "Bad credentials" errors in logs**
- The token is invalid, expired, or not set. Verify `GITHUB_TOKEN` is correctly configured and the token has not been revoked.

**"403 Resource not accessible by personal access token"**
- The token is missing a required permission. Review the permissions table above and regenerate the token with the correct scopes.

**"404 Not Found" when accessing a repository**
- The token does not have access to that repository. For fine-grained tokens, ensure the repository is included in the token's repository access list.

**Cannot list organization repositories**
- For fine-grained tokens, grant the **Members** organization permission. For classic tokens, add the `read:org` scope.
