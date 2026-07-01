# Freshworks Integration

Arbetern integrates with the **Freshworks** product suite so the **pulse**
(customer-success) agent can read support and sales context directly from Slack:

- **Freshdesk** (ticketing) — list, search and read support tickets and their
  conversation threads.
- **Freshchat** (conversations) — read live-chat conversations and their
  messages.
- **Freshworks CRM / Freshsales** (sales) — search contacts, deals and
  accounts, and read a contact or deal by ID.

All tools are **read-only** (list / search / get). None of them create or
modify anything in Freshworks.

> **Scope: this integration is restricted to the `pulse` agent only.** The
> Freshworks tools are advertised exclusively to pulse and the dispatch layer
> rejects the call from any other agent, even if its model fabricates a tool
> name. The allowlist lives in one place — `restrictedIntegrations` in
> [`commands/helpers.go`](../commands/helpers.go) (`"freshworks": {"pulse"}`).
> To expose Freshworks to additional agents, add their IDs there; both tool
> registration and dispatch read the same map.

Each product is configured **independently** — set only the products you use.
A product with missing credentials is simply not advertised; the others still
work.

## Tools

| Tool | Product | Description |
|---|---|---|
| `freshdesk_list_tickets` | Freshdesk | List recent tickets (newest-updated first), optionally filtered by `updated_since` |
| `freshdesk_get_ticket` | Freshdesk | Get one ticket by ID, optionally with its conversation thread |
| `freshdesk_search_tickets` | Freshdesk | Search tickets with the Freshdesk filter query syntax (e.g. `priority:4 AND status:2`) |
| `freshchat_get_conversation` | Freshchat | Get a conversation header by conversation ID |
| `freshchat_get_conversation_messages` | Freshchat | Get the messages in a conversation |
| `freshworks_crm_search` | CRM | Search contacts, deals and accounts by term |
| `freshworks_crm_get_contact` | CRM | Get a contact by ID |
| `freshworks_crm_get_deal` | CRM | Get a deal/opportunity by ID |

## Required Credentials

Each product authenticates differently:

| Environment Variable | Product | Required | Description |
|---|---|---|---|
| `FRESHDESK_DOMAIN` | Freshdesk | for Freshdesk | Freshdesk host, e.g. `acme.freshdesk.com` |
| `FRESHDESK_API_KEY` | Freshdesk | for Freshdesk | Freshdesk API key (HTTP Basic username; the password is a fixed placeholder) |
| `FRESHCHAT_URL` | Freshchat | for Freshchat | Freshchat API base **including** `/v2`, e.g. `https://acme-123.freshchat.com/v2` |
| `FRESHCHAT_API_TOKEN` | Freshchat | for Freshchat | Freshchat API token (sent as a Bearer JWT) |
| `FRESHWORKS_CRM_DOMAIN` | CRM | for CRM | Freshworks CRM host, e.g. `acme.myfreshworks.com` |
| `FRESHWORKS_CRM_API_KEY` | CRM | for CRM | Freshworks CRM API key (sent as `Authorization: Token token=<key>`) |

A product is enabled only when **both** of its values are set. Startup logs the
configured products:

```
Freshworks integration enabled (products: [Freshdesk Freshchat Freshworks CRM])
```

## Setup

### Freshdesk

1. Sign in to Freshdesk and open your **Profile settings**.
2. Copy **Your API Key** (right-hand panel). Authentication is HTTP Basic with
   the API key as the username and any non-empty password.
3. Set `FRESHDESK_DOMAIN` to your portal host (e.g. `acme.freshdesk.com`) and
   `FRESHDESK_API_KEY` to the key.

The API is documented at <https://developers.freshdesk.com/api/>.

### Freshchat

1. In Freshchat go to **Admin → API Tokens** and generate a token.
2. Note your API region URL — the base ends in `/v2`
   (e.g. `https://acme-123.freshchat.com/v2`). It is shown alongside the token.
3. Set `FRESHCHAT_URL` to that base and `FRESHCHAT_API_TOKEN` to the token.

The API is documented at <https://developers.freshchat.com/api/>.

### Freshworks CRM (Freshsales)

1. In the CRM go to **Settings → API Settings** and copy your **API key**.
2. Set `FRESHWORKS_CRM_DOMAIN` to your CRM host (e.g. `acme.myfreshworks.com`)
   and `FRESHWORKS_CRM_API_KEY` to the key.

The API is documented at <https://developers.freshworks.com/crm/api/>.

## Helm

Set the corresponding kebab-case keys in `secretValues` (see
[`helm/values.yaml`](../helm/values.yaml)). Each product is optional:

```yaml
secretValues:
  freshdesk-domain: "acme.freshdesk.com"
  freshdesk-api-key: "..."
  freshchat-url: "https://acme-123.freshchat.com/v2"
  freshchat-api-token: "..."
  freshworks-crm-domain: "acme.myfreshworks.com"
  freshworks-crm-api-key: "..."
```

The chart wires each pair into the matching environment variables on the app
container only when its `-domain` / `-url` key is present.

Because the tools are gated to `pulse` in code, prefer mounting the credentials
under `customCredentials.pulse` so the secret material never reaches other
agents' pods:

```yaml
customCredentials:
  pulse:
    freshdesk-domain: "acme.freshdesk.com"
    freshdesk-api-key: "..."
    freshchat-url: "https://acme-123.freshchat.com/v2"
    freshchat-api-token: "..."
    freshworks-crm-domain: "acme.myfreshworks.com"
    freshworks-crm-api-key: "..."
```

## Security

- All tools are **read-only**; there is no write path to Freshworks.
- API keys/tokens are provided via environment variables (Kubernetes Secrets in
  the Helm chart) and are never logged.
- Access is restricted to the `pulse` agent via the central allowlist; the
  dispatch layer refuses the tools for any other agent.
