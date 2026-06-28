# Headroom Compression Integration

Arbetern can shrink its LLM token usage with
[**Headroom**](https://github.com/headroomlabs-ai/headroom), a local-first
context-compression engine, **without changing any prompts or which model you
use**. When enabled, Headroom runs as a **sidecar container** in the same pod.
Before every LLM call arbetern sends the conversation to the sidecar's
compression-only endpoint, gets back a smaller message list, and then calls its
normal model provider with that smaller payload.

Because compression is decoupled from the upstream call, it works for **every
backend** — GitHub Models, Azure OpenAI, and Azure Foundry (Claude) — with no
change to endpoints or auth.

## How it works

```
 arbetern app
     │  1. POST /v1/compress  { model, messages }   (HEADROOM_PROXY_URL, loopback)
     ▼
 headroom sidecar  ──  compresses tool outputs (SmartCrusher / logs / search / …)
     │  2. returns compressed messages  (no LLM call, no provider keys)
     ▼
 arbetern app
     │  3. calls the model provider as normal, with the smaller messages
     ▼
 model provider  (GitHub Models  ·  Azure OpenAI  ·  Azure Foundry / Claude)
```

The app POSTs its OpenAI-format messages to the sidecar's
[`POST /v1/compress`](https://headroom-docs.vercel.app/docs/proxy) endpoint,
which returns the compressed messages plus token accounting **without calling
any LLM**. Arbetern then runs its existing `doChat` / `doResponses` /
`doAnthropic` path unchanged. The biggest savings come from bloated tool outputs
(JSON arrays, logs, search results) in the conversation history; user and system
messages are preserved, and tool-call/result pairs are dropped atomically so the
downstream request stays well-formed.

Compression is wired once in `llm.Client.CompleteWithTools`
([llm/client.go](../llm/client.go)) and **fails open**: if the sidecar is
unreachable or returns an error, the original (uncompressed) messages are sent,
so a compression outage never breaks an LLM call.

## Configuration

### App side

| Environment Variable | Required | Description |
|---|---|---|
| `HEADROOM_PROXY_URL` | no | Base URL of a Headroom compression sidecar (e.g. `http://localhost:8787`). When set, each conversation is compressed via `<HEADROOM_PROXY_URL>/v1/compress` before every LLM call. Empty disables compression. Applies to **all** backends. |

When deployed via Helm with `headroom.enabled: true`, `HEADROOM_PROXY_URL` is
injected automatically (pointing at the loopback sidecar); you do **not** set it
by hand.

On startup the app logs:

```
Headroom compression enabled via http://localhost:8787 (applies to all backends)
```

and, when a request is actually compressed:

```
[llm] headroom compressed 18432->4120 tokens (saved 14312)
```

### Sidecar side (Helm)

Enable the sidecar (optionally tuning it) under the `headroom` key in your
values file:

```yaml
headroom:
  enabled: true
  resources:
    requests: { cpu: 100m, memory: 512Mi }
    limits:   { cpu: "1",  memory: 1Gi }
```

Full set of knobs:

| Value | Default | Description |
|---|---|---|
| `headroom.enabled` | `false` | Run the Headroom sidecar and inject `HEADROOM_PROXY_URL` into the app container |
| `headroom.image.repository` | `ghcr.io/chopratejas/headroom` | Sidecar image |
| `headroom.image.tag` | `latest` | Sidecar image tag |
| `headroom.image.pullPolicy` | `IfNotPresent` | Image pull policy |
| `headroom.port` | `8787` | Loopback port the sidecar binds; the app reaches it at `http://localhost:<port>` |
| `headroom.telemetry` | `false` | When `false`, passes `--no-telemetry` (anonymous telemetry off) |
| `headroom.extraArgs` | `[]` | Extra args appended to `headroom proxy` |
| `headroom.env` | `{}` | Extra environment variables for the sidecar |
| `headroom.resources` | `{}` | Resource requests/limits for the sidecar |
| `headroom.securityContext` | non-root (uid 65532) | Must satisfy the pod-level `runAsNonRoot` policy |

The sidecar runs `headroom proxy --host 0.0.0.0 --port <port>` with `HOME=/tmp`
(so the non-root user has a writable cache directory) and liveness/readiness
probes against `/health`. It is **compression-only** — it never calls the LLM,
so it needs no provider keys or upstream configuration.

## Backend compatibility

Because compression happens before (and separately from) the provider call,
every backend benefits:

| Backend (arbetern) | Wire protocol | Compressed? |
|---|---|---|
| **GitHub Models** (default) | OpenAI Chat Completions | ✅ Yes |
| **Azure OpenAI** (gpt-* deployments) | Azure Chat Completions / Responses | ✅ Yes |
| **Azure Foundry** (claude-* deployments) | Anthropic Messages API | ✅ Yes |

## Trade-offs

- **CCR retrieval is not wired.** Headroom's reversible "retrieve the original
  on demand" tool (`headroom_retrieve`) is only automatic in full-proxy mode.
  With `/v1/compress` the model sees the compressed view and cannot re-expand a
  truncated tool output. For arbetern's tool-heavy workloads (Datadog, Databricks,
  search) the SmartCrusher savings are the main win, so this is usually an
  acceptable trade.
- **One extra loopback round-trip per LLM call** (~tens of ms, bounded to 30s).
  Negligible against the token savings on each provider call, and skipped
  entirely when the sidecar is unreachable (fail-open).
- **Short prompts barely compress.** Headroom passes content under ~200 tokens
  through unchanged, so simple completions see little or no change.

## Verifying

With the sidecar running, check its health and savings from inside the pod (or
via `kubectl exec`):

```bash
# Liveness
curl http://localhost:8787/health

# Cumulative token savings
curl http://localhost:8787/stats
```

You can also confirm compression is firing from the app logs (the
`[llm] headroom compressed …` line above).

## References

- Headroom repository: https://github.com/headroomlabs-ai/headroom
- Headroom proxy / `/v1/compress` docs: https://headroom-docs.vercel.app/docs/proxy
