# Chorus / ZoomInfo Integration

Arbetern integrates with the Chorus (ZoomInfo) API to bring call intelligence, conversation analytics, and deal momentum data directly into Slack via the **Pulse** agent.

## Required Credentials

| Environment Variable | Required | Description |
|---|---|---|
| `CHORUS_API_TOKEN` | yes | Your Chorus API token (generated in Chorus → Personal Settings) |
| `CHORUS_BASE_URL` | no | Chorus API base URL (default: `https://chorus.ai`). Override for on-prem or custom endpoints |

## Getting Your API Token

1. Log in to [chorus.ai](https://chorus.ai).
2. Click your avatar (top-right) → **Personal Settings**.
3. Scroll to the **API** section.
4. Click **Generate Token** (or copy your existing token).
5. Use this token as `CHORUS_API_TOKEN`.

> **Note:** Tokens inherit the permissions of the user who generates them. For production use, create a dedicated service account with access to the meetings, conversations, and deals you want to query.

## Helm Deployment

```yaml
secretValues:
  chorus-api-token: "your-chorus-api-token"
  # chorus-base-url: "https://chorus.ai"  # only needed for custom endpoints
```

Or create the secret manually:

```bash
kubectl create secret generic arbetern-secrets \
  --from-literal=chorus-api-token=your-chorus-api-token
```

## Local Development

```bash
export CHORUS_API_TOKEN="your-chorus-api-token"
# export CHORUS_BASE_URL="https://chorus.ai"  # optional
go run .
```

## API Endpoints Used

Arbetern uses two Chorus API versions:

**v3** — primary, used for listing/searching conversations (engagements):

| Endpoint | Method | Description |
|---|---|---|
| `/v3/engagements` | GET | List conversations with rich filters (date, duration, type, participant, team, disposition, trackers) |

**v1 (JSON:API)** — used for single-conversation detail and sales qualifications:

| Endpoint | Method | Description |
|---|---|---|
| `/api/v1/conversations/:id` | GET | Get detailed analytics for a specific conversation |
| `/api/v1/sales-qualifications` | POST | Extract Sales Qualification Framework (MEDDIC) data from a recording |
| `/api/v1/sales-qualifications/:recording_id` | GET | Retrieve previously extracted SQF analysis |
| `/api/v1/sales-qualifications/actions/writeback-crm` | POST | Write qualification-derived field updates back to CRM |

> **Note:** The v3 `/engagements` endpoint is the canonical way to list/search conversations. The v1 `/conversations/:id` endpoint is only for fetching full detail on a single conversation.

## Available Tools

Once configured, the Pulse agent exposes the following Chorus tools:

| Tool | Description |
|---|---|
| **chorus_list_conversations** | List/search conversations via v3 API. Supports filters: date range, duration, engagement type, participant email, user/team IDs, disposition, trackers. Returns compact summaries with meeting notes, action items, participants, and opportunity info |
| **chorus_get_conversation** | Get full details for a specific conversation by ID (v1 API) — recording analytics, deal info, participants with roles/titles, trackers, action items, and metrics |
| **chorus_create_sales_qualification** | Extract MEDDIC / Sales Qualification Framework data from a recording. Returns structured fields with supporting quotes |
| **chorus_get_sales_qualification** | Retrieve a previously extracted Sales Qualification analysis by recording ID |
| **chorus_writeback_crm** | Write qualification-derived field updates back to CRM (e.g. Salesforce Opportunity) |

## Example Slack Commands

```
/pulse show me meetings from last week
/pulse what conversations happened with Acme in the last month
/pulse get conversation details for ID abc123
/pulse give me all deals from the last 30 days
/pulse which deals are closing this quarter
/pulse summarize call activity for the Acme account
/pulse show me calls with john@acme.com
/pulse run MEDDIC analysis on recording abc123
/pulse get the sales qualification for my last call with Acme
/pulse write back MEDDIC fields to the Salesforce opportunity
```

> When users ask about "deals", Pulse lists Chorus conversations and extracts embedded deal/opportunity data from engagements that have linked CRM deals.

## Combining with Salesforce

Chorus works best alongside Salesforce. When both integrations are configured, Pulse can cross-reference data:

```
/pulse give me all deals closing this month with their call activity
/pulse which renewals have no recent meetings
/pulse prepare a QBR summary for the Acme account with call insights and pipeline data
```

The LLM uses `chorus_list_conversations` to find calls with embedded deal data, then `salesforce_query` for CRM opportunity details, and presents a unified view.
