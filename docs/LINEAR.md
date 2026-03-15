# Linear Integration

This document describes how to configure and use the Linear ticketing system integration with arbetern.

## Overview

The Linear integration allows agents to:

| Tool | Description |
|---|---|
| **create_linear_issue** | Create an issue with title, description, priority, and optional assignee |
| **get_linear_issue** | Fetch full details of a specific issue by identifier (e.g. `ENG-123`) |
| **search_linear_issues** | Search for issues by keyword across titles and descriptions |
| **update_linear_issue** | Update an issue's title and/or description |
| **list_linear_teams** | Discover available teams and their IDs |
| **resolve_linear_user** | Find a Linear user by name or email for issue assignment |

Example Slack commands:

```
/seihin create a Linear issue from the bug report above
/ovad find Linear issues related to the login timeout
/seihin update ENG-123 with the new acceptance criteria we discussed
```

## Setup

### 1. Generate a Personal API Token

1. Log in to [linear.app](https://linear.app) with a service or bot account.
2. Navigate to **Settings → API → Personal API Keys**.
3. Click **Create Key**, give it a name (e.g. `arbetern-bot`), and copy the token.

> **Tip:** Use a dedicated service account rather than a personal account, so the bot's access is not tied to a single person.

### 2. Find Your Team ID (Optional)

If you want a default team for issue creation, find your team's ID:

1. In Linear, navigate to **Settings → Teams**.
2. Click on the team you want to use as the default.
3. The team ID appears in the URL: `https://linear.app/your-org/settings/teams/<TEAM_ID>/general`

Alternatively, after configuring the API token, agents can call `list_linear_teams` to discover all available team IDs at runtime.

### 3. Configure Environment Variables

| Variable | Required | Description |
|---|---|---|
| `LINEAR_API_TOKEN` | **Yes** | Personal API token from step 1 |
| `LINEAR_TEAM_ID` | No | Default team ID for issue creation. If not set, users must specify `team_id` per request or use `list_linear_teams`. |

### Method 1: Environment Variables (Local / Docker)

```bash
export LINEAR_API_TOKEN="lin_api_xxxxxxxxxxxxxxxx"
export LINEAR_TEAM_ID="your-team-uuid-here"   # optional
```

### Method 2: Helm Deployment

Add to your `values.yaml`:

```yaml
env:
  - name: LINEAR_API_TOKEN
    valueFrom:
      secretKeyRef:
        name: arbetern-secrets
        key: linear-api-token
  - name: LINEAR_TEAM_ID
    value: "your-team-uuid-here"   # optional
```

Create the Kubernetes secret:

```bash
kubectl create secret generic arbetern-secrets \
  --from-literal=linear-api-token=lin_api_xxxxxxxxxxxxxxxx
```

## Usage

Once configured, agents automatically gain access to Linear tools. No additional slash commands are needed — the agent decides when to use Linear based on the conversation.

### Creating Issues

```
/seihin create a Linear ticket for the login timeout bug we discussed
/ovad open a high-priority Linear issue for the API rate limiting problem
```

### Searching Issues

```
/seihin find Linear issues related to authentication
/agent-q what are the open issues about the deploy pipeline in Linear?
```

### Updating Issues

```
/seihin review ENG-123 and improve its description
/seihin update the acceptance criteria on ENG-456 based on our discussion
```

### Listing Teams

```
/ovad what Linear teams do we have?
```

## Permissions

The Linear API token needs the following scopes (granted by default for Personal API Keys):

| Scope | Description |
|---|---|
| `issues:read` | Read issues, search, and fetch issue details |
| `issues:create` | Create new issues |
| `issues:update` | Update issue title and description |
| `teams:read` | List accessible teams and workflow states |
| `users:read` | Search and resolve team members for issue assignment |

Personal API Keys in Linear automatically have access to all resources the user can access, with the same permission level as the user's role.
