# Slack Bot Setup Guide

## Prerequisites

- A Slack workspace where you have admin permissions
- A public URL where the arbetern server will be deployed (e.g., `https://ai.example.com`). You will configure and deploy the server in Step 6 after obtaining the Slack credentials.

## Step 1: Create a Slack App

1. Go to https://api.slack.com/apps
2. Click **Create New App** > **From scratch**
3. Set the app name (e.g. `ovad` for the ovad agent)
4. Select your workspace
5. Click **Create App**

## Step 2: Configure Bot Token Scopes

1. In the left sidebar, go to **OAuth & Permissions**
2. Scroll to **Scopes** > **Bot Token Scopes**
3. Add the following scopes:

| Scope | Purpose |
|---|---|
| `commands` | Register and receive slash commands |
| `channels:history` | Read messages from public channels |
| `chat:write` | Post responses to channels |
| `files:write` | Upload file snippets to channels (large search results, reports, code blocks) |
| `users:read` | Resolve Slack user IDs to real names (used by agents like Seihin to look up the user's identity for Jira queries) |

## Step 3: Create the Slash Command

1. In the left sidebar, go to **Slash Commands**
2. Click **Create New Command**
3. Fill in:
   - **Command:** `/ovad` (or your agent's name, e.g. `/seihin`, `/agent-q`, `/goldsai`)
   - **Request URL:** `https://<your-server>/<agent-name>/webhook` (e.g. `https://ai.example.com/ovad/webhook`)
   - **Short Description:** `AI-powered assistant` (adjust per agent)
   - **Usage Hint:** `[your agent-specific usage hint]`
4. Click **Save**

> **Multi-agent setup:** Create a separate Slack app (or slash command) for each agent, each pointing to its own `/<agent>/webhook` path.

## Step 4: Install the App to Your Workspace

1. In the left sidebar, go to **Install App**
2. Click **Install to Workspace**
3. Review the permissions and click **Allow**
4. Copy the **Bot User OAuth Token** (starts with `xoxb-`)

## Step 5: Get the Signing Secret

1. In the left sidebar, go to **Basic Information**
2. Under **App Credentials**, find **Signing Secret**
3. Click **Show** and copy the value

## Step 6: Configure Arbetern Environment Variables

Set these environment variables on the server running arbetern:

```
SLACK_BOT_TOKEN=xoxb-your-bot-token
SLACK_SIGNING_SECRET=your-signing-secret
GITHUB_TOKEN=your-github-pat
GENERAL_MODEL=openai/gpt-4o
PORT=8080
# Optional: enable thread follow-ups via Socket Mode (see below)
SLACK_APP_TOKEN=xapp-1-...
THREAD_SESSION_TTL=3m
```

## Step 7: Invite the Bot to Channels

In each Slack channel where you want to use an agent, run:

```
/invite @<bot-name>
```

The bot needs to be present in a channel to read its message history.

## Usage

Once configured, use the agent's slash command in any channel the bot has been invited to:

```
/ovad please debug the latest message in this channel
```

```
/ovad please add env var KEY=NAME in amplify.tf in devops-infra repository
```

## Troubleshooting

**"dispatch_failed" error when using the command**
- The server is unreachable from Slack. Verify the Request URL is correct and the server is running.

**"not_in_channel" error in logs**
- Run `/invite @<bot-name>` in the channel.

**"invalid_auth" error in logs**
- The `SLACK_BOT_TOKEN` is wrong or expired. Reinstall the app and copy the new token.

**"channel_not_found" when reading history**
- For private channels, the bot needs the `groups:history` scope. Add it under **OAuth & Permissions** and reinstall the app.

---

## Socket Mode: Thread Follow-ups

Socket Mode lets users reply directly in a thread instead of typing another `/command`. The bot keeps a session open for each thread (default: 3 minutes, refreshed on every reply).

### Prerequisites

- The steps above (Steps 1–7) are already completed
- The arbetern server is deployed and working with slash commands

### Step 1: Enable Socket Mode

1. Go to https://api.slack.com/apps → select your app
2. In the left sidebar, go to **Socket Mode**
3. Toggle **Enable Socket Mode** to **On**

### Step 2: Generate an App-Level Token

1. In the same **Socket Mode** page (or **Basic Information** → **App-Level Tokens**)
2. Click **Generate Token and Scopes**
3. Give it a name (e.g. `arbetern-socket`)
4. Add scope: **`connections:write`**
5. Click **Generate**
6. Copy the token (starts with `xapp-1-...`)

### Step 3: Subscribe to Bot Events (REQUIRED)

> **⚠️ This is the most commonly missed step.** Without event subscriptions, Socket Mode connects successfully but **no message events are delivered**. The logs will show `connected` but thread replies will be silently ignored by Slack.

1. In the left sidebar, go to **Event Subscriptions**
2. Toggle **Enable Events** to **On**
3. Under **Subscribe to bot events**, add:

| Event | Why |
|---|---|
| `message.channels` | Receive messages posted in public channels the bot is in |
| `message.groups` | Receive messages posted in private channels the bot is in |

4. Click **Save Changes**
5. **Reinstall the app** — go to **Install App** → **Reinstall to Workspace** (Slack requires reinstallation after adding event scopes)

> **Note:** No Request URL is needed when Socket Mode is enabled — the app connects outbound to Slack.

### Step 4: Configure the Environment Variable

Add to your deployment:

```
SLACK_APP_TOKEN=xapp-1-your-app-level-token
```

Optionally configure the session TTL (default: 3 minutes):

```
THREAD_SESSION_TTL=5m
```

For Helm deployments, add the token to `secretValues` in your values file:

```yaml
secretValues:
  slack-app-token: "xapp-1-..."
```

And optionally set the TTL in `env`:

```yaml
env:
  THREAD_SESSION_TTL: "3m"
```

### Step 5: Verify

1. Deploy the updated app
2. Check logs for: `Socket Mode enabled — listening for thread replies`
3. Run a slash command (e.g. `/ovad give me my todo tasks`)
4. After the bot replies in a thread, type a follow-up **directly in that thread** (no slash command needed)
5. The bot should respond within the thread

### How It Works

- When a slash command is executed, arbetern posts an audit message (creating a thread) and opens a **thread session**
- The session stays active for `THREAD_SESSION_TTL` (default: 3 minutes), refreshed on every message
- Any user reply in that thread is automatically routed through the same agent — no `/command` prefix needed
- After the TTL expires with no activity, the session closes and new thread replies are ignored

### Troubleshooting

**"SLACK_APP_TOKEN not set" warning in logs**
- Socket Mode is disabled. Set the `SLACK_APP_TOKEN` environment variable.

**Socket Mode connects but thread replies are ignored (no events)**
- **Most common cause:** Bot event subscriptions are missing. Go to **Event Subscriptions** → **Subscribe to bot events** and add `message.channels` + `message.groups`, then **reinstall the app to your workspace**.
- Check that Socket Mode is enabled: **Socket Mode** → toggle **On**.

**Thread replies are ignored (events arrive but no response)**
- Check that the session hasn't expired (default 3 minutes of inactivity)
- Check the logs for `[session] expired` — if seen, increase `THREAD_SESSION_TTL`

**Debugging: enable verbose Socket Mode logging**
- Set `SOCKET_MODE_DEBUG=1` to see raw wire-level protocol events from the Slack SDK
- All received events are logged with their type — look for `[socket-mode] events-api:` and `[socket-mode] message:` entries
- If you see `connected` but never see `events-api:` or `message:` entries, the event subscriptions are not configured

**Bot responds to its own messages (loop)**
- This shouldn't happen — the bot filters out its own user ID. Check logs for the `Bot user ID: ...` line at startup.
