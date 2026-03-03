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
| `ATLASSIAN_PROJECT` | *(optional)* Default Jira project key (e.g. `ENG`). If omitted, the bot will ask which project to use. |

### Option B: OAuth 2.0 (client credentials)

| Environment Variable | Description |
|---|---|
| `ATLASSIAN_URL` | Your Atlassian instance URL (e.g. `https://yourorg.atlassian.net`) |
| `ATLASSIAN_CLIENT_ID` | OAuth 2.0 client ID from your Atlassian Developer Console app |
| `ATLASSIAN_CLIENT_SECRET` | OAuth 2.0 client secret |
| `ATLASSIAN_PROJECT` | *(optional)* Default Jira project key (e.g. `ENG`) |

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
  atlassian-project: "ENG"
```

### OAuth 2.0

```yaml
secretValues:
  atlassian-url: "https://yourorg.atlassian.net"
  atlassian-client-id: "your-oauth-client-id"
  atlassian-client-secret: "your-oauth-client-secret"
  atlassian-project: "ENG"
```

Or create the secret manually:

```bash
# Basic Auth
kubectl create secret generic arbetern-secrets \
  --from-literal=atlassian-url=https://yourorg.atlassian.net \
  --from-literal=atlassian-email=bot@yourorg.com \
  --from-literal=atlassian-api-token=ATATT3x... \
  --from-literal=atlassian-project=ENG

# OAuth 2.0
kubectl create secret generic arbetern-secrets \
  --from-literal=atlassian-url=https://yourorg.atlassian.net \
  --from-literal=atlassian-client-id=your-client-id \
  --from-literal=atlassian-client-secret=your-client-secret \
  --from-literal=atlassian-project=ENG
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
   | `EDIT_ISSUES` | Edit Issues | Update ticket descriptions and summaries (used by Seihin agent) |

3. **Generate the API token** from the service account:
   - Log in as the service account at [id.atlassian.com](https://id.atlassian.com/manage-profile/security/api-tokens).
   - Create a classic API token (without scopes).
   - Use this token as `ATLASSIAN_API_TOKEN` and the service account email as `ATLASSIAN_EMAIL`.

> **Note:** If your Jira instance uses a custom permission scheme, verify that the *Member* role includes `BROWSE_PROJECTS` and `CREATE_ISSUES`. Check under **Jira Administration → Permission Schemes**.

## Usage

Once configured, the bot exposes the following Jira tools to the LLM:

| Tool | Description |
|---|---|
| **create_jira_ticket** | Create an issue with summary, description, type, labels, assignee, and team |
| **list_jira_projects** | Discover available project keys |
| **search_jira_issues** | Search for issues using JQL (e.g., find all in-progress tickets for a user) |
| **get_jira_issue** | Fetch full details of a specific issue by key (including description) |
| **update_jira_issue** | Update an issue's summary and/or description |

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
