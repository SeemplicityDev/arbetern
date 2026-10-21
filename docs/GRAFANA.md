# Grafana Integration

Arbetern integrates with **Grafana** so the **ovad**, **pulse**, **seihin** and
**hermes** agents can find dashboards, read how they are built, run the queries
saved on their panels, and query the ClickHouse datasources behind them —
logs, metrics and traces stored by ClickStack / OpenTelemetry, or any table
with read-only SQL. It talks to one Grafana instance (Grafana Cloud or
self-hosted) over the HTTP API with a service-account token.

The integration is **read-only**. Nothing creates, edits or deletes anything in
Grafana or in a datasource; every query goes through Grafana's datasource query
API and is checked before it is sent (see [Security](#security)).

> **Scope: the tools follow the Datadog observability set.**
> They are advertised to ovad, pulse, seihin and hermes, and the dispatch layer
> rejects them for any other agent. The allowlist lives in one place —
> `restrictedIntegrations` in [`commands/helpers.go`](../commands/helpers.go)
> (`"grafana": {...}`).

## Tools

The dashboard tools mirror the read-only tools of Grafana's own MCP server
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
| `grafana_get_sql_schema` | A ClickHouse datasource's tables with engine, rows, size, partition key, sort key and retention, or one table's columns and key membership |
| `grafana_query_sql` | One read-only ClickHouse statement on one or more ClickHouse datasources |
| `grafana_search_logs` | OpenTelemetry logs: total, a breakdown and the newest lines, filtered by service, severity, text, attributes and trace id |
| `grafana_query_metrics` | OpenTelemetry metrics: the metric names in range, or one metric per interval grouped by service or attributes |
| `grafana_search_traces` | OpenTelemetry traces: one trace's spans, or matching spans with a per-operation summary of counts, errors and latency |

Not included, on purpose:

- **Write tools** — dashboard, folder, annotation, alerting, incident and
  datasource changes.
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
  selection saved — to the one Grafana would pick. A datasource override may be
  a uid, a name or a fragment unique to one datasource, and points the whole
  panel at that datasource.
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
  Grafana's familiar steps and raised to the panel's minimum interval. Pass
  `interval` (e.g. `1m`) to choose it.
- **Output.** Short results come back as rows. A long time series is
  summarised per series — grouped by its label columns — as last / avg / min /
  max / sum with the point count and span. Each panel carries an *open in
  Grafana* link with the same range and variables. A failing query is reported
  next to the query text that was sent, and the other panels still return.

Query variables whose options Grafana computes when the dashboard loads cannot
be expanded to "All" without a saved option list; the tool asks for explicit
values in that case, unless the reference sits inside `$__conditionalAll`.

Not supported: dashboards stored in Grafana's v2 schema, library panels, panels
that reuse another panel's results (`-- Dashboard --`) and ad hoc filters.

## Querying ClickHouse datasources

The five ClickHouse tools run through Grafana's datasource query API, so they
reach exactly the datasources the service account may query, with that
datasource's own credentials and network path.

- **Which datasources.** By default a call runs on every visible ClickHouse
  datasource and answers in one section per datasource — for example one per
  cluster. `datasources` narrows it to uids, names or fragments unique to one.
  A datasource that cannot be reached right now is reported in one line; the
  others still answer. `grafana_get_sql_schema` uses the first datasource that
  answers unless one is named.
- **Time range and macros.** `start` / `end` (default the last hour) set the
  range the ClickHouse plugin's macros expand to: `$__timeFilter(col)`,
  `$__fromTime`, `$__toTime`, `$__timeInterval(col)`, `$__interval_s`.
  `$__from`, `$__to` and `$__interval` are filled in by the connector.
- **Logs, metrics and traces.** `grafana_search_logs` and
  `grafana_search_traces` read the logs and traces tables configured on each
  datasource; `grafana_query_metrics` reads the OpenTelemetry exporter's
  `otel_metrics_<type>` tables in the same database. Column names are the
  exporter's defaults. Attribute filters match the signal's own attributes and
  its resource attributes, exactly or with `*` wildcards.
- **Counters.** A counter's default aggregation is its increase per interval
  (`rate` divides by the interval). Delta counters are summed; a cumulative
  counter's increase is taken per series before summing.
- **SQL.** `grafana_query_sql` takes one statement. A `SELECT` without a
  top-level `LIMIT` gets one, placed before a trailing `SETTINGS` or `FORMAT`
  clause; at most `max_rows` rows (default 100, up to 500) are shown per
  datasource, with a note when more exist. Errors come back with the executed
  query, macros expanded.

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
account may query when it lacks `datasources:read`. Give each ClickHouse
datasource a database user with `SELECT`-only grants (ideally a `readonly`
profile) so the database refuses writes on its own.

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
- Datasource queries are retried only on `429`, `502` and `503`; a `500` or
  `504` is not repeated, so a slow or failing query is not re-run against the
  backing database.
- Panels, and datasources in a multi-datasource call, run at most three at a
  time. The request timeout is 90 seconds; the database's own execution limit
  still applies.
- The logs, metrics and traces tools always bound the time column and send
  their queries for one datasource in a single request.
- Dashboards are cached for two minutes and the datasource list for five.
- Redirects are refused, so the token is never sent to another host.

## Security

- **Read-only, in layers.** Grafana side: a Viewer account. Connector side:
  every SQL statement must start with `SELECT`, `WITH`, `SHOW`, `DESCRIBE`,
  `DESC`, `EXISTS` or `EXPLAIN`, may not contain a write or session keyword
  anywhere, must be a single statement, and may not call table functions that
  reach outside the database (`url`, `s3`, `remote`, `file`, `mysql`, …).
  Database side: a `SELECT`-only user. The logs, metrics and traces tools build
  their SQL from escaped literals and quoted identifiers.
- Panel queries are the ones dashboard authors saved; the connector only fills
  in variables and macros.
- The token comes from an environment variable (a Kubernetes Secret in the
  Helm chart) and is never logged.
- Query results go to the agent's model and into the Slack thread it answers
  in, like any other integration's data. Keep the account's datasource
  permissions to what agents may see.
- Error text shown to the model is sanitized: connection failures are reported
  without host names, and Grafana's own message and trace ID go to the pod log.
