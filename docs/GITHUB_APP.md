# GitHub App Setup Guide

## Overview

arbetern authenticates to GitHub as a **GitHub App** by default. Everything it
does on GitHub then carries the App's own bot account, for example
`arbetern[bot]`: the pull requests it opens, its comments and review replies,
branch updates, auto-merge, and the commits of [project](PROJECTS.md) sessions.
No person's or shared machine user's account is involved, the App reaches only
the repositories it is installed on, and its tokens expire after an hour.
arbetern renews them itself.

The App covers GitHub API and git access. The GitHub Models LLM backend still
needs a personal access token (`GITHUB_TOKEN`, see
[GITHUB_PAT.md](GITHUB_PAT.md)); with the Anthropic API, Amazon Bedrock or
Azure OpenAI as the LLM backend, no token is needed.

---

## Step 1: Register the App

Open this link after replacing `ORGANIZATION` with your organization's login.
For an App on a personal account, start the link with
`https://github.com/settings/apps/new?` instead and keep the rest.

```
https://github.com/organizations/ORGANIZATION/settings/apps/new?name=Arbetern&description=Arbetern%27s%20GitHub%20identity%3A%20reads%20code%2C%20opens%20and%20follows%20pull%20requests&url=https%3A%2F%2Fgithub.com%2Fjustmike1%2Farbetern&public=false&webhook_active=false&contents=write&pull_requests=write&actions=write&checks=read&statuses=read&administration=read&metadata=read
```

The link fills in the name, the homepage, a private App (installable only on
this account), no webhook (arbetern polls GitHub, it needs no events), and these
repository permissions:

| Permission | Access | Why arbetern needs it |
|---|---|---|
| **Contents** | Read and write | Read code, create branches and commits, merge pull requests; project sessions push through it |
| **Pull requests** | Read and write | Open, edit, comment on, update and auto-merge pull requests |
| **Actions** | Read and write | Read workflow runs, jobs and logs; re-run failed jobs |
| **Checks** | Read-only | Read check runs and their annotations to surface CI errors |
| **Commit statuses** | Read-only | Read the statuses a [project](PROJECTS.md#following-the-pull-request) waits on |
| **Administration** | Read-only | Optional: list the teams with access to a repository (`list_repo_teams`) |
| **Metadata** | Read-only | Required by GitHub for every App |

Leave **Workflows** unset. Without it GitHub refuses any push or commit that
adds or changes a file under `.github/workflows`, which keeps
[project](PROJECTS.md#security) sessions, whose pushes use the App, from
altering CI.

Review the page and click **Create GitHub App**. App names are unique across
GitHub; if the name is taken, pick another. The bot account is the App's slug
plus `[bot]` (an App named `Arbetern` acts as `arbetern[bot]`), and arbetern
reads it from GitHub, so any name works.

## Step 2: Generate a Private Key

1. On the App's settings page, note the **App ID** (or the **Client ID**;
   either works).
2. Under **Private keys**, click **Generate a private key**. A `.pem` file
   downloads; it is the only copy.
3. Optional: under **Display information**, upload a logo (for example
   `assets/logo-head.png`) so the bot's avatar matches.

## Step 3: Install the App

1. Open **Install App** in the App's settings and choose the account.
2. Select **All repositories** or **Only select repositories**. An
   organization-scoped project reaches only the repositories selected here,
   whatever its patterns match.
3. The installation ID is the number at the end of the page's URL
   (`…/settings/installations/12345678`). arbetern needs it only when the App
   is installed on more than one account.

## Step 4: Configure arbetern

| Variable | Required | Description |
|---|---|---|
| `GITHUB_APP_ID` | yes | The App ID or client ID |
| `GITHUB_APP_PRIVATE_KEY` | yes | The private key: the PEM file's content, base64-encoded or not |
| `GITHUB_APP_INSTALLATION_ID` | no | The installation to act as; needed only when the App is installed on more than one account |

With the Helm chart and `createSecret: true`:

```yaml
secretValues:
  github-app-id: "123456"
  github-app-private-key: |
    -----BEGIN RSA PRIVATE KEY-----
    ...
    -----END RSA PRIVATE KEY-----
```

With a Secret you create yourself (`createSecret: false`):

```bash
kubectl create secret generic arbetern-secrets \
  --from-literal=github-app-id=123456 \
  --from-file=github-app-private-key=arbetern.private-key.pem \
  ...
```

and still list `github-app-id` in `secretValues` with any non-empty value, so
the chart wires the App's variables.

When both an App and `GITHUB_TOKEN` are set, arbetern uses the App for GitHub
and the token only for the GitHub Models backend.

## Verifying the App

- The startup log shows `GitHub: authenticating as GitHub App <id>`.
- The console's **Integrations** panel shows GitHub with the auth mode
  **GitHub App** and checks every permission above against what the
  installation grants. A permission the App has but arbetern does not use is
  flagged as extra.
- Ask an agent to list repositories, for example `/ovad list repos`.

## What Shows the App's Name

| Where | Author |
|---|---|
| Pull requests, comments, review replies, branch updates, auto-merge | The bot account, `arbetern[bot]` |
| Commits arbetern makes through the API, such as an agent's file edit | The bot account; GitHub signs them, so they show as verified |
| Commits of project sessions | `arbetern[bot]` with the bot's no-reply email (`<id>+arbetern[bot]@users.noreply.github.com`), so GitHub links them to the App. `projects.sessions.git.name` and `email` override it |

The bot's login is `<slug>[bot]` in the REST API and in searches
(`author:arbetern[bot]`); GraphQL names it by the slug alone.

## Moving from a Personal Access Token

1. Register and install the App (steps 1 to 3) on the repositories the token
   reaches.
2. Update automation that recognizes arbetern by its login: CI rules that
   exempt the token's account from branch or title conventions, CODEOWNERS
   entries and rulesets that name it. They need the App's bot account instead.
3. Rulesets apply to the App unless it is added as a bypass actor. arbetern
   does not need a bypass: it merges only what the base branch's rules already
   allow.
4. Deploy with the App's variables while no project pull request has auto-merge
   on. arbetern treats auto-merge that the token's account turned on as someone
   else's and no longer turns it off. It recognizes its earlier comments by
   their hidden marker, so follow-up rounds do not answer them.
5. Remove `GITHUB_TOKEN` unless the GitHub Models backend uses it, and revoke
   the token once nothing else does. The console's **Pull requests** page lists
   pull requests by the current identity, so ones the token opened drop off it.

## Rotating the Private Key

1. Generate a new key on the App's settings page (an App can hold several).
2. Update `GITHUB_APP_PRIVATE_KEY` and restart arbetern.
3. Delete the old key on the settings page.

---

## Troubleshooting

**`GitHub App setup failed` at startup**
- The private key is not a PEM block, or not the App's RSA key. Paste the whole
  `.pem` file, including the `BEGIN` and `END` lines.

**`creating a GitHub App installation token: … 401`**
- The App ID does not match the key, the key was deleted, or the server's clock
  is off by more than a minute (the request is signed with a short-lived JWT).

**`the GitHub App is not installed on any account`**
- Install it (step 3).

**`the GitHub App is installed on more than one account`**
- Set `GITHUB_APP_INSTALLATION_ID`.

**`403 Resource not accessible by integration`**
- A permission is missing. After you change an App's permissions, an owner of
  the account must accept them on the installation before tokens carry them.
  The Integrations panel shows which permission is missing.

**`404 Not Found` for a repository**
- The App is not installed on that repository.

**Project session commits show `arbetern` instead of the bot**
- The chart sets `projects.sessions.git.name`, or arbetern could not read the
  bot account; its log says `the GitHub App's commit identity is unknown`.
