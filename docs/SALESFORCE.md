# Salesforce Integration

Arbetern reads from Salesforce via the SOQL Query API, primarily to power the
**Pulse** agent's Customer Success workflows (account health dashboards,
opportunity tracking, renewal-signal surfacing). Authentication uses the
**OAuth 2.0 client credentials** flow against a Connected App — no user
interaction, no refresh tokens, no password.

## Required Credentials

| Environment Variable | Required | Description |
|---|---|---|
| `SF_CONSUMER_KEY` | yes | Connected App **Consumer Key** (client ID) |
| `SF_CONSUMER_SECRET` | yes | Connected App **Consumer Secret** (client secret) |
| `SF_LOGIN_URL` | no | Salesforce login URL. Default `https://login.salesforce.com`. Use `https://test.salesforce.com` for sandbox orgs, or your My Domain URL (`https://<mydomain>.my.salesforce.com`) if My Domain is enforced |

> When all three vars are unset the Salesforce client stays uninitialized and
> the related tools (`salesforce_query`, `salesforce_describe`) and the
> account-dashboard fan-out simply skip Salesforce.

## Creating the Connected App

1. **Setup → App Manager → New Connected App** (Lightning) → choose
   "Create a Connected App".
2. **Basic Information**:
   - Name: `Arbetern`
   - API Name: `Arbetern`
   - Contact Email: a service-account mailbox.
3. **API (Enable OAuth Settings)** — check **Enable OAuth Settings**:
   - **Callback URL**: any valid HTTPS URL (e.g. `https://localhost/oauth/callback`). Client-credentials flow doesn't use it but the form requires one.
   - **Selected OAuth Scopes**:
     - `Manage user data via APIs (api)` — required for SOQL queries.
     - `Perform requests at any time (refresh_token, offline_access)` — required to keep a server-side token without re-auth.
   - **Enable Client Credentials Flow** — check this box. Without it the
     `grant_type=client_credentials` request returns `unsupported_grant_type`.
   - Leave PKCE / require-secret-for-web-server toggles at their defaults.
4. **Save** → wait ~10 minutes for the new app to propagate across Salesforce
   data centers before testing.
5. **Manage Consumer Details** → reveal and copy the **Consumer Key** and
   **Consumer Secret** → store them as `SF_CONSUMER_KEY` / `SF_CONSUMER_SECRET`.

### Bind a Run-As User to the Connected App

Client-credentials flow always runs as a single Salesforce user — Arbetern's
queries inherit that user's profile, permission sets, sharing rules, and field-
level security. Pick a dedicated Integration User (not a real human's account):

1. **Setup → Manage Connected Apps → Arbetern → Edit Policies**.
2. Under **Client Credentials Flow**, set **Run As** to the integration user.
3. Save.

The integration user needs:
- API access enabled on its profile.
- Read access to every SObject Arbetern queries (Account, Opportunity, Contact,
  Case, plus any custom objects you reference in workflow prompts).
- Field-Level Security read access to every field referenced in your SOQL.

## Helm Deployment

```yaml
secretValues:
  sf-consumer-key: "3MVG9..."
  sf-consumer-secret: "your-consumer-secret"
  # sf-login-url: "https://yourorg.my.salesforce.com"  # only if not the default
```

Or create the secret manually:

```bash
kubectl create secret generic arbetern-secrets \
  --from-literal=sf-consumer-key=3MVG9... \
  --from-literal=sf-consumer-secret=your-consumer-secret
```

## Local Development

```bash
export SF_CONSUMER_KEY="3MVG9..."
export SF_CONSUMER_SECRET="your-consumer-secret"
# export SF_LOGIN_URL="https://test.salesforce.com"   # sandboxes
go run .
```

You can sanity-check the credentials with curl:

```bash
curl -X POST "$SF_LOGIN_URL/services/oauth2/token" \
  -d grant_type=client_credentials \
  -d "client_id=$SF_CONSUMER_KEY" \
  -d "client_secret=$SF_CONSUMER_SECRET"
```

A successful response returns an `access_token` and `instance_url`. Common
errors:

| Error | Likely cause |
|---|---|
| `invalid_client` | Wrong consumer key/secret, OR the Connected App hasn't propagated yet (wait 10 min) |
| `unsupported_grant_type` | "Enable Client Credentials Flow" checkbox is off |
| `invalid_grant` | Run-As user is missing, deactivated, locked, or has no API access |

## Tools Exposed to the LLM

| Tool | Purpose |
|---|---|
| `salesforce_query` | Run an arbitrary SOQL query and return all records (paginates `nextRecordsUrl` automatically; capped at 10 000 records per call). |
| `salesforce_describe` | Describe an SObject — list its fields, types, and labels. Useful when the model needs to discover field API names before composing a query. |

Both tools target API version `v62.0` against the `services/data/<version>/`
REST endpoints.

## Where Salesforce Data Surfaces

- **Source dashboards** (any agent): include `salesforce_query` as a source
  when creating a dashboard via `/<agent> create dashboard …`.
- **Prompt dashboards / workflows**: a prompt-driven dashboard or a scheduled
  workflow can call `salesforce_query` directly inside its tool-loop.

## Troubleshooting

- **Tool returns "salesforce client not configured"** — one of
  `SF_CONSUMER_KEY` / `SF_CONSUMER_SECRET` is empty. Check the pod env vars.
- **`401 INVALID_SESSION_ID`** — the cached access token expired and the
  refresh failed. Restart the pod or wait for the next automatic retry; the
  client retries on 401 by re-running `client_credentials` once per request.
- **`400 MALFORMED_QUERY`** — your SOQL has a syntax error. Quote string
  literals with single quotes, not double quotes. Date literals are bare
  (`2026-01-01T00:00:00Z`), not quoted.
- **Empty results despite data existing** — check the Run-As user's sharing
  rules and field-level security; client-credentials queries only see what
  that user can see.
