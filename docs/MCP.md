# MCP & Connectors

Arbetern can call tools hosted on [Model Context Protocol](https://modelcontextprotocol.io)
servers. A registered server is a **connector**: it is tested once to discover
its tools, and from then on every allowed agent sees those tools in its LLM
tool loop next to the built-in integrations.

## Registering a connector

Open **MCP & Connectors** in the web UI (or `POST /api/mcp`) and provide:

| Field | Meaning |
|---|---|
| `name` | Display name; also the prefix of the exposed tool names |
| `url` | The server's Streamable HTTP endpoint (absolute `http(s)` URL) |
| `headers` | Sent on every request, e.g. `Authorization: Bearer …` |
| `agents` | Agent IDs allowed to use the tools; empty means every agent |
| `enabled` | Disabled connectors keep their configuration but expose no tools |

Header values may reference environment variables as `${NAME}`. The value is
resolved when a request is made, so the token can live in the Helm Secret
instead of the persisted JSON. Literal values are stored in the state bucket
and returned masked (`••••••••`) by the API; sending the mask back on update keeps
the stored value.

Connectors are stored at `mcp/<id>.json` in the state bucket (see
[STATE.md](STATE.md)).

## Testing and discovery

**Test** (`POST /api/mcp/<id>/test`) runs `initialize`, `notifications/initialized`
and a paginated `tools/list`, then stores the server info, the tools and the
outcome (`last_check`, `last_error`). Only connectors whose last test succeeded
expose tools. Changing the URL or headers clears the discovered tools until the
connector is tested again.

## How agents see the tools

Each discovered tool becomes an LLM function named `mcp_<connector-slug>_<tool>`
(non-alphanumeric characters replaced, 64 characters max, de-duplicated). The
description is prefixed with `[MCP: <connector>]` and the server's `inputSchema`
is passed through unchanged. A call opens a fresh session (`initialize` +
`tools/call`), flattens the text content blocks, and returns up to 32 KiB of
output to the model. A tool-side `isError` result is returned as text prefixed
with `Error from <connector>` so the model can react.

Agents outside a connector's allowlist never receive its tools.

## Current scope

- Transport: Streamable HTTP with JSON or SSE responses. stdio and legacy
  HTTP+SSE servers are not supported.
- Authentication: static headers only; no OAuth flow.
- Only `tools` are used. Resources, prompts and sampling are ignored.
- One session per call; no long-lived sessions or server-initiated requests.
- The server is reached from where arbetern runs, so it must be routable from
  the cluster and is trusted as much as any other integration.

## Who can change connectors

Everyone with UI access can see connectors and their tools. Adding, editing,
testing and deleting them can be limited with `MCP_ADMIN_TEAMS` (Slack user
group IDs) and `MCP_ADMIN_EMAILS` (addresses or domains), set from the chart's
`mcp.adminTeams` / `mcp.adminEmails`:

```yaml
mcp:
  adminTeams:
    - S0A6S3KNNLW
```

The check works like an agent's `allowed_teams`: the email oauth2-proxy injects
is resolved to a Slack user, whose group membership is checked. Members see the
usual buttons; everyone else gets a read-only page and the API answers 403 to
every verb but GET. Both lists empty means no restriction.
