# Google Drive / Sheets Integration

Arbetern integrates with **Google Drive** and **Google Sheets** so the **pulse**
agent can:

- see which Drive folders it has been given (`drive_list_folders`);
- resolve a file by name across all of them (`drive_find_file`);
- **read any file's contents** — CSV, text, JSON, Markdown, logs, plus Google
  Docs/Sheets/Slides converted to text on the fly (`drive_read_file`);
- inspect a spreadsheet's tabs before writing to it
  (`sheets_get_spreadsheet_info`);
- read cell ranges (`sheets_read_range`); and
- **append batches of rows** to a tab (`sheets_append_row`).

> **Scope: this integration is restricted to the `pulse` agent only.** Its tools
> are advertised exclusively to pulse and the dispatch layer rejects the call
> from any other agent, even if its model fabricates the tool name. The
> allowlist lives in one place — `restrictedIntegrations` in
> [`commands/helpers.go`](../commands/helpers.go) (`"google": {"pulse"}`). To
> expose Google to additional agents, add their IDs there; both tool
> registration and dispatch read the same map.

## Scope: sharing is the configuration

There is nothing to configure about *which* folders the agent can use. The
service account has no Drive of its own, so it can only see what somebody
explicitly shared with it — and the connector simply adopts that:

1. At first use it asks Drive **"what has been shared with me?"** and adopts
   those folders and shared drives, plus their subtrees, as its working set.
2. To grant access, **share a folder** with the service account's email address.
   It becomes usable within the 5-minute discovery cache — no config change, no
   redeploy, no restart.
3. To revoke access, **unshare** it. Same window.

How the agent behaves depends on how many folders it can see:

| Folders shared | Behaviour |
|---|---|
| **None** | Every tool fails with a message naming the service-account address to share with |
| **Exactly one** | It is used implicitly. The agent is instructed never to ask which folder to use |
| **Several** | Searches span all of them by default. The agent only narrows to one when the user names it, or when a search returns ambiguous matches |

`drive_list_folders` is how the model finds this out, and how you can check what
the connector actually sees.

### Optional: pinning folders

`GOOGLE_DRIVE_FOLDER_IDS` (comma-separated) confines the connector to specific
folders **even when the service account can see more**. Use it when the account's
shares are broader than what this agent should touch — for example when the same
key is reused elsewhere. Leave it unset for the normal case.

### What the in-code guard does and does not buy you

Every Drive and Sheets call resolves the target's folder ancestry first and
refuses anything out of scope **before the request is sent**. Being precise about
what that is worth:

- **With discovery (the default), the guard mirrors the Drive ACL rather than
  narrowing it.** Its value is failing fast with an actionable message instead of
  a bare 404, and giving the model a definite answer to "which folders can I
  use". A file the service account was separately granted one-to-one is also
  accepted, because refusing an explicit grant would be surprising.
- **With pins configured, the guard is a real additional boundary** — it refuses
  folders the account *can* see but that this deployment did not authorise.

Either way, the outer boundary is the same one Google enforces: **nothing is
reachable unless someone shared it with the service account.** Verdicts are
cached for 10 minutes so a batch write does not re-walk every target's ancestry.

### Two constraints that do not change

- **No domain-wide delegation.** The account acts as **itself**, never
  impersonating a Workspace user. Nothing here can read a person's Drive, mail or
  calendar.
- **Drive is read-only; only Sheets values are writable.** The connector cannot
  create, rename, move, delete or re-share any file. See
  [Limitations](#limitations).

## What the connector does not disclose

The folders this integration reads hold commercial and customer data, and two
paths would otherwise carry that data outside the Drive ACL that governs it.

**Pod logs do not receive payloads.** The tool loop logs every call's arguments,
which is right for control data (a repo, a date window) and wrong for a
`sheets_append_row` batch carrying customer names, contract values and renewal
dates. Those arguments are redacted to their *shape* before the log line is
written, so the log still answers "what did the agent do" without answering
"what did it say":

```
LLM called tool: sheets_append_row({"raw":false,"targets":"<2 targets, 3 rows redacted>"})
LLM called tool: drive_find_file({"limit":200,"names":"<2 items redacted>"})
```

Spreadsheet IDs stay in the clear for correlation; file names, search terms, cell
ranges and cell values do not. Arguments that fail to parse — the usual cause is
a tool call truncated at the model's output ceiling — are logged as
`<N bytes, unparsed>` rather than as half a payload. The redaction list lives in
`sensitiveArgTools` in [`commands/helpers.go`](../commands/helpers.go); every
other connector logs verbatim as before.

File **contents** are never logged: a read logs the byte count, content type and
file ID only.

**Slack channels do not receive internals.** A Google API failure is split in
two. The full diagnostic — Google's own message — goes to the pod log, where an
operator can act on it. Only a sanitized sentence reaches the tool result and
therefore the channel. Raw API text reads to a user as a broken product and
invites the model to editorialise about internals nobody can act on:

| Situation | Log | Channel |
|---|---|---|
| Bad field selector (a connector bug) | `Invalid field selection sharedWithMe` | "the Google connector issued a request Google rejected… This is a defect in arbetern… do not retry it" |
| Bad A1 range (the model's own mistake) | `Unable to parse range: 'Renewls'` | passed through verbatim — this is what lets the model fix itself |
| 403 naming the GCP project | `The caller does not have permission for project seemplicity-mgmt` | "access was denied… never shared with this connector's service account, or shared read-only" |
| 404 naming a file ID | `File not found: 1abcXYZ` | "the file was not found… re-resolve it with drive_find_file" |

The **service-account address is not printed into channels** either. It
identifies internal infrastructure and reveals the GCP project name, and a Slack
reply travels further than the channel once it is screenshotted. Errors point at
the operator surfaces instead; the address is shown on the arbetern integrations
page and in the boot log, which is where whoever needs to act on it is looking.

## Auth mode

Arbetern runs headless in a pod, so there is no interactive OAuth flow available.
Authentication is the **JWT-bearer grant** (RFC 7523): arbetern signs a
short-lived assertion with the service account's RSA private key and exchanges it
for an access token at `https://oauth2.googleapis.com/token`. Tokens are cached
and refreshed 5 minutes before expiry.

## Required Credentials

| Environment Variable | Required | Description |
|---|---|---|
| `GOOGLE_CREDENTIALS_JSON` | yes | The service-account key file, **base64-encoded** (raw JSON is also accepted) |
| `GOOGLE_DRIVE_FOLDER_IDS` | no | Comma-separated folder IDs to confine the connector to. Unset = use every folder shared with the account |
| `GOOGLE_SCOPES` | no | Override the OAuth scopes. Defaults to `spreadsheets` + `drive.readonly` |

The key is the only required value. Access itself is granted by sharing a folder
with the service account — see [Scope](#scope-sharing-is-the-configuration).

Base64 is the expected form for the key because a PEM private key's `\n` escapes
survive a Kubernetes Secret round-trip far more reliably as one opaque blob:

```bash
base64 -w0 < seemplicity-mgmt-a8650c77398e.json   # Linux
base64 -i   seemplicity-mgmt-a8650c77398e.json    # macOS
```

Startup logs on success — the second line tells you what the connector can
actually reach, which is the thing worth checking after a deploy:

```
Google integration enabled (service account: arbetern-pulse-sheets@seemplicity-mgmt.iam.gserviceaccount.com, scope: all folders shared with the account)
Google Drive: 1 folder reachable ("CS Reporting", 1AbCdEf...) — it will be used automatically
```

With several folders shared:

```
Google Drive: 3 locations reachable: "CS Reporting", "Renewals FY26", "Acme Account Plans"
```

With nothing shared yet:

```
Google Drive access check failed — nothing is shared with arbetern-pulse-sheets@… — share a Drive folder with that address (Editor to allow appends)
```

A failed first token exchange is not fatal — the client retries every 5 seconds
in the background and the tools become available once it connects.

## Setup

1. **Create the service account.** In the Google Cloud console for the project
   (e.g. `seemplicity-mgmt`) go to **IAM & Admin → Service Accounts → Create
   service account**. It needs **no** IAM roles on the project — Drive access
   comes from the Drive share, not from GCP IAM. Note its email, of the form
   `<name>@<project>.iam.gserviceaccount.com`.

2. **Create a JSON key.** On the account's **Keys** tab choose **Add key →
   Create new key → JSON**. The file downloads once; base64 it as above and
   store it as `google-credentials-json`.

3. **Enable the APIs.** In the same project enable **Google Sheets API** and
   **Google Drive API** (APIs & Services → Library). Without these every call
   returns a 403 naming the disabled API.

4. **Create and share a folder.** Create the folder in Drive (or use a shared
   drive), then share it with the service account's email address:

   - **Editor** — to allow `sheets_append_row` to write.
   - **Viewer** — enough for reading files and ranges.

   That is the whole access grant. There is no folder ID to configure: the
   connector discovers the share within 5 minutes. Share more folders later and
   they appear on their own; unshare one and it disappears.

   Share **exactly one** folder if you want the agent to use it without ever
   asking which folder to use.

   You only need a folder ID if you intend to *pin* the connector to specific
   folders — take it from the folder's URL:

   ```
   https://drive.google.com/drive/folders/1AbCdEfGhIjKlMnOpQrStUvWxYz0123456
                                          └──────────── this ────────────┘
   ```

   Files in **subfolders** are in scope too (up to 6 levels deep).

5. **Confirm what it sees.** After the pod is up, ask pulse
   `what Google Drive folders do you have access to` — it calls
   `drive_list_folders` and lists them. Or check the boot log line above.

6. **Verify the boundary.** Ask pulse to read a file ID from a folder that was
   *not* shared. The call must be refused before it is sent, with:

   ```
   file <id> is out of scope for this connector: it is not inside any folder
   shared with arbetern-pulse-sheets@…
   ```

### Scopes

The default is:

```
https://www.googleapis.com/auth/spreadsheets
https://www.googleapis.com/auth/drive.readonly
```

`drive.file` is narrower on paper, but it authorises only files the app itself
created or that were individually granted through a picker — it **cannot
enumerate the contents of a folder that was shared with the account**, so
`drive_find_file` would return nothing. `drive.readonly` is therefore the Drive
scope, and it is not as broad as it reads: the service account has no Drive of
its own and no visibility beyond the shared folder, so "read every file you can
see" means "read the shared folder". Write access is confined to
`spreadsheets` — the connector cannot create, move, delete or re-share any Drive
file.

Override with `GOOGLE_SCOPES` (comma- or space-separated) if your Workspace
policy requires something different.

## Reading files

`drive_read_file` streams a file's content and returns a bounded window of text.
Drive has two download paths and the tool picks the right one, which is the usual
cause of a confusing 403 when done by hand:

| File kind | How it is fetched | What comes back |
|---|---|---|
| CSV, TXT, JSON, YAML, Markdown, logs | `files.get?alt=media` — raw bytes | The text |
| Google Doc | `files.export` | Markdown (or `text/plain` via `export_as`) |
| Google Sheet | `files.export` | CSV — for tab/range control use `sheets_read_range` instead |
| Google Slides | `files.export` | Plain text |
| PDF, images, .xlsx, archives | not extracted | A note naming the type and size |

**Everything streams.** The body is consumed through a limit reader with a
caller cap, so a 400 MB video dropped into the shared folder cannot be pulled
into memory: the read stops at the cap, the rest of the body is drained and
discarded, and the result is marked truncated. Nothing is ever written to disk.

**Paging.** A truncated result tells the model exactly how to continue
(`offset_bytes=…`). For raw downloads the offset becomes an HTTP `Range` header,
so continuing a large file does not re-transfer the part already seen. Exports
do not support ranges, so those skip by discarding — fine, since an exported Doc
or Sheet is small.

**Binary files are reported, not guessed at.** A model handed 200 KB of decoded
JPEG will confabulate about its contents, so the tool returns the type, the size
and a suggestion instead — convert the file to a Google Doc/Sheet in Drive, or
share the link with the user. Detection trusts the content type first and falls
back to sniffing the head for NUL bytes and invalid UTF-8, which is what actually
separates a CSV from an image.

| Limit | Value |
|---|---|
| Content returned per call | 200 KB default, 1 MiB ceiling (`max_bytes`) |
| Bytes pulled off the wire per call | 64 MiB hard cap |

## Batching and rate limits

The Sheets API allows **60 write requests per minute per user** and **300 read
requests per minute per project**, and a single scheduled workflow can target
many spreadsheets in one run. The connector is built so volume does not turn into
429s:

- **Rows are batched per destination.** Every row bound for the same
  `(spreadsheet, tab)` pair travels in **one** `values:append` request. 50 rows
  across 15 spreadsheets is **one tool call and 15 requests**, not 50 — which
  also matters for the 200-round per-tick tool budget in
  [`config/config.go`](../config/config.go).
- **Reads are batched per spreadsheet.** Several ranges collapse into one
  `values:batchGet`.
- **Name lookups are batched.** `drive_find_file` resolves an entire list of
  names with a single Drive query per folder chunk, across every shared folder.
- **Client-side quota budget.** A rolling per-minute allowance (55 writes, 250
  reads) makes a burst *wait* rather than discover the limit through errors.
- **Retry with backoff.** 429, 500, 502, 503 and 504 are retried up to 5 times
  with exponential backoff (1s → 30s) plus jitter, honouring `Retry-After` when
  the response supplies it. Everything else — 400 bad range, 403 not shared, 404
  wrong ID — fails immediately, because retrying a caller error only delays it.
- **Bounded concurrency.** Up to 4 spreadsheets are written in parallel, so a
  15-spreadsheet batch finishes well inside one tick.

> **Why `values:append` and not `values:batchUpdate`?** `batchUpdate` writes to
> **fixed ranges** and has no append semantics — using it to add rows after
> existing data would require first reading each tab's row count, which costs an
> extra request per tab and races any concurrent writer. `values:append` is the
> correct primitive for appending and is *already* one request per tab with all
> rows in it, so the request count is the same and the write is safe.

### Per-target failure isolation

A batch is not all-or-nothing. If one target fails — a tab that does not exist, a
spreadsheet outside the folder — the others are still written, and the tool result
names each target's outcome so the model can correct and retry **only** what
failed:

```
Appended 45 rows across 14 of 15 targets (15 API requests)

Spreadsheet             Tab        Rows  Range              Status
──────────────────────  ─────────  ────  ─────────────────  ──────
1AbC…                   Renewals   3     Renewals!A12:C14   ok
…

1 target failed:
• `1XyZ…` tab `Renewls` — google API error (HTTP 400 INVALID_ARGUMENT): Unable to parse range: 'Renewls'
```

### Safety limits

| Limit | Value |
|---|---|
| Rows per `sheets_append_row` call | 5 000 |
| `(spreadsheet, tab)` targets per call | 100 |
| Ranges per `sheets_read_range` call | 50 |
| Cells returned by one read | 20 000 |
| Files returned by `drive_find_file` | 200 |
| Subfolder depth searched / scope-checked | 6 |
| Shared folders / drives adopted | 50 |

Appends use `insertDataOption=INSERT_ROWS`, which never overwrites whatever sits
below the current table — a footer or a second table on the same tab is safe.

## Helm Deployment

Only the key is needed:

```yaml
createSecret: true

secretValues:
  google-credentials-json: "PASTE_BASE64_OF_SERVICE_ACCOUNT_KEY_JSON"
  # google-drive-folder-ids: "1AbCd...,1ZyXw..."   # optional: confine to these folders
  # google-scopes: ""                              # optional
```

Or create the secret manually:

```bash
kubectl create secret generic arbetern-secrets \
  --from-literal=google-credentials-json="$(base64 -w0 < key.json)"
```

The chart emits `GOOGLE_CREDENTIALS_JSON` only when `google-credentials-json` is
non-empty in `secretValues`, so leaving the block unset cleanly disables the
integration. `GOOGLE_DRIVE_FOLDER_IDS` and `GOOGLE_SCOPES` are emitted
independently.

### Restricting to a single agent at deploy time — recommended

Because the tools are already gated to pulse in code, the right production setup
is to mount the Google credentials **only for pulse** via the chart's per-agent
credential overlay, instead of the global secret. The folder holds commercial and
customer data, so keeping the key material off every other agent's process
environment is worth the extra block:

```yaml
createSecret: true

customCredentials:
  pulse:
    google-credentials-json: "REPLACE_ME"
```

This provisions `arbetern-pulse-secrets`, mounts it at
`/etc/arbetern/agent-credentials/pulse/`, and the app overlays those keys on top
of the global config for pulse only:

```
Google override for agent "pulse" (service account: arbetern-pulse-sheets@…, scope: all folders shared with the account)
```

## Available Tools

| Tool | Description |
|---|---|
| **drive_list_folders** | List the folders and shared drives the connector can reach. No arguments. When exactly one is listed the agent uses it silently; when several are, searches span all of them |
| **drive_find_file** | Find files by name across every shared folder and its subfolders, returning each file's ID. Pass a whole list in `names` — resolved by a batched query. `name_contains` does a substring match; omitting both lists everything. `folder_id` narrows to one folder, `spreadsheets_only` to Google Sheets, `recursive` (default true) controls subfolder descent, `limit` caps at 200 |
| **drive_read_file** | Read a file's contents. Uploaded files come back as text; Google Docs/Sheets/Slides are converted automatically (`export_as` overrides the target). `max_bytes` caps the window and `offset_bytes` continues a truncated read. Binary files return a note naming the type instead of content |
| **sheets_get_spreadsheet_info** | List a spreadsheet's title and tabs with row/column counts. Use it to get the exact tab name before appending — tab names are case- and space-sensitive |
| **sheets_read_range** | Read cell values. Accepts `range` (one A1 range or a bare tab name) or `ranges` (up to 50, read in one request), plus `formatted` to return displayed values rather than raw |
| **sheets_append_row** | Append rows to a tab of one or more spreadsheets. Single form: `spreadsheet_id` + `tab` + `rows`. Batch form: `targets` — an array of `{spreadsheet_id, tab, rows}`. Each row is an array of cell values in column order. `raw` writes values literally |

Every tool refuses a target outside what has been shared with the service
account, and every file ID should come from `drive_find_file` rather than being
guessed or carried in from elsewhere.

### Example: one batched write

```json
{
  "targets": [
    {"spreadsheet_id": "1AbC…", "tab": "Renewals", "rows": [["Acme", "2026-03-01", "120000"], ["Globex", "2026-04-15", "90000"]]},
    {"spreadsheet_id": "1XyZ…", "tab": "Q3",       "rows": [["Initech", "2026-05-02", "45000"]]}
  ]
}
```

## Example Slack Commands

```
/pulse what Google Drive folders do you have access to
/pulse find the "Q3 Renewals" spreadsheet and show me its tabs
/pulse read the churn-signals.csv file in the shared folder and summarize it
/pulse read the "FY26 Renewal Playbook" doc and tell me the escalation steps
/pulse append this quarter's at-risk accounts to the Renewals tab of the Q3 Renewals sheet
/pulse for each account with a renewal in the next 60 days, add a row to its account-plan sheet
```

A scheduled pulse workflow can call these tools directly inside its tool-loop —
e.g. read a CSV of usage data from the folder, pull renewals from Salesforce,
then write one batch of rows per account sheet in a single tick.

## Limitations

- **Drive is read-only; only Sheets values are writable.** The connector can list
  folders, read any file, and append rows to a spreadsheet tab. It cannot create,
  rename, move, delete or re-share a file, create a spreadsheet or a tab, edit an
  existing row in place, or change formatting. The write scope is `spreadsheets`.
- **No text extraction from binary formats.** PDFs, images, `.xlsx`/`.docx` and
  archives return a type-and-size note, not content. Converting the file to a
  Google Doc or Sheet in Drive (Open with → Google Docs/Sheets) makes it readable.
- **The tab must already exist.** An append to a missing tab fails with a parse
  error from Sheets. Discover the exact name with `sheets_get_spreadsheet_info`.
- **Scope is whatever was shared.** With no pins, anything shared with the service
  account is reachable — which is the point, but it means the Drive share is the
  only thing standing between the agent and a folder. Pin folder IDs when that is
  not tight enough.
- **Discovery lags a share by up to 5 minutes.** A newly shared folder appears
  within the discovery cache TTL; there is no way to force it sooner short of a
  restart.
- **6 levels of nesting.** Subfolders are searched and scope-checked to a depth of
  6, and at most 50 shared folders/drives are adopted. A file buried deeper reads
  as out of scope.
- **Read windows are bounded.** One `drive_read_file` returns at most 1 MiB
  (200 KB by default); larger files are paged with `offset_bytes`.
- **Rate limits are per-project.** The client-side budget covers this process. If
  other systems share the same GCP project's Sheets quota, the effective ceiling
  is lower than 300 reads/minute.

## Follow-on

The same client and service account can serve **Google Calendar** at no
additional connector cost — the auth, discovery, scoping and retry machinery in
[`google/client.go`](../google/client.go) and [`google/drive.go`](../google/drive.go)
is agnostic to which Google API it talks to. Adding Calendar means a new scope, a `google/calendar.go`, and its
tools; nothing about the credential or the folder model has to change.

## Troubleshooting

- **Tool returns "Google Drive is not configured for this agent"** — the agent is
  not `pulse`, or `google-credentials-json` is empty. Check the pod env vars and
  `restrictedIntegrations`.
- **Tool returns "not connected, may still be initializing"** — the first token
  exchange has not succeeded yet. Check the logs for `[google] token retry
  failed`; the usual causes are a mangled private key (see base64 above) or the
  Sheets/Drive APIs not being enabled on the project.
- **"Nothing is shared with arbetern-…"** — the connector authenticated fine but
  no folder has been shared with it. Share one with the service-account address.
  Sharing the *file* alone also works, but sharing a folder is what makes
  `drive_find_file` useful.
- **`drive_list_folders` shows fewer folders than you shared** — the share went to
  a different address (check for a typo in the service-account email), or it was
  made under 5 minutes ago and discovery has not refreshed. A shared *drive* also
  requires the account to be added as a member, not just given a link.
- **The agent reports "a defect in arbetern… the Google connector issued a
  request Google rejected"** — a connector bug, not a data or permissions
  problem. The real message is in the pod log (`google error while …`); grep for
  it there rather than asking the agent for detail it deliberately does not have.
- **HTTP 403 with "has not been used in project … or it is disabled"** (pod log)
  — enable the Google Sheets API and Google Drive API on the project. In the
  channel this surfaces as a generic "access was denied".
- **HTTP 404 on a file you can see in your own browser** — it has not been shared
  with the *service account*. Your own access is irrelevant.
- **`drive_read_file` says a file is binary** — it is a PDF, image, Office
  document or archive. Convert it in Drive to a Google Doc/Sheet, or point the
  agent at a text/CSV equivalent.
- **A Google Doc read fails with "cannot be exported as …"** — the `export_as`
  value is not offered for that type. Omit it and let the tool choose.
- **Appends fail with "Unable to parse range"** — the tab name is wrong. Call
  `sheets_get_spreadsheet_info` and use the exact title.
- **Appends fail with 403 but reads work** — the folder was shared as Viewer.
  Change it to Editor.
