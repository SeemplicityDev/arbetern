# Freshworks Integration

Arbetern integrates with the **Freshworks** product suite so the **pulse**
(customer-success) and **seihin** (product-management) agents can read support
and sales context directly from Slack:

- **Freshdesk** (ticketing) — list, search and read support tickets and their
  conversation threads, and record an outcome on a ticket as a private note or
  a tag.
- **Freshchat** (conversations) — read live-chat conversations and their
  messages.
- **Freshworks CRM / Freshsales** (sales) — search contacts, deals and
  accounts, and read a contact or deal by ID.

Freshchat and the CRM are **read-only** (list / search / get). Freshdesk is
read-only apart from two writes on an existing ticket: a **private note**
(internal, never shown to the requester) and a **tag**. Nothing creates, closes
or replies to a ticket, and no tool can send anything to a customer.

> **Scope: this integration is restricted to the `pulse` and `seihin` agents.**
> The Freshworks tools are advertised exclusively to those agents and the
> dispatch layer rejects the call from any other agent, even if its model
> fabricates a tool name. The allowlist lives in one place —
> `restrictedIntegrations` in
> [`commands/helpers.go`](../commands/helpers.go) (`"freshworks": {"pulse", "seihin"}`).
> To expose Freshworks to additional agents, add their IDs there; both tool
> registration and dispatch read the same map.

Each product is configured **independently** — set only the products you use.
A product with missing credentials is simply not advertised; the others still
work.

## Tools

| Tool | Product | Description |
|---|---|---|
| `freshdesk_list_tickets` | Freshdesk | List recent tickets (newest-updated first), optionally filtered by `updated_since` or by requester email |
| `freshdesk_get_ticket` | Freshdesk | Get one ticket by ID, optionally with its conversation thread |
| `freshdesk_search_tickets` | Freshdesk | Search tickets with the Freshdesk filter query syntax (e.g. `priority:4 AND status:2`, `agent_id:123`, `company_id:99`). Valid fields only: status, priority, type, tag, agent_id, group_id, company_id, created_at, updated_at, due_by, fr_due_by, cf_<custom> — there is no free-text/`company`/`subject` search. `page` (1..10, 30 per page) walks a longer backlog; `exclude_tags` drops matched tickets carrying a tag, since the query syntax has no negation operator |
| `freshdesk_find_agent` | Freshdesk | Resolve an agent's `agent_id` by email or name (for `agent_id:<id>` searches of assigned tickets) |
| `freshdesk_list_ticket_fields` | Freshdesk | List ticket fields (system + custom); use it to find a custom attribute's exact `cf_<name>` key (e.g. "Customer Name" → `cf_customer_name`) to scope a search by customer |
| `freshdesk_add_note` | Freshdesk | Add a **private** (internal) note to a ticket — visible to Freshdesk agents only, never to the requester, and it notifies nobody |
| `freshdesk_add_tags` | Freshdesk | Add tags to a ticket, keeping its existing tags. Re-adding a tag writes nothing and says so, which makes a tag a processed-once marker |
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

Because the tools are gated to `pulse` and `seihin` in code, prefer mounting the
credentials under `customCredentials.pulse` (and `customCredentials.seihin` if
that agent should also reach Freshworks) so the secret material never reaches
other agents' pods:

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

## Recurring jobs

`exclude_tags` and `freshdesk_add_tags` are designed to work together, so a
scheduled workflow can process each ticket exactly once without keeping state
of its own:

1. Search for the tickets to handle and pass the marker tag in `exclude_tags`,
   so anything already handled is filtered out.
2. Do the work for one ticket, then write the result with `freshdesk_add_note`.
3. Tag the ticket **last**, with `freshdesk_add_tags`.

Because the tag is written last, a ticket that errors part-way through stays
untagged and is simply picked up again on the next run. The tag is sticky: a
tagged ticket is never revisited, even if the requester updates it later.

## Security

- Freshchat and CRM are **read-only**. The only writes are a private note and a
  tag on a Freshdesk ticket; there is no path to reply to a requester, change a
  ticket's status or assignment, or write to Freshchat or the CRM.
- The note and tag writes need a Freshdesk API key whose agent role permits
  editing tickets. A read-only key keeps the tools advertised but every write
  fails with HTTP 403.
- API keys/tokens are provided via environment variables (Kubernetes Secrets in
  the Helm chart) and are never logged.
- Access is restricted to the `pulse` and `seihin` agents via the central
  allowlist; the dispatch layer refuses the tools for any other agent.
