# Model Service

The model service is Bureau's unified gateway for LLM and inference
access. It is a Bureau service principal running in its own sandbox,
holding API keys for external providers and routing to local inference
engines. Agents access it via service socket (native CBOR) or via an
HTTP compatibility layer (for standard SDK use).

The model service replaces the pattern of putting model API keys in
per-sandbox credential bundles and having each proxy forward directly to
provider APIs. Model API credentials are organizational resources, not
per-agent identity — they belong at the project level, not the sandbox
level.

fundamentals.md defines the service socket mechanism. authorization.md
defines the grant system. telemetry.md defines the telemetry pipeline.
This document defines the model service built on those foundations.

---

## Why a Service

External model access via per-proxy credential injection has structural
problems:

- **Credential scope mismatch**: API keys for OpenAI, Anthropic,
  OpenRouter are organizational resources shared across agents. Putting
  them in per-sandbox credential bundles treats them as per-agent
  identity, which they are not. Key rotation means updating every
  template's credential bundle.

- **No cost visibility**: each proxy forwards model API calls
  independently. There is no central point to track cumulative spend,
  attribute costs to projects, or enforce budgets.

- **No model selection intelligence**: agents must know exact model
  names (`gpt-4.1-2026-02-something`). When models update, templates
  break. Historical data shows 100% failure rate on calls with obsolete
  model names.

- **No batching**: each request goes out individually. Embedding
  workloads, background analysis, and other latency-tolerant tasks
  cannot benefit from batched inference on local hardware.

- **No unified protocol**: agents using OpenAI SDK get one protocol,
  Anthropic SDK gets another. Bureau's own agents, microservices, and
  future local inference (IREE) need a binary-native protocol that
  handles images, audio, video, and embeddings without base64 overhead.

The model service centralizes model access as Bureau infrastructure:
project-scoped accounting, stable model aliases, batching, streaming,
binary-native data handling, and provider-agnostic routing.

---

## Architecture

```
                          Native CBOR API
Agent (sandbox) ──────────────────────────────▶ Model ──▶ OpenRouter
                                                Service
Agent (sandbox) ──bridge──▶ HTTP Compat Layer ──────────▶ llama.cpp
                  TCP→Unix   (path-prefix)              (local)
                                                     ──▶ IREE
Claude Code ───proxy──▶ api.anthropic.com               (future)
               (existing path, unchanged)
```

The model service has two API surfaces on two Unix sockets:

- **Native CBOR** (`/run/bureau/listen/service.sock`): Bureau agents and
  microservices use this directly. Binary data native (raw bytes, not
  base64). Continuations, batching, streaming. Authenticated via service
  token on connection handshake.

- **HTTP compatibility** (`/run/bureau/listen/http.sock`): standard SDK
  access via the bridge. Path-prefix routing: `/openai/v1/...`,
  `/anthropic/v1/...`, `/openrouter/v1/...`. Each path speaks the
  provider's native HTTP protocol. Authenticated via service token in
  the Authorization header (the Bureau token IS the "API key" from the
  SDK's perspective).

Templates declare `required_services: ["model"]` to get the service
socket bind-mounted. The bridge maps a TCP port to the HTTP socket for
SDK-based agents.

Claude Code and other operator tools that manage their own API keys
continue using the proxy's `proxy_services` credential injection. The
model service handles everything else.

---

## Provider Registry

Providers are the backends the model service routes to. Each provider
is registered via a Matrix state event in the service room.

**State event**: `m.bureau.model_provider`, state key = provider name.

```json
{
  "endpoint": "https://openrouter.ai/api",
  "auth_method": "bearer",
  "capabilities": ["chat", "embeddings", "streaming"],
  "batch_support": false
}
```

```json
{
  "endpoint": "unix:///run/bureau/service/llama.sock",
  "auth_method": "none",
  "capabilities": ["chat", "embeddings", "batch"],
  "batch_support": true,
  "max_batch_size": 32
}
```

Provider types:

- **HTTP upstream** (OpenRouter, OpenAI, Anthropic, Google): standard
  REST API with bearer token auth. The model service makes HTTP requests
  to the endpoint, injecting the API key from the project's account.

- **Local service** (llama.cpp, vLLM, IREE): Bureau service principals
  in sandboxes with GPU device access. They expose OpenAI-compatible
  HTTP endpoints on their service sockets. The model service routes to
  them via Unix socket — no internet round-trip, no API key needed.
  Multiple replicas are supported via the service directory (the model
  service load-balances across instances).

- **CBOR service** (future IREE model server): native CBOR protocol for
  multi-model serving with shared memory pools and KV caches. Binary
  data flows without serialization overhead. The model service speaks
  CBOR to these providers, avoiding HTTP+JSON+base64 entirely for the
  internal path.

Day 0 providers: OpenRouter (multi-model cloud access) and local
llama.cpp (embeddings, analysis). IREE and direct provider APIs follow.

---

## Model Registry

Models are identified by stable aliases, not versioned API names. The
model service resolves aliases to (provider, provider_model_name) at
request time.

**State event**: `m.bureau.model_alias`, state key = alias name.

```json
{
  "provider": "openrouter",
  "provider_model": "openai/gpt-4.1",
  "pricing": {"input_per_mtok_microdollars": 2000, "output_per_mtok_microdollars": 8000},
  "capabilities": ["code", "reasoning", "streaming"]
}
```

```json
{
  "provider": "llama-local",
  "provider_model": "nomic-embed-text-v1.5",
  "pricing": {"input_per_mtok_microdollars": 0},
  "capabilities": ["embeddings", "batch"]
}
```

When a model version changes, one state event update propagates to all
agents. No template changes, no credential bundle updates. The alias
is stable; the backing model is not.

`"model": "auto"` selects based on requested capabilities and cost. The
selection policy is configurable per project.

---

## Per-Project Accounting

Cost attribution is per-project, not per-fleet. A fleet may host
multiple operators and projects; a single billing pool across all of
them is insufficient.

### Accounts

An account binds a provider API key to a set of projects. The model
service selects the most specific matching account for each request.

**State event**: `m.bureau.model_account`, state key = account name.

```json
{
  "provider": "openrouter",
  "credential_ref": "openrouter-alice",
  "projects": ["iree-amdgpu", "iree-compiler"],
  "priority": 0,
  "quota": {
    "daily_microdollars": 50000000,
    "monthly_microdollars": 500000000
  }
}
```

```json
{
  "provider": "openrouter",
  "credential_ref": "openrouter-shared",
  "projects": ["*"],
  "priority": -1,
  "quota": {
    "daily_microdollars": 10000000
  }
}
```

`credential_ref` names reference entries in the model service's sealed
credential bundle. The actual API keys are delivered by the launcher
via the standard credential pipeline — they never appear in Matrix
state events.

Account selection for a (project, provider) pair: explicit project
match wins over wildcard, higher priority wins ties. This lets
operators bring their own API keys for their projects while having a
shared fallback for everything else.

### Project Identity

The requesting agent's project is carried in the service token. The
daemon mints a service token for each agent that includes the project
derived from the agent's workspace assignment:

```json
{
  "subject": "@iree/amdgpu/agent/code-reviewer:bureau.local",
  "machine": "@iree/fleet/prod/machine/gpu-box:bureau.local",
  "audience": "model",
  "grants": [{"actions": ["model/*"]}],
  "project": "iree-amdgpu",
  "issued_at": 1740000000,
  "expires_at": 1740000300
}
```

For the native CBOR API, the token is sent in the connection handshake
(first message). For the HTTP compatibility layer, the token is the
"API key" — agents read `/run/bureau/service/token/model.token` and
the SDK sends it in the Authorization header:

```bash
export OPENAI_API_KEY=$(cat /run/bureau/service/token/model.token)
```

The model service recognizes Bureau service tokens, verifies the
signature, and extracts project identity for account selection and
cost attribution.

### Cost Attribution

Every model API call produces a telemetry event:

```json
{
  "project": "iree-amdgpu",
  "agent": "@iree/amdgpu/agent/code-reviewer:bureau.local",
  "model_alias": "codex",
  "provider": "openrouter",
  "provider_model": "openai/gpt-4.1",
  "account": "alice/openrouter",
  "input_tokens": 12000,
  "output_tokens": 3500,
  "cost_microdollars": 52000,
  "latency_ms": 2340,
  "batch_size": 1
}
```

These feed into the telemetry pipeline as spans, enabling per-project,
per-agent, and per-model cost dashboards. Quota enforcement reads
cumulative spend from these events.

---

## Native CBOR API

The native API is CBOR over the service socket. Messages carry binary
data (images, audio, video, embeddings) as raw bytes — no base64
encoding. This is the primary API for Bureau's own agents and
microservices.

### Completion

```cbor
{
  "action": "complete",
  "model": "codex",
  "messages": [
    {"role": "system", "content": "You are a code reviewer."},
    {"role": "user", "content": "Review this function.", "attachments": [
      {"type": "image/png", "data": h'89504e47...'}
    ]}
  ],
  "stream": true,
  "latency_policy": "immediate"
}
```

Response (streamed):
```cbor
{"type": "delta", "content": "The function has..."}
{"type": "delta", "content": " a potential race condition..."}
{"type": "done", "model": "openai/gpt-4.1", "usage": {"input": 4200, "output": 890}, "cost_microdollars": 15200}
```

### Embed

```cbor
{
  "action": "embed",
  "model": "local-embed",
  "input": [h'raw text bytes', h'more text bytes'],
  "latency_policy": "batch"
}
```

Response:
```cbor
{
  "embeddings": [h'float32 vector bytes', h'float32 vector bytes'],
  "model": "nomic-embed-text-v1.5",
  "usage": {"input": 512},
  "cost_microdollars": 0
}
```

### Continue (continuation threading)

```cbor
{
  "action": "complete",
  "model": "codex",
  "continuation_id": "abc-123",
  "messages": [
    {"role": "user", "content": "Now check for memory leaks too."}
  ]
}
```

The model service maintains conversation state keyed by (agent,
continuation_id). Previous messages are automatically prepended. The
continuation is server-side state — the agent does not resend history.
Continuations expire after a configurable TTL (default: 1 hour).

---

## Batching

Requests include a latency policy that controls batching behavior:

- **immediate**: forward now, return as fast as possible. Default. The
  only mode available on the HTTP compatibility layer.

- **batch**: accumulate up to N requests or T milliseconds (whichever
  comes first), then send as a single batch to the provider. Suited
  for embeddings workloads, background analysis, and other
  latency-tolerant tasks.

- **background**: lowest priority. Use idle capacity, yield to
  immediate and batch requests. Suited for large analysis jobs.

```
CBOR requests ─▶ Latency Router ─▶ Immediate Queue ─▶ Provider
                                 ─▶ Batch Accumulator ─▶ Provider (batched)
                                 ─▶ Background Queue ─▶ Provider (when idle)
```

For local inference (llama.cpp, IREE), batching is where throughput
gains are largest — the inference engine processes multiple sequences
simultaneously. A single embedding request uses a fraction of the
GPU's capacity; 32 batched requests saturate it.

Batching is transparent to the caller. The response arrives when the
provider completes the batch and the caller's result is extracted.

---

## Streaming

The service socket protocol supports bidirectional streaming. This is
required for:

- **Model output streaming**: token-by-token response delivery (SSE
  equivalent over CBOR). The native API streams delta messages as they
  arrive from the provider.

- **Audio/media streaming**: STT input (audio chunks flowing in) and
  TTS output (audio chunks flowing out) are continuous bidirectional
  streams. Binary data in CBOR avoids the base64 overhead that makes
  HTTP-based media streaming wasteful.

- **Large input streaming**: sending large documents or codebases to
  the model without buffering the entire input in memory.

The framing protocol uses explicit stream-open and stream-close markers
on a single connection, allowing either side to send multiple messages.
This extends the current request→response pattern in lib/service
socket infrastructure.

---

## HTTP Compatibility Layer

The HTTP compatibility layer presents provider-native HTTP APIs on a
second Unix socket. The bridge converts TCP↔Unix so agents can use
standard SDK base URLs.

Path-prefix routing:
- `/openai/v1/chat/completions` → OpenAI-compatible (routed to
  configured provider)
- `/anthropic/v1/messages` → Anthropic-compatible (routed to
  Anthropic or compatible provider)

Each path prefix speaks the provider's native HTTP protocol. The model
service is a transparent proxy per-prefix: it injects credentials,
tracks costs, and enforces quota, but does not translate between
provider schemas.

SSE streaming passes through transparently.

Authentication: the agent's service token is sent as the API key. The
SDK includes it in the Authorization header. The model service
validates the token, extracts project identity, and proceeds.

```
Agent SDK ─▶ 127.0.0.1:8642 ─▶ Bridge ─▶ /run/bureau/service/model-http.sock
                                         ─▶ Model Service ─▶ Provider API
```

Template configuration:
```json
{
  "required_services": ["model"],
  "environment_variables": {
    "OPENAI_BASE_URL": "http://127.0.0.1:${MODEL_HTTP_PORT}/openai/v1"
  }
}
```

The agent's entrypoint reads the service token:
```bash
export OPENAI_API_KEY=$(cat /run/bureau/service/token/model.token)
```

---

## Relationship to the Proxy

The proxy keeps its role for per-sandbox identity concerns:

- **Matrix token injection**: each agent has its own Matrix account.
  The proxy holds the agent's token and injects it into Matrix API
  calls. This is inherently per-sandbox.

- **Non-model external APIs**: GitHub API keys, Jira tokens, and other
  external service credentials that are per-agent or per-template
  remain in `proxy_services`.

- **Observation forwarding**: unchanged.

- **Service routing**: unchanged. The model service itself is
  discovered via the service directory and routed like any other
  Bureau service.

Model API credential injection moves from the proxy to the model
service. Templates that previously declared `proxy_services` entries
for OpenAI/Anthropic/etc. instead declare `required_services: ["model"]`
and use the service token for authentication.

The proxy and model service serve different credential scopes:

| | Proxy | Model Service |
|---|---|---|
| Credential scope | Per-sandbox (agent identity) | Per-project (organizational resource) |
| What it handles | Matrix, non-model APIs | Model/inference APIs |
| Cost tracking | Not its concern | Core responsibility |
| Quota enforcement | Non-model rate limits | Model API budgets |

---

## Local Inference Providers

Local inference engines (llama.cpp, vLLM, IREE) run as Bureau service
principals in their own sandboxes. Bureau chose bwrap specifically
because it can expose hardware devices (Vulkan, HIP, CUDA) to
sandboxed processes.

Each local inference principal:

- Runs in a sandbox with GPU device access via the template's device
  bind mounts
- Exposes an OpenAI-compatible HTTP endpoint on its service socket
  (llama.cpp and vLLM both support this natively)
- Registers in `#bureau/service` with capabilities (chat, embeddings,
  batch, etc.)
- Can be replicated: multiple instances on different machines, the
  model service load-balances via the service directory

The model service routes to local providers via Unix socket (same
machine) or via the transport relay (different machine). No internet
round-trip, no API key — the service token authentication between
the model service and the local provider is Bureau-internal.

Future IREE model server: a single sandbox hosting multiple models
with shared memory pools and KV caches. The model service routes to
it via native CBOR (not HTTP), and binary data (images, audio) flows
without serialization overhead. This is where the native CBOR API's
binary handling matters most.

---

## Configuration Lifecycle

The model service syncs its configuration from Matrix state events at
startup and via incremental /sync during operation:

- `m.bureau.model_provider` — provider registry (endpoints, auth,
  capabilities)
- `m.bureau.model_alias` — model aliases (provider, model name,
  pricing, capabilities)
- `m.bureau.model_account` — API key accounts (provider, credential
  ref, project scope, quota)

API key credential refs in `m.bureau.model_account` reference entries
in the model service's sealed credential bundle. The launcher delivers
the actual keys via the standard credential pipeline. When an operator
adds a new account, they:

1. Add the API key to the model service's credential store (sealed)
2. Publish the `m.bureau.model_account` state event referencing the
   credential name
3. The model service picks up the new account via /sync

Key rotation: update the sealed credential, restart the model service
(or implement hot-reload of the credential bundle). The Matrix state
event does not change — only the underlying credential material.

---

## Quota Enforcement

Before forwarding a request, the model service checks the project's
cumulative spend against its account quota. If the quota is exhausted,
the request is rejected with a clear error (HTTP 429 for the
compatibility layer, CBOR error response for the native API).

Quota state is maintained in memory from telemetry events, with
periodic persistence to avoid losing tracking state across restarts.
The quota window (daily, monthly) is configured per account.

Quota enforcement is best-effort for concurrent requests: two requests
arriving simultaneously may both pass the check and both execute, with
the overage detected on the next check. This is acceptable — the
alternative (serializing all requests through a lock) would destroy
throughput.
