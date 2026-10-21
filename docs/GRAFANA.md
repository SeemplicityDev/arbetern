# Grafana Integration

Arbetern integrates with **Grafana** so the **ovad**, **pulse**, **seihin** and
**hermes** agents can find dashboards, read how they are built, and run the
queries saved on their panels to see the data the panels show. It talks to one
Grafana instance (Grafana Cloud or self-hosted) over the HTTP API with a
service-account token.

The integration is **read-only**. The connector reads dashboards, folders,
datasources and annotations, and runs the queries dashboard panels already
define. It has no tool that writes, and no tool that runs a free-form query:
an agent can only execute what a dashboard author saved.

> **Scope: the tools follow the Datadog observability set.**
> They are advertised to ovad, pulse, seihin and hermes, and the dispatch layer
> rejects them for any other agent. The allowlist lives in one place —
> `restrictedIntegrations` in [`commands/helpers.go`](../commands/helpers.go)
> (`"grafana": {...}`).

## Tools

The tools mirror the read-only tools of Grafana's own MCP server
([mcp-grafana](https://github.com/grafana/mcp-grafana)), prefixed `grafana_`.

| Tool | Description |
|---|---|
| `grafana_search_dashboards` | Dashboards by title fragment, tags (all must match) and folder, with uid, folder, tags and link. Up to 100 per page |
| `grafana_search_folders` | Folders by title fragment, with the uid `grafana_search_dashboards` filters on |
| `grafana_get_dashboard_summary` | Title, folder, default time range, refresh, variables with their saved selections, and every panel's id, title, type and query count grouped by row (collapsed rows included) |
| `grafana_get_dashboard_panel_queries` | The saved queries of one panel (`panel_id`, full text) or of every panel (excerpted), with each query's datasource and the variables it references |
| `grafana_get_dashboard_property` | A JSONPath read of the dashboard JSON, e.g. `$.panels[?(@.id == 4)]` or `$..targets[*].rawSql`. Supports keys, indexes, `*`, `..`, unions, slices and `?()` filters |
| `grafana_list_datasources` | Datasources the account may query, with uid, type and the default, filterable by type and name |
| `grafana_run_panel_query` | Runs the queries of up to 8 panels in one call and returns their data |
| `grafana_get_annotations` | Annotations, newest first, filterable by time window, dashboard, panel, tags and type |

Not included, on purpose:

- **Write tools** — dashboard, folder, annotation, alerting, incident and
  datasource changes.
- **Free-form query tools** such as `query_sql` and `query_prometheus`. The
  MCP server itself drops raw SQL under `--disable-write` because it passes the
  query through unfiltered; here every query an agent runs comes from a
  dashboard.
- **Alerting, Incident, OnCall, Sift, profiling and admin reads.** Add them as
  separate tools if a deployment relies on them.

## Running panels

`grafana_run_panel_query` does what the dashboard does when it loads, then
shapes the result for a model.

- **Time range.** The dashboard's own default range applies unless `start` /
  `end` are given. Both accept Grafana's relative syntax (`now-6h`,
  `now-1d/d`), RFC 3339, a date, or unix seconds / milliseconds.
- **Variables.** Saved selections apply unless overridden in `variables`.
  Comma-separate several values of a multi-value variable; `All` selects every
  value. Datasource variables resolve to the selected datasource, or — with no
  selection saved — to the one Grafana would pick (the default when it matches,
  else the first match of the plugin type and regex). Passing a datasource uid
  for such a variable points the whole panel at another datasource.
- **Formatting.** Variables are interpolated with Grafana's syntax (`$var`,
  `${var}`, `[[var]]`, `${var:format}`) and its formats (`singlequote`,
  `sqlstring`, `csv`, `pipe`, `regex`, `glob`, `json`, `lucene`, …). A
  multi-value variable without a format follows the datasource's own rule —
  `'a','b'` for ClickHouse and SQL datasources, a regex alternation for
  Prometheus and Loki.
- **Macros.** Built-ins such as `$__from`, `$__to`, `$__interval` and
  `$__range` are filled in by the connector. The ClickHouse plugin's
  `$__conditionalAll(condition, $var)` becomes `1=1` when the variable is set
  to All or empty, as in the browser. Datasource macros such as
  `$__timeFilter`, `$__timeInterval` and `$__interval_s` are left for the
  datasource to expand, exactly as Grafana's frontend does.
- **Interval.** The bucket size is the range over about 200 points, rounded to
  Grafana's familiar steps (20s, 2m, 5m, 1h, …) and raised to the panel's
  minimum interval. Pass `interval` (e.g. `1m`) to choose it.
- **ClickHouse.** Queries run in table format, so the rows are exactly what
  the SQL returns. No `LIMIT` is appended, so trailing `SETTINGS` / `FORMAT`
  clauses keep working; the output is capped instead.
- **Output.** Short results come back as rows. A long time series is
  summarised per series — grouped by its label columns — as last / avg / min /
  max / sum with the point count and span. Each panel carries an *open in
  Grafana* link with the same range and variables. A failing query is reported
  next to the query text that was sent, and the other panels still return.

Variables whose options Grafana computes when the dashboard loads (query
variables) cannot be expanded to "All" without a saved option list; the tool
asks for explicit values in that case, unless the reference sits inside
`$__conditionalAll`.

Not supported: dashboards stored in Grafana's v2 schema, library panels,
panels that reuse another panel's results (`-- Dashboard --`) and ad hoc
filters.

## Required Credentials

| Environment Variable | Required | Description |
|---|---|---|
| `GRAFANA_URL` | yes | Instance URL, e.g. `https://acme.grafana.net`. A sub-path such as `https://ops.example.com/grafana` works |
| `GRAFANA_SERVICE_ACCOUNT_TOKEN` | yes | Service-account token (`glsa_…`), sent as a Bearer token |

Startup verifies the token against `/api/user` in the background with retries
and logs the account:

```
[grafana] connected to https://acme.grafana.net as arbetern (sa-1-arbetern)
```

The tools are advertised once the token has been accepted.

## Setup

1. In Grafana go to **Administration → Users and access → Service accounts**
   and create an account with the **Viewer** role. Viewer covers every tool:
   reading dashboards, folders and annotations, and querying the datasources it
   is allowed to query.
2. Add a token to the account and copy it.
3. Set `GRAFANA_URL` and `GRAFANA_SERVICE_ACCOUNT_TOKEN`.

What the agents can query is what the account can query. Datasource
permissions are the control: a datasource the account may not query fails with
a permission error, and the datasource list falls back to the datasources the
account may query when it lacks `datasources:read`.

## Helm

Set both keys in `secretValues` (see [`helm/values.yaml`](../helm/values.yaml)):

```yaml
secretValues:
  grafana-url: "https://acme.grafana.net"
  grafana-service-account-token: "glsa_…"
```

The chart wires them into `GRAFANA_URL` and `GRAFANA_SERVICE_ACCOUNT_TOKEN`
only when both are present. Either key can also be set per agent under
`customCredentials.<agent>`.

## Reliability and cost

- Reads (`GET`) are retried up to three times on `429` and `5xx` with
  exponential backoff and jitter, honouring `Retry-After`.
- `/api/ds/query` is retried only on `429`, `502` and `503`; a `500` or `504`
  is not repeated, so a slow or failing query is not re-run against the
  backing database.
- Panels in one call run at most three at a time. The request timeout is 90
  seconds, leaving room for datasource queries.
- Dashboards are cached for two minutes and the datasource list for five, so
  a summary, its panel queries and a run cost one dashboard read.
- Redirects are refused, so the token is never sent to another host.

## Security

- **Read-only.** The connector's only `POST` is `/api/ds/query`, with queries
  taken from dashboards. Pair it with a Viewer account so the credential cannot
  write either.
- The token comes from an environment variable (a Kubernetes Secret in the
  Helm chart) and is never logged.
- Query results go to the agent's model and into the Slack thread it answers
  in, like any other integration's data. Keep the account's datasource
  permissions to what agents may see.
- Error text shown to the model is sanitized; Grafana's own message and trace
  ID go to the pod log.
