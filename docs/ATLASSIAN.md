# Atlassian Integration (Jira + Confluence)

Arbetern integrates with the Atlassian Cloud platform — creating Jira tickets from Slack conversations, searching and reading Confluence pages, and more.

## Required Credentials

Arbetern supports two authentication methods:

### Option A: Basic Auth (email + API token)

| Environment Variable | Description |
|---|---|
| `ATLASSIAN_URL` | Your Atlassian instance URL (e.g. `https://yourorg.atlassian.net`) |
| `ATLASSIAN_EMAIL` | The email address of the Atlassian account used for authentication |
| `ATLASSIAN_API_TOKEN` | An Atlassian API token (not your account password) |
| `JIRA_PROJECT` | *(optional)* Default Jira project key (e.g. `ENG`). If omitted, the bot will ask which project to use. |

### Option B: OAuth 2.0 (client credentials)

| Environment Variable | Description |
|---|---|
| `ATLASSIAN_URL` | Your Atlassian instance URL (e.g. `https://yourorg.atlassian.net`) |
| `ATLASSIAN_CLIENT_ID` | OAuth 2.0 client ID from your Atlassian Developer Console app |
| `ATLASSIAN_CLIENT_SECRET` | OAuth 2.0 client secret |
| `JIRA_PROJECT` | *(optional)* Default Jira project key (e.g. `ENG`) |

> **Note:** If both Basic Auth and OAuth credentials are configured, OAuth takes precedence.

## Step-by-step Setup

### Method 1: Basic Auth (API token)

#### 1. Get Your Atlassian Instance URL

This is the base URL you use to access Jira, e.g. `https://yourorg.atlassian.net`.

#### 2. Create a Jira API Token

Arbetern uses **Basic Auth** (email + token), which requires a **classic API token**.

> **Important:** Do NOT use "Create API token with scopes" — scoped tokens are designed for Atlassian Forge/Connect apps and do not work with Basic Auth REST API calls. They will fail with HTTP 401.

1. Log in to [https://id.atlassian.com/manage-profile/security/api-tokens](https://id.atlassian.com/manage-profile/security/api-tokens).
2. Click **Create API token** (the button *without* scopes).
3. Give it a label (e.g. `Arbetern`) and click **Create**.
4. Copy the token — it is only shown once.

Classic API tokens inherit all permissions of the account that created them, so the account must have permission to create issues in the target Jira project.

> **Tip:** Do NOT use a personal admin account — the token would inherit full admin access. Create a dedicated service account instead (see below).

> **Tip:** Use a dedicated service account rather than a personal account, so the bot's access isn't tied to a single person.

#### 3. Note the Account Email

Use the email address associated with the Atlassian account that created the API token. This is the value for `ATLASSIAN_EMAIL`.

#### 4. Find Your Project Key

Open Jira and navigate to the target project. The project key is the prefix shown on issue IDs (e.g. if issues are `ENG-123`, the key is `ENG`).

You can also let the bot discover projects at runtime — users can ask it to `list jira projects`.

### Method 2: OAuth 2.0 (client credentials)

OAuth 2.0 is recommended when you want app-level access without tying credentials to a specific user account.

#### 1. Create an OAuth 2.0 App

1. Go to [developer.atlassian.com/console/myapps](https://developer.atlassian.com/console/myapps/).
2. Click **Create** → **OAuth 2.0 integration**.
3. Give it a name (e.g. `Arbetern`) and click **Create**.

#### 2. Configure Scopes

In the app settings, go to **Permissions** and configure scopes for both Jira and Confluence:

**Jira API** → **Configure**:

| Scope | Why |
|---|---|
| `read:jira-work` | Search and read issues |
| `write:jira-work` | Create and update issues |
| `read:jira-user` | Resolve user accounts for assignee lookups |

**Confluence API** → **Configure**:

| Scope | Why |
|---|---|
| `read:confluence-content.all` | Search and read Confluence pages |
| `read:confluence-space.summary` | List Confluence spaces |

#### 3. Get Client Credentials

Go to **Settings** in your app and copy:
- **Client ID** → `ATLASSIAN_CLIENT_ID`
- **Secret** → `ATLASSIAN_CLIENT_SECRET`

#### 4. Authorize the App

The app must be authorized for your Atlassian site:
1. Go to **Authorization** → **Add** → select your site.
2. Grant the scopes configured above.

> **Note:** OAuth tokens are automatically refreshed by Arbetern before they expire.


## Helm Deployment

### Basic Auth

```yaml
secretValues:
  atlassian-url: "https://yourorg.atlassian.net"
  atlassian-email: "bot@yourorg.com"
  atlassian-api-token: "ATATT3x..."
  jira-project: "ENG"
```

### OAuth 2.0

```yaml
secretValues:
  atlassian-url: "https://yourorg.atlassian.net"
  atlassian-client-id: "your-oauth-client-id"
  atlassian-client-secret: "your-oauth-client-secret"
  jira-project: "ENG"
```

Or create the secret manually:

```bash
# Basic Auth
kubectl create secret generic arbetern-secrets \
  --from-literal=atlassian-url=https://yourorg.atlassian.net \
  --from-literal=atlassian-email=bot@yourorg.com \
  --from-literal=atlassian-api-token=ATATT3x... \
  --from-literal=jira-project=ENG

# OAuth 2.0
kubectl create secret generic arbetern-secrets \
  --from-literal=atlassian-url=https://yourorg.atlassian.net \
  --from-literal=atlassian-client-id=your-client-id \
  --from-literal=atlassian-client-secret=your-client-secret \
  --from-literal=jira-project=ENG
```
```

## Required Jira Permissions

Classic API tokens inherit **all** permissions of the account, so using a personal admin account is dangerous. Create a dedicated service account with minimal access.

### Setting Up a Service Account

1. **Create a new Atlassian account** for the bot (e.g. `arbetern-bot@yourorg.com`).
   - Use a shared mailbox or group alias so it's not tied to one person.
   - Do **not** add this account to any admin groups (`jira-administrators`, `site-admins`, etc.).

2. **Grant project-level access** — in each Jira project the bot should work with:
   - Go to **Project Settings → People**.
   - Add the service account with the **Member** role.
   - This grants the minimum permissions needed:

   | Permission Key | Name | Why |
   |---|---|---|
   | `BROWSE_PROJECTS` | Browse Projects | List projects and view project metadata |
   | `CREATE_ISSUES` | Create Issues | Create new tickets via the API |
   | `EDIT_ISSUES` | Edit Issues | Update ticket summaries, descriptions, labels, priority and custom fields (`update_jira_issue`, `assign_jira_team`) |
   | `ASSIGN_ISSUES` | Assign Issues | Set the assignee (`account_id` of `update_jira_issue`, and `assignee` of `create_jira_ticket`) |
   | `TRANSITION_ISSUES` | Transition Issues | Move tickets to another status (`transition_jira_issue`, and `done_on_merge` in projects) |
   | `SCHEDULE_ISSUES` | Schedule Issues | Put tickets in a sprint (`assign_jira_active_sprint`, `assign_jira_next_sprint`, and the sprint options of `create_jira_ticket`) |
   | `MODIFY_REPORTER` | Modify Reporter | Set the Reporter to the originating human on create (see [Reporter attribution](#reporter-attribution)) |

3. **Generate the API token** from the service account:
   - Log in as the service account at [id.atlassian.com](https://id.atlassian.com/manage-profile/security/api-tokens).
   - Create a classic API token (without scopes).
   - Use this token as `ATLASSIAN_API_TOKEN` and the service account email as `ATLASSIAN_EMAIL`.

> **Note:** If your Jira instance uses a custom permission scheme, verify that the *Member* role includes `BROWSE_PROJECTS` and `CREATE_ISSUES`. Check under **Jira Administration → Permission Schemes**.

## Reporter attribution

When Arbetern creates a Jira ticket it sets the **Reporter** to the human who actually
requested it, so every ticket is traceable back to a real person rather than the bot
service account. The requester is resolved to a Jira account ID and passed as
`reporter.accountId` on issue creation. How the requester is identified depends on the
surface:

| Surface | Reporter resolved from | Result |
|---|---|---|
| **Slack** (slash command, DM) | Slack user → email (`users.info`) → Jira account | Reporter = the Slack user who ran the command |
| **Web chat UI** (with SSO) | OAuth-proxy-verified email (`X-Auth-Request-Email`) → Jira account | Reporter = the signed-in chat user |
| **Scheduled workflow / automation** | — (no human requester) | Reporter defaults to the service account (Arbetern) |

Notes:

- The web-chat path requires the [oauth2-proxy SSO](../README.md#authentication-sso) in
  front of the app so the verified email is available; without a proxy (local dev) the
  reporter falls back to the service account.
- Resolution is **email-first** (the reliable key); the Slack path falls back to a
  confident display-name match. If the requester cannot be matched to a Jira account, the
  ticket is still created with the service account as reporter.
- Setting Reporter on create requires the service account to hold the **Modify Reporter**
  project permission (separate from Create Issues). Without it, Jira rejects the reporter
  field — Arbetern then retries and creates the ticket with the default (service-account)
  reporter, logging that the permission is missing.


## Usage

Once configured, the bot exposes the following Jira tools to the LLM:

| Tool | Description |
|---|---|
| **create_jira_ticket** | Create an issue with summary, description, type, labels, assignee, and team, optionally in the active sprint (`use_active_sprint`) or the next one (`use_next_sprint`) |
| **list_jira_projects** | Discover available project keys |
| **search_jira_issues** | Search for issues using JQL (e.g., find all in-progress tickets for a user); `fields` adds up to 10 more fields to each result, by id or name, such as a custom field's option values. A field Jira does not return is named as such, not shown as empty |
| **get_jira_issue** | Fetch full details of a specific issue by key (including description); `fields` adds up to 10 more fields, by id or name |
| **update_jira_issue** | Update an issue in one write: summary, description, labels (`labels` adds, `remove_labels` removes), priority, assignee (`account_id`), and any other field by id or name through `fields`. List fields are merged, never replaced, and the result shows each edited field as Jira holds it afterwards. See [Editing fields](#editing-fields) |
| **transition_jira_issue** | Move an issue to another status through one of the workflow transitions open to it, by target status or transition name, optionally setting a resolution. When none fits, the error lists the transitions available from the current status |
| **assign_jira_active_sprint** | Move an issue into the project's active sprint |
| **assign_jira_next_sprint** | Move an issue into the project's next sprint: the first future sprint on the board running the active sprint, or the first future sprint when none is active |

| **search_confluence_pages** | Search for Confluence pages by keyword |
| **get_confluence_page** | Fetch the full content of a Confluence page by ID |
| **list_confluence_spaces** | List available Confluence spaces |

Example Slack commands:

```
/ovad create a Jira ticket from the test plan above
/ovad open a bug in project QA for the login timeout issue
/seihin go over my jira tickets in progress and edit and redefine the description and its details
/seihin review ticket ENG-123 and improve its description
```

### Editing fields

`update_jira_issue` reads the issue and its edit metadata first, checks every
requested change, and only then writes them all in one request. A refused
edit, whether by this check or by Jira, changes nothing.

```json
{
  "issue_key": "ENG-123",
  "labels": ["triaged"],
  "priority": "High",
  "fields": {
    "customfield_10010": {"add": [{"value": "Acme"}], "remove": ["Globex"]},
    "customfield_10011": "<team uuid>",
    "duedate": "2026-12-31"
  }
}
```

- **Fields** are named by id (`customfield_10010`) or exact name. A name that
  several fields share is refused; use the id.
- **List fields** (labels, multi-selects, checkboxes, components, versions,
  multi-user pickers) take `{"add": [...], "remove": [...]}`, or a plain value
  or list, which is added. The issue keeps its other values. Replacing a whole
  list, or clearing it with `null`, is refused, so a list can only lose the
  values named in `remove`.
- **Other fields** take their new value, or `null` to clear it. Users take a
  Jira accountId, a Team field takes the team UUID from `resolve_jira_team`,
  and rich-text fields such as the description take markdown.
- **Allowed values.** Select options, priorities, components and versions can
  be given by name or as Jira objects (`{"value": "Acme"}`, `{"id": "10200"}`).
  They are checked against the values the field allows on that issue. A value
  it does not allow is refused with the closest matches or the allowed list,
  and nothing is written. A field missing from the edit metadata, such as one
  off the edit screen, is still sent, and Jira's own check applies.
- **Result.** Each edited field is read back after the write and shown with its
  previous value. Values the issue already had, and values to remove it did not
  have, are listed rather than treated as errors. When nothing needs to change,
  nothing is written.
