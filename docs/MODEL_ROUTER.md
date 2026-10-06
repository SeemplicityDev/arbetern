# Model Router

Arbetern can let a small local model decide which model each interactive
request starts on, instead of sending every turn to the same hosted model. The
router is [Qwen3-4B-Instruct-2507](https://huggingface.co/Qwen/Qwen3-4B-Instruct-2507)
served by [llama.cpp](https://github.com/ggml-org/llama.cpp) as a **sidecar
container** in the arbetern pod. It reads the request, answers with one word,
and arbetern starts the tool loop on the matching model:

| Tier | Starts on | Typical request |
|---|---|---|
| `thanks` | `LIGHT_MODEL`, or no model call at all (see [Thread acknowledgements](#thread-acknowledgements)) | "thanks, that's exactly it" |
| `light` | `LIGHT_MODEL` | "what's the status of PROJ-142?", a greeting |
| `general` | `GENERAL_MODEL` | "summarize yesterday's alerts for the payments service", "yes, go ahead" |
| `code` | `CODE_MODEL` | "fix the failing test in billing-service and open a PR" |
| `heavy` | `HEAVY_MODEL` | "do a root cause analysis of last night's outage using the incident channel, Datadog logs and the deploy history" |

An unset `LIGHT_MODEL` or `HEAVY_MODEL` falls back to `GENERAL_MODEL`, so the
router can be enabled one tier at a time.

The sidecar never calls a hosted model and needs no credentials. Routing
**fails open**: while the sidecar is downloading its model, slow or down, turns
are routed by the keyword heuristics (`isCodeIntent`) exactly as without it.

The same tiers can instead come from TypeSafe's hosted decision model, Jev,
with no sidecar; see [Hosted alternative: TypeSafe Jev](#hosted-alternative-typesafe-jev).

## What is routed

| Entry point | Routed | Notes |
|---|---|---|
| Slack slash command | yes | |
| Slack thread reply | yes | `light` is raised to `general` (see below); a reply that only thanks the bot may be dropped |
| Console chat | yes | turns with history raise `light` to `general`; a thank-you is answered on `LIGHT_MODEL`, because the chat waits for a reply |
| Workflow tick, dashboard render | no | they keep the workflow's `model` override or `CODE_MODEL` |
| Debug analysis (`debug …`, Actions run links) | no | single-shot analysis on `GENERAL_MODEL` |
| Profile summaries, embeddings | no | see [Other jobs considered](#other-jobs-considered) |

Three rules sit on top of the router's answer:

- **A follow-up never starts on `LIGHT_MODEL`**, unless it is nothing but a
  thank-you. "yes, do it" reads as trivial on its own but can set off the
  heaviest work in the conversation, and the router only sees the new message,
  so follow-ups classified `light` start on `GENERAL_MODEL`. This also keeps a
  conversation on one model's prompt cache: each model caches separately, so
  hopping between models mid-thread pays a cache write on each hop.
- **The code switch still applies.** A turn that starts on the light or general
  model moves to `CODE_MODEL` on its first code tool call (`modify_file`,
  `search_code`, …), as before.
- **A heavy turn stays heavy.** With `HEAVY_MODEL` set, the turn is never moved
  to `CODE_MODEL`.

When `GENERAL_MODEL`, `CODE_MODEL`, `LIGHT_MODEL` and `HEAVY_MODEL` all resolve
to the same model there is nothing to choose between, so the router is only
asked about possible thread acknowledgements.

## Thread acknowledgements

In an active Slack thread session every human reply used to start a full tool
loop, so "thanks!" cost the system prompt, every tool schema and a "You're
welcome!" that added noise to the thread. With the router enabled, a thread
reply ends without a model turn only when **all** of these hold:

1. **Every word is gratitude, a goodbye or filler** from a fixed list
   (`thanks`, `ty`, `cheers`, `appreciate`, `bye`, `תודה`, `merci`, … plus words
   such as `so`, `much`, `great`, `perfect`), with at least one gratitude
   word. Approvals (`yes`, `ok`, `sure`, `go`, `lgtm`) are not on the list and
   neither are 👍, ✅, 👌 or 🚀. Anything with a `?`, a link, a mention or code,
   or over 120 characters, fails.
2. **The router agrees** that the message is `thanks`. If the router is down,
   the reply is answered as before.
3. **The bot did not just ask a question.** If the bot's messages since the
   previous human reply contain a `?` outside links and code, the thank-you may
   be a yes ("Want me to open a PR?" → "great, thanks"), so it is answered.

The word list does the deciding and the model can only veto. That is
deliberate: on held-out messages the model labels "yes, thanks", "sure thing,
thanks" and "👍 thanks" as `thanks`, which would have silently dropped
approvals (see [Evaluation](#evaluation)). Those messages start on
`GENERAL_MODEL` instead.

The session stays open, so the next real request in the thread is answered as
usual. Set `modelRouter.skipAcknowledgements: false`
(`MODEL_ROUTER_SKIP_ACKS=false`) to answer every thank-you; they then start on
`LIGHT_MODEL`.

## How it works

```
 Slack / chat turn
     │  1. POST /v1/chat/completions   (MODEL_ROUTER_URL, 127.0.0.1 inside the pod)
     │     routing prompt + the first 1000 bytes of the request
     ▼
 model-router sidecar (llama.cpp + Qwen3-4B, CPU)
     │  2. {"tier": "light"}   (JSON schema constrained, temperature 0, ≤ 16 tokens)
     ▼
 arbetern
     │  3. tool loop on the tier's model; code tools can still move it to CODE_MODEL
     ▼
 model provider  (Anthropic API · Bedrock · Azure OpenAI · Azure Foundry · GitHub Models)
```

- The classification starts with the turn and runs while arbetern gathers the
  channel history, the user's memory and any CI logs, so most of its latency is
  hidden. A possible thread acknowledgement is decided before the memory lookup,
  so dropping it costs no embedding call either.
- The routing prompt is identical on every call, so llama.cpp keeps it in each
  slot's KV cache and only the request itself is processed. A cold slot has to
  read the whole prompt first, which takes seconds on CPU, longer than a turn
  waits. arbetern therefore **warms every slot at startup**
  (`MODEL_ROUTER_SLOTS`, set from `modelRouter.parallel`), retrying until the
  sidecar has downloaded and loaded the model. If the sidecar restarts later,
  the first classification times out but still fills the slot, and the router
  is back after the breaker's 30s cooldown.
- The reply is constrained with `response_format: {type: "json_schema", …}`
  (an enum of the five tiers). The client is plain OpenAI chat completions, so
  `MODEL_ROUTER_URL` can also point at Ollama, vLLM or LM Studio, or, with
  `MODEL_ROUTER_API=systemone`, at TypeSafe Jev.
- The router never retries a turn's classification. A failing call is guarded
  by the same breaker as Headroom and the embeddings backend
  ([llm/breaker.go](../llm/breaker.go)): one timeout takes it out of the path, a
  couple of cheap failures (refused connection, `503 Loading model`) are
  tolerated first, and it is probed again after 30s, doubling to 10 minutes. Its
  state is on the **Performance** page under *Optional services*, and its
  latency and failures under *Model calls* in the row named after
  `modelRouter.model.alias`.

The router is a cost and latency optimisation, never a security boundary: a
user who writes "this is a heavy request" may get the heavy model, which they
could get anyway by asking a hard question.

## Configuration

### App side

| Environment Variable | Default | Description |
|---|---|---|
| `MODEL_ROUTER_URL` | unset | Base URL of the routing server. Empty disables routing. Set by Helm when `modelRouter.enabled: true`. |
| `MODEL_ROUTER_API` | `openai` | `openai` for the sidecar and any chat-completions server (Ollama, vLLM, LM Studio); `systemone` for TypeSafe Jev and servers compatible with its `/v1/systemone` API. |
| `MODEL_ROUTER_API_KEY` | unset | Bearer key for a hosted router. Chart secret `model-router-api-key`. |
| `MODEL_ROUTER_MODEL` | unset | `model` field sent with each request. llama.cpp ignores it; Ollama and vLLM need it. Set by Helm from `modelRouter.model.alias`. |
| `MODEL_ROUTER_TIMEOUT` | `5s` | Deadline of one classification. A turn waits at most this long before falling back to keyword routing. |
| `MODEL_ROUTER_SLOTS` | `2` | Server slots warmed at startup. Set by Helm from `modelRouter.parallel`. |
| `MODEL_ROUTER_SKIP_ACKS` | `true` | Drop Slack thread replies that only thank the bot, under the rules above. |
| `LIGHT_MODEL` | `GENERAL_MODEL` | Model for the `light` tier: a [label](../README.md#model-labels) or the active backend's model ID. Validated at startup like `CODE_MODEL`. |
| `HEAVY_MODEL` | `GENERAL_MODEL` | Model for the `heavy` tier. Validated at startup like `CODE_MODEL`. |

`LIGHT_MODEL` and `HEAVY_MODEL` do nothing without `MODEL_ROUTER_URL`; startup
logs a warning when they are set alone. A label resolves to the active
backend's ID; on AWS Bedrock that is the global inference profile, so pin a
regional one such as `eu.anthropic.claude-opus-5-5` by its full ID (check the
model card's *Programmatic Access* table for the profiles your region offers):

```yaml
env:
  LIGHT_MODEL: "haiku"   # claude-haiku-4-5 on the Anthropic API
  HEAVY_MODEL: "opus"    # claude-opus-5-5 on the Anthropic API
```

The console's **Integrations** page lists the light and heavy models next to
the general and code models, and **Usage & Billing** splits spend per model, so
the effect of routing shows up as cost moving between rows.

### Sidecar (Helm)

```yaml
modelRouter:
  enabled: true
```

| Value | Default | Description |
|---|---|---|
| `modelRouter.enabled` | `false` | Run the sidecar and inject the `MODEL_ROUTER_*` variables into the app container |
| `modelRouter.image.repository` | `ghcr.io/ggml-org/llama.cpp` | llama.cpp server image (amd64 and arm64) |
| `modelRouter.image.tag` | `server-b11312` | The build the arguments were verified against. llama-server renames and drops flags between builds (this one rejects `--no-mmap`), so upgrade deliberately |
| `modelRouter.port` | `8788` | Port the sidecar binds on `127.0.0.1`; nothing outside the pod can reach it |
| `modelRouter.model.docker.repository` | `ai/qwen3` | Docker Hub repository of the model artifact |
| `modelRouter.model.docker.digest` | `sha256:3605803b…` | Digest of the artifact's weights layer, fetched and checked at container start. The default is `ai/qwen3:4b-instruct-2507-q4_K_M`, the evaluated file. Empty falls through to `hfRepo` |
| `modelRouter.model.hfRepo` | `unsloth/Qwen3-4B-Instruct-2507-GGUF:Q4_K_M` | Hugging Face `<repo>[:<quant>]` that llama-server downloads itself, used only without a docker digest; the same file, but resolved by name rather than pinned |
| `modelRouter.model.path` | `""` | Path of a GGUF baked into a custom image; wins over both and skips the download |
| `modelRouter.model.alias` | `qwen3-4b` | Name the model is served and reported under |
| `modelRouter.contextSize` | `4096` | Total KV cache across slots. Keep it set: llama-server otherwise sizes it to the model's 256k native context, about 36 GiB |
| `modelRouter.parallel` | `2` | Classifications served at once; also the number of slots the app warms |
| `modelRouter.threads` | `2` | CPU threads; match `resources.limits.cpu` |
| `modelRouter.timeout` | `""` | `MODEL_ROUTER_TIMEOUT`; empty uses the app default |
| `modelRouter.skipAcknowledgements` | `true` | `MODEL_ROUTER_SKIP_ACKS` |
| `modelRouter.cacheSizeLimit` | `4Gi` | Size limit of the `emptyDir` the model is downloaded into |
| `modelRouter.extraArgs` | `[]` | Extra `llama-server` arguments |
| `modelRouter.env` | `{}` | Extra environment variables for the sidecar |
| `modelRouter.resources` | 2 CPU; memory requests 4Gi, limits 5Gi | Sidecar resources |
| `modelRouter.livenessProbe` / `readinessProbe` | `{}` | Off; see below |
| `modelRouter.securityContext` | non-root (uid 65532) | Must satisfy the pod-level `runAsNonRoot` policy |

The sidecar runs `llama-server --host 127.0.0.1 --port <port> --model <gguf>
--alias <alias> --ctx-size <n> --parallel <n> --threads <n> --jinja
--load-mode none --cache-ram 0`, with the model in an `emptyDir` at `/models`.

**Where the model comes from.** Docker publishes Qwen3 on Docker Hub as
`ai/qwen3` model artifacts (CNCF ModelPack), and the `4b-instruct-2507-q4_K_M`
tag holds the very file the router was evaluated with: its weights layer has
the same sha256 as the Hugging Face quant. An artifact like that is not a
runnable image, and two shortcuts do not work with it:

- A Kubernetes image volume would mount it empty: containerd only unpacks tar
  layers, and this layer is the raw GGUF
  ([containerd#11381](https://github.com/containerd/containerd/issues/11381)).
- llama-server's own `--docker-repo` only recognises layers whose media type
  mentions `gguf`, and these tags use `application/vnd.cncf.model.weight.v1.raw`.

So the container's entrypoint fetches the layer by digest from Docker Hub
(`auth.docker.io` for an anonymous pull token, `registry-1.docker.io` and its
CDN for the blob), checks its sha256, and then starts llama-server. A file
already in the `emptyDir` is reused across container restarts. Any failure
exits non-zero, so Kubernetes retries with backoff while turns use keyword
routing. One fetch per pod start stays far below Docker Hub's anonymous limit
of 100 pulls per IP every 6 hours.

The last two llama-server arguments matter for memory:

- `--load-mode none` reads the weights instead of memory-mapping them. llama.cpp
  repacks the weights for the CPU into a second buffer, so with the default
  mmap the model is held twice: about 2.3 GiB mapped plus 2.3 GiB repacked.
- `--cache-ram 0` turns off llama-server's host prompt cache, which defaults to
  8 GiB and would eventually push the sidecar past any reasonable limit. The
  routing prompt lives in each slot's KV cache, which is all the router needs.

**No probes by default**, for the same reasons as
[Headroom](HEADROOM.md#probe-tuning): a readiness probe on any container
gates the whole pod, and a new pod downloads 2.5 GB before the model is
ready. A missing router costs model choice, never correctness. llama-server
answers `GET /health` with `503` while loading and `200` when ready, so a
`startupProbe` with a generous `failureThreshold` is the safe place to start if
you want one.

## Sizing and latency

- **Memory.** Resident memory with the chart's arguments is about 3.3 GiB:
  2.3 GiB of repacked weights, 0.3 GiB of other tensors, 0.6 GiB of KV cache
  for `contextSize: 4096` and a small compute buffer. The 5Gi limit leaves room
  for the page cache of the model file while it loads.
- **CPU.** Inference is CPU-bound and latency scales with cores; see
  [Evaluation](#evaluation) for measured numbers. Read the p95 of the router's
  row under *Model calls* on the Performance page, and raise `resources` and
  `threads` together if it nears `MODEL_ROUTER_TIMEOUT`.
- **Per replica.** The router is a sidecar, so every arbetern replica runs its
  own copy (the chart default of two replicas runs two).
- **Startup.** Each new pod downloads the model from Docker Hub into its
  `emptyDir`, so it needs egress to `auth.docker.io`, `registry-1.docker.io`
  and Docker's CDN; until the download finishes, turns use keyword routing. To
  skip the download altogether, build an image
  `FROM ghcr.io/ggml-org/llama.cpp:server-b11312` that copies the GGUF in, push
  it next to the arbetern image, and set `modelRouter.image` and
  `modelRouter.model.path`.

## Evaluation

The routing prompt was tuned and checked against 85 labelled messages in the
style of the agents' traffic: lookups, drafts, code changes, investigations,
thank-yous in English, Hebrew and German, and approvals and mixed messages
("thanks, now do the same for the eu cluster"). 25 of them were held out and
only run after tuning stopped. Results with the chart's model, quantization and
arguments, at temperature 0:

| | Tuning set (60) | Held out (25) |
|---|---|---|
| Tier in the acceptable set | 59 | 20 |
| Thank-yous recognised | 14 / 14 | 7 / 7 |
| Approvals or requests labelled `thanks` | 0 | 4 |
| Requests dropped end to end | 0 | 0 |

All 4 held-out mislabels are an approval or request next to a thank-you; the
word list rejects every one of them, which is why it, and not the model,
decides drops. The other misses route a one-system question or a flaky-test
hunt to `heavy`, which costs more but loses nothing.

Latency on two CPU threads of an Apple M4 Pro, with weight repacking: p50
0.6s, p90 0.8s, max 1.4s per classification once a slot is warm, and 8s for a
cold slot. Server vCPUs are usually slower per core, so expect higher numbers
in a cluster; the warm-up keeps the cold cost off real turns.

## Hosted alternative: TypeSafe Jev

[Jev](https://docs.typesafe.ai/introduction) is a hosted decision model: it
answers a typed question about a piece of text with one option from a list, a
probability for every option and a confidence. That is the router's job
exactly, so arbetern can ask it instead of the sidecar:

```yaml
modelRouter:
  enabled: false                        # no sidecar
env:
  MODEL_ROUTER_URL: "https://api.typesafe.ai"
  MODEL_ROUTER_API: "systemone"
  MODEL_ROUTER_MODEL: "jev-1.13.0"      # pin: an alias moves when TypeSafe ships, and the thresholds below were set for this version
secretValues:
  model-router-api-key: "<TypeSafe API key>"
```

Each classification is one `POST /v1/systemone` whose `state` is
`{"message": <first 1000 bytes of the request>}` and whose one Choice question
lists the five tiers with the same descriptions the sidecar prompt uses. The
answer's confidence gates it, following
[TypeSafe's guidance](https://docs.typesafe.ai/primitives/choice): below 0.5
the turn starts on `GENERAL_MODEL`, and `thanks` only counts toward dropping a
thread reply from 0.76 (the word list and the bot-question check still apply).
There are no slots to warm, and the breaker, fallback and Performance-page row
work as they do for the sidecar.

| | Sidecar (Qwen3-4B) | TypeSafe Jev |
|---|---|---|
| Where the message goes | Nowhere: the pod's loopback | TypeSafe's API. The docs do not say where requests are processed; the [DPA](https://typesafe.ai/legal/data-processing) uses EU SCCs and keeps data "as long as necessary", and zero data retention is an enterprise plan |
| Cost | 2 CPU and 4–5 GiB per replica, plus a 2.5 GB download per pod start | $0.042 per million input tokens, output free: about $0.00002 per classification |
| Latency | Measured 0.6s p50 warm on two M4 cores, 8s cold (hidden by the warm-up) | 127 ms p50 and 231 ms p95 from [LiteLLM's benchmark](https://docs.litellm.ai/blog/jev-auto-router-benchmark) host; the round trip from your cluster's region comes on top |
| Accuracy on these tiers | Measured above | Not measured here. LiteLLM's 4-tier routing benchmark: 95% against Haiku's 74%, on labels written by the benchmark's author |
| Calibrated confidence | No | Yes, per answer |
| Languages | Multilingual (Hebrew thank-yous recognised) | English first; other languages "handled but not equally well" ([Models](https://docs.typesafe.ai/models)) |
| Availability | Yours to run | Early access; 40 requests/s per account, which "can change without notice" |

To compare it with the sidecar on the same messages before switching, run the
evaluation set against it (local test, needs a key):

```bash
MODEL_ROUTER_EVAL_URL=https://api.typesafe.ai MODEL_ROUTER_EVAL_API=systemone \
MODEL_ROUTER_EVAL_MODEL=jev-1.13.0 MODEL_ROUTER_EVAL_KEY=… \
  go test ./llm/ -run 'TestRouterEval$|TestRouterEvalHoldout' -count=1 -v
```

Open-weight models that speak the same API (Kev, decider-4b) also exist, but
they ship their own servers and mostly want a GPU, which fits a CPU sidecar
worse than llama.cpp does.

## Other jobs considered

The router replaces keyword routing and lets some thank-yous skip the hosted
model. Other places that call a hosted model were considered for the local
model and left on the hosted one:

| Job | Why it stays hosted |
|---|---|
| Profile summaries | Up to 60 questions in, a 400-word profile out: minutes of CPU generation per profile, and it would hold the slots the latency-sensitive classifications need |
| Debug analyses | Channel history plus CI logs is tens of thousands of tokens, and the answer quality is the point |
| Workflow ticks, dashboard renders | Tool-heavy and long; each workflow already names its model explicitly |
| Embeddings | Qwen3-4B is not an embedding model, and changing the semantic memory's model means re-embedding the whole index |
| Chat titles | Already derived from the first message, without a model call |

## Verifying

Startup logs the configuration and, once the sidecar answers, the warm-up:

```
Model router enabled via http://127.0.0.1:8788 (timeout 5s; light: claude-haiku-4-5, heavy: general model; thread acknowledgements skipped: true)
[llm] model router warmed 2 slot(s) in 1m12s
```

Each routed turn logs its decision, and a dropped thank-you says so:

```
[user=U0123456789 channel=C0123456789] model router: light request, starting on claude-haiku-4-5 (routed in 1.4s)
[user=U0123456789 channel=C0123456789] thread reply only thanks the bot; no model turn
```

A router that stops answering is logged once per transition:

```
[llm] Model router unhealthy, skipping it for 30s: no answer within 5s: context deadline exceeded
```

Inside the pod, query the sidecar directly (the app container is distroless;
the llama.cpp image has `curl`):

```bash
kubectl exec deploy/<arbetern-deployment> -c model-router -- curl -s localhost:8788/health

kubectl exec deploy/<arbetern-deployment> -c model-router -- curl -s localhost:8788/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"messages":[{"role":"user","content":"Reply with one word: light or heavy. Message: list my open PRs"}],"max_tokens":4,"temperature":0}'
```

## References

- Qwen3-4B-Instruct-2507: https://huggingface.co/Qwen/Qwen3-4B-Instruct-2507
- Docker Hub model artifacts: https://hub.docker.com/r/ai/qwen3
- GGUF quantizations: https://huggingface.co/unsloth/Qwen3-4B-Instruct-2507-GGUF
- llama.cpp server: https://github.com/ggml-org/llama.cpp/tree/master/tools/server
