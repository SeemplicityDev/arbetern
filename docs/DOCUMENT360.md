# Document360 Integration

Arbetern integrates with **Document360** so the **pulse** (customer-success)
agent can answer questions from the knowledge base directly in Slack: search
published articles, browse the category tree, list articles and read an
article's content. It uses the Document360 **v3 customer API**.

The integration is **read-only**. The connector only ever issues `GET`
requests; nothing creates, edits, publishes or deletes content, and there is no
tool that could.

> **Scope: this integration is restricted to the `pulse` agent.**
> The Document360 tools are advertised exclusively to pulse and the dispatch
> layer rejects the call from any other agent, even if its model fabricates a
> tool name. The allowlist lives in one place — `restrictedIntegrations` in
> [`commands/helpers.go`](../commands/helpers.go) (`"document360": {"pulse"}`).
> To expose Document360 to additional agents, add their IDs there; both tool
> registration and dispatch read the same map.

## Tools

| Tool | Description |
|---|---|
| `document360_list_workspaces` | List the project's workspaces (knowledge-base versions) with IDs; marks the default. Other tools use the default when `workspace_id` is omitted |
| `document360_search` | Keyword search across the **published, visible** articles of a workspace. Returns titles and `article_id`s; drafts are never returned. `page` / `page_size` (1..100) paginate |
| `document360_list_categories` | The workspace's nested category tree with category IDs |
| `document360_list_articles` | One page of article summaries (ID, status, version, last updated). With `category_id` the connector scans the workspace and keeps only that category's articles, since the API has no server-side category filter |
| `document360_get_article` | One article by `article_id`: title, status, public link and the latest **published** body as text, with snippets and variables resolved. Long bodies are truncated with a note |

`workspace_id` accepts a workspace ID, slug or name and is matched against the
listed workspaces, so an unknown value is refused rather than sent to the API.

## Required Credentials

| Environment Variable | Required | Description |
|---|---|---|
| `DOCUMENT360_API_KEY` | yes | Scoped API key (`d360_sk_…`), sent as the `X-API-Key` header |
| `DOCUMENT360_PROJECT_ID` | no | Project UUID. Needed only when the key can see more than one project; otherwise the single visible project is discovered |
| `DOCUMENT360_REGION` | no | API region: `eu` (default), `us` or `ca`. Selects the API host; any other value disables the integration with a startup log line |

Startup logs the resolved project:

```
Document360 integration enabled (region: eu, project: Product Docs (46f48bc7-…))
```

The project is verified in the background with retries, so a slow or briefly
unavailable API never blocks startup; the tools are advertised once the key has
been accepted.

## Setup

1. In Document360 go to **Settings → Knowledge base portal → API keys** and
   click **Create API key**.
2. Give it a **read-only content role** and scope its content access to the
   workspaces (and, if you like, languages or categories) the agent may read. A
   key cannot exceed the creator's own access.
3. Copy the secret once it is shown — it cannot be recovered later — and set it
   as `DOCUMENT360_API_KEY`.
4. If your portal is hosted in the US or Canada region, set
   `DOCUMENT360_REGION` accordingly.
5. If the key can see several projects, set `DOCUMENT360_PROJECT_ID`; the
   startup log lists the candidates when this is needed.

The API is documented at <https://apidocs.document360.com>.

## Helm

Set the kebab-case keys in `secretValues` (see
[`helm/values.yaml`](../helm/values.yaml)):

```yaml
secretValues:
  document360-api-key: "d360_sk_…"
  # document360-project-id: "…"   # optional
  # document360-region: "eu"      # optional: eu, us or ca
```

The chart wires them into the matching environment variables on the app
container only when `document360-api-key` is present.

Because the tools are gated to `pulse` in code, prefer mounting the key under
`customCredentials.pulse` so the secret never reaches other agents' pods:

```yaml
customCredentials:
  pulse:
    document360-api-key: "d360_sk_…"
```

## Reliability

- Every call is a `GET`, so a retry can never duplicate a side effect.
- `429` and `5xx` responses and transport errors are retried up to four times
  with exponential backoff and jitter, honouring `Retry-After`.
- When the API reports an exhausted per-minute read window
  (`X-RateLimit-Remaining: 0`, or a `429`), the connector pauses new requests
  until the window resets instead of spending retries on further `429`s.
- The workspace list is cached for ten minutes with single-flight refresh; a
  failed refresh keeps serving the last good list.
- Category and filtered-article walks are bounded (at most ten pages each) so
  one question cannot exhaust the key's rate limit.

## Security

- **Read-only.** The client has no code path for `POST`, `PUT`, `PATCH` or
  `DELETE`. Pair it with a read-only key so the credential itself cannot write
  either.
- The API host is chosen from a fixed region allowlist — the key is never sent
  to a configurable host.
- The key is supplied via an environment variable (a Kubernetes Secret in the
  Helm chart) and is never logged. Search terms are redacted from tool-call
  logs and never appear in request logging.
- Error text shown to the model is sanitized (no request internals); the API's
  own message and request ID go to the pod log only.
- Access is restricted to the `pulse` agent via the central allowlist; the
  dispatch layer refuses the tools for any other agent.
