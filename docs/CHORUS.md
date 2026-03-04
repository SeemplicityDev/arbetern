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

Arbetern uses the Chorus REST API v3:

| Endpoint | Method | Description |
|---|---|---|
| `/api/v3/engagements` | GET | List meetings within a date range |
| `/api/v3/conversations/{meetingId}` | GET | Get detailed analytics for a specific meeting |
| `/api/v3/momentum/deals` | POST | List deals with momentum scores and risk indicators |

## Available Tools

Once configured, the Pulse agent exposes the following Chorus tools:

| Tool | Description |
|---|---|
| **chorus_list_meetings** | List meetings/calls within a date range. Returns title, participants, duration, and link |
| **chorus_get_conversation** | Get detailed analytics for a specific meeting — summary, talk ratio, sentiment, topics, action items, trackers, and participants |
| **chorus_list_deals** | List deals from Chorus Momentum with scores, activity counts, risk indicators, and stage info |

## Example Slack Commands

```
/pulse give me all deals from the last month
/pulse what are my biggest deals closing this quarter
/pulse show me meetings from last week
/pulse get conversation details for meeting abc-123
/pulse what deals have low momentum scores
/pulse summarize call activity for the Acme account
```

## Combining with Salesforce

Chorus works best alongside Salesforce. When both integrations are configured, Pulse can cross-reference data:

```
/pulse give me all deals closing this month with their call activity
/pulse which renewals have no recent meetings
/pulse prepare a QBR summary for the Acme account with call insights and pipeline data
```

The LLM automatically decides which tools to invoke — no special syntax needed.
