# codeRoute

OpenAI-compatible API gateway that routes requests across multiple LLM
providers, with failover, BYOK key storage, smart model selection, semantic
caching, and an IoT bridge. Self-hosted, Go, backed by PostgreSQL.

## Running

```bash
OPENAI_API_KEY=sk-... ANTHROPIC_API_KEY=sk-ant-... docker compose up --build
```

The gateway prints a client key once on first start; use it as the API key in
your editor and point the base URL at `http://localhost:8080/v1`.

Set `ADMIN_EMAIL` and `ADMIN_PASSWORD` to create the first operator account,
then sign in at `http://localhost:8080/`. Without an account or an
`ADMIN_TOKEN` the dashboard and management API are unreachable — an
unauthenticated key-minting surface would be worse than having none at all.

> **No limits.** A client key authenticates and nothing more: there is no rate
> limit and no spend cap. A leaked key means uncapped spend against your stored
> provider keys. Revocation is the only control. Treat every `cr_` key as a
> secret and don't expose the gateway more widely than you need.

## Endpoints

### Caller-facing — authenticated with a client key (`cr_…`)

| Endpoint | Purpose |
|---|---|
| `POST /v1/chat/completions` | OpenAI-compatible completions, streaming supported |
| `GET /v1/models` | Models available given the configured provider keys |
| `POST /v1/iot/inference` | Device inference, edge-first with cloud fallback |
| `POST /v1/iot/telemetry` | Device telemetry ingest |
| `GET /v1/iot/telemetry?device_id=` | Recent readings for a device |

A revoked key gets `401`. Nothing else is enforced against a caller.

### Management — signed in, or holding `ADMIN_TOKEN`

| Endpoint | Purpose |
|---|---|
| `GET /v1/admin/keys` | List client keys (never their hashes) |
| `POST /v1/admin/keys` | Mint a client key; the raw key is shown exactly once |
| `DELETE /v1/admin/keys/{id}` | Revoke a key, keeping its usage history |
| `GET /v1/admin/providers` | Which upstreams have a key, by fingerprint |
| `PUT /v1/admin/providers/{name}` | Store or rotate an upstream key, verified before it is saved |
| `DELETE /v1/admin/providers/{name}` | Remove an upstream key |
| `GET /api/stats` | Traffic, spend, cache hit rate over `?window=` |
| `GET /api/usage` | Recent calls with key, model, tokens, cost |
| `GET /api/keys` | Client keys, with when each was created and last used |
| `GET /api/catalogue` | Every model the providers serve; `?free=true`, `?provider=` to narrow |
| `POST /api/discover` | Re-read every provider's model list now |
| `POST /api/probe` | Send a trial completion to each candidate model now |
| `GET /api/probes` | What the last sweep found |
| `GET /api/route?model=` | The chain a request would follow, without sending one |
| `GET /api/active` | Which model is answering now, which answered last, and what the next request would reach for |
| `GET /api/scores?task=` | What this deployment's traffic says about each model |
| `GET` / `PUT /v1/admin/model-tags` | Mark which models are for which kind of work |
| `GET` / `POST /v1/admin/users` | List or create operator accounts |
| `PATCH /v1/admin/users/{id}` | Disable an account, or set its password |
| `POST /api/login` / `logout` | Start or end a dashboard session |
| `GET /api/me` | Who the current session belongs to |
| `POST /api/password` | Change your own password |
| `GET /api/new-models` | Models that have appeared since the window began |
| `GET` / `PUT /api/settings` | Read or change the free-only switch at runtime |
| `POST /api/playground` | Send a prompt through the gateway and see what answered |
| `GET /api/models` | Per-model traffic, paired with the routing catalogue |

### Public

| Endpoint | Purpose |
|---|---|
| `GET /health` | Liveness, provider key status, MQTT and cache status |
| `GET /` | Operator dashboard (prompts for the admin token) |

## Configuration

### Core

| Variable | Default | Purpose |
|---|---|---|
| `DATABASE_URL` | local postgres | Connection string |
| `ENCRYPTION_KEY` | committed default | AES key for stored provider keys — **change this** |
| `ADMIN_EMAIL` / `ADMIN_PASSWORD` | — | Creates the first operator account, if none exists |
| `ADMIN_TOKEN` | — | Shared token for scripts; people sign in instead |
| `TRUST_PROXY_HEADERS` | `false` | Believe `X-Forwarded-*`; only behind a proxy that sets them |
| `OPENAI_API_KEY` / `ANTHROPIC_API_KEY` / `GOOGLE_API_KEY` | — | Optional BYOK bootstrap; keys are otherwise added from the dashboard |

### Routing

| Variable | Default | Purpose |
|---|---|---|
| `ROUTING_MODE` | `auto` | `auto` (route only when the client asks for `auto`), `always`, `off` |
| `ROUTING_OBJECTIVE` | `balanced` | `balanced`, `latency`, or `cost` |
| `PROVIDERS` | all presets | Narrow the provider list, or add one with `name=url` |
| `FREE_ONLY` | `false` | Initial free-only setting; the dashboard button overrides it |
| `DISCOVERY_INTERVAL` | `1h` | How often each provider is asked what it serves |
| `MODEL_CATALOG` / `MODEL_CATALOG_PATH` | built-ins | Override or extend the curated catalogue |
| `LATENCY_FEEDBACK_INTERVAL` | `5m` | How often observed behaviour is folded back into ranking; `0` disables |
| `OBSERVATION_WINDOW` | `6h` | How far back routing looks when judging a model |
| `ROUTING_ATTEMPTS_PER_PROVIDER` | `2` | Models one request may try at each provider before moving on |
| `ROUTING_MAX_ATTEMPTS` | `8` | Cap on *retries*; every provider is always tried at least once |
| `PROBE_INTERVAL` | `6h` | How often to check which models actually work; `0` disables |
| `PROBE_MODELS_PER_PROVIDER` | `3` | How many models a sweep checks at each provider |
| `PROBE_FRESHNESS` | `24h` | How long a probe result is trusted |

### Semantic cache

| Variable | Default | Purpose |
|---|---|---|
| `CACHE_ENABLED` | `true` | Needs pgvector and an embedding key; otherwise inert |
| `CACHE_THRESHOLD` | `0.95` | Minimum cosine similarity for a stored entry to answer |
| `CACHE_TTL` | `24h` | How long an entry may be served |
| `CACHE_MAX_TEMPERATURE` | `0.3` | Above this, the caller wants varied output and is never served a replay |
| `EMBEDDING_MODEL` / `EMBEDDING_BASE_URL` | OpenAI `text-embedding-3-small` | Point at a local server to keep prompts off the network |

### IoT

| Variable | Default | Purpose |
|---|---|---|
| `MQTT_BROKER` | — | Enables the MQTT bridge |
| `IOT_EDGE_ENDPOINT` | — | OpenAI-compatible local model server, tried before the cloud |
| `IOT_API_KEY` | — | Client key that MQTT-originated usage is attributed to |

See [.env.example](.env.example) for the full list with comments.

## Signing in

The dashboard uses an email and password. Accounts live in Postgres with
bcrypt-hashed passwords; a session is an HttpOnly, SameSite=Lax cookie holding
a random token whose hash alone is stored, valid for 12 hours.

`ADMIN_EMAIL` and `ADMIN_PASSWORD` create the first account, and are read
**only when no account exists** — re-running with them set will not reset a
password you have since changed, nor resurrect an account you disabled. Remove
them from the environment afterwards.

Further accounts are created from the management API. Disabling one ends its
sessions immediately, and the last account that can sign in cannot be disabled
unless an `ADMIN_TOKEN` is set, so you cannot lock yourself out with one click.

`ADMIN_TOKEN` still works and is the right thing for scripts and `curl` —
forcing automation through a login flow tends to end with an email and password
in someone's shell history. Either credential reaches the same endpoints.

Sign-in attempts are throttled per account and per source address: five wrong
answers within fifteen minutes locks that identity out for five. Password
checking is slow by design, which defeats offline guessing, but on its own it
would still let an attacker tie up the server.

A wrong password and an unknown address return the same status and the same
message, so neither reveals who has an account.

## Providers

Seventeen are built in — OpenAI, Anthropic, Google Gemini, OpenRouter, Groq,
SambaNova, Mistral, NVIDIA NIM, Hugging Face, GMI Cloud, xAI, DeepSeek,
Together, Cerebras, xKiro, TeamoRouter and B.AI. All appear in the dashboard;
paste a key to enable one. Every base URL was checked against the live
endpoint.

TeamoRouter is reachable at `api.teamorouter.com` (the preset) and also at
`api.teamorouter.cn`, which its own site advertises. Switch with
`PROVIDERS=+teamorouter=https://api.teamorouter.cn/v1`.

Adding one that isn't listed needs no code, because a provider is data:

```bash
PROVIDERS=+mynewvendor=https://api.example.com/v1
```

Anything OpenAI-compatible works that way. A vendor with its own wire format
still needs a client — see the `Kind` field in `internal/provider/specs.go`.

### Model discovery

Saving a key reads that provider's `/models` list, and it refreshes on
`DISCOVERY_INTERVAL`. Discovery is what lets the gateway resolve a model name
to a provider: several vendors serve the same open-weight models, so the name
alone does not say where to send a request. Pin one explicitly with
`provider:model` — `groq:llama-3.3-70b-versatile`.

The cached list survives restarts, and a provider that fails to answer keeps
its previous list rather than having its models vanish.

### Knowing which models actually work

Discovery tells you what a provider *serves*. It does not tell you what your
account may *use* — entitlements, deposits and daily allowances all differ, and
a list of eight hundred models routinely contains a few dozen you can reach.

Routing used to find that out the hard way, with real requests doing the
discovery and paying for it in latency and failures. Probing does it in
advance: a trial completion of one token, sent on `PROBE_INTERVAL`, to the
models routing would actually reach. Models that answer are tried first;
models that refuse are set aside and the reason recorded.

It is deliberately bounded. Probing every model at every provider would cost
real money and burn the very free allowances it exists to protect, so a sweep
takes the head of each provider's ranked list — `PROBE_MODELS_PER_PROVIDER`,
three by default — plus anything you have marked, since routing is restricted
to those.

**Confirmed working is a preference, not a filter.** A model nobody has probed
is unknown, not broken, so it stays in the chain behind the confirmed ones.
Results expire after `PROBE_FRESHNESS`, because an account that had credit
yesterday may not today.

Changing a provider's key discards its probe results: what an old key could
reach says nothing about a new one.

The dashboard's **Check which work** button runs a sweep on demand, for when
you have just fixed a billing problem and would rather not wait.

### Free models

Where a provider publishes prices, zero-priced models are detected
automatically. Two shapes are understood, because vendors disagree:

| Vendor | Shape | Scale |
|---|---|---|
| OpenRouter | `{"prompt":"0.0000008","completion":"..."}` | per token |
| xKiro | `{"unit":"per_1m_tokens","input":0.75,"output":1.5}` | per million |

Both are read correctly. A pricing block that states neither its fields nor a
unit is treated as unpublished rather than guessed at — misreading the scale
would be wrong by six orders of magnitude. Prices in a currency other than USD
are also treated as unpublished, since they are not comparable with the rest of
the catalogue.

In practice OpenRouter currently lists around twenty models at zero, and xKiro
around forty.

Ask for free-only routing per request by using the model `auto:free`. For the
deployment-wide rule there is a button on the dashboard — it takes effect on
the next request and is written to the database, so a restart does not undo it.
`FREE_ONLY=true` sets the starting value for a fresh deployment; once the
button has been used, the stored value wins.

Turning it on is refused while no free models are known, since it would
otherwise refuse every request and look like a broken gateway.

While it is on, a request naming a priced model is **served by a free one
instead** rather than refused — otherwise every editor with a model name
configured would stop working the moment an operator enabled the guardrail.
The response's `model` field reports what actually ran, which is the standard
way that substitution is signalled. A named model that is already free is used
exactly as asked.

**A model with no published price is never treated as free.** Most providers
don't publish prices through their API, so their models are unpriced rather
than free, and free-only routing skips them. That is deliberate: the
alternative is assuming zero and quietly spending money. Several providers do
offer genuinely free *account tiers* — that's a property of your account, not
of a model, so the gateway flags it in the dashboard but won't route on it.

### A caveat on key verification

Saving a key checks it against the provider. For four presets — OpenRouter,
SambaNova, NVIDIA and Hugging Face — the model list is public, so the check
proves the endpoint is reachable but **not** that the key works. Those show as
`unverified` in the dashboard, and a bad key surfaces on the first completion.

## Provider keys

Upstream keys live encrypted in Postgres, and are managed from the dashboard or
the admin API — no restart, no redeploy:

```bash
curl -X PUT http://localhost:8080/v1/admin/providers/openai \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -d '{"api_key":"sk-..."}'
```

The key is checked against the provider before it is stored, so a typo is
refused at the point of entry rather than by every completion afterwards. Pass
`"skip_verification": true` for an upstream the gateway cannot reach at save
time, or a proxy that does not serve `/models`.

Keys are never readable back — the API returns a short fingerprint, enough to
tell two keys apart or confirm which one is deployed. Setting the environment
variables above still works as a bootstrap for a fresh deployment; they seed
the same table on startup.

Adding an OpenAI key also brings the semantic cache to life, since that is what
pays for embeddings. It takes effect within 30 seconds, without a restart.

## What happens when a model is unavailable

Routing keeps going. A failed attempt is not an error the caller sees — the
gateway moves to the next candidate and only reports failure once every one has
been tried, listing what each said.

The chain interleaves providers and gives each more than one chance:

```
1. openrouter  model-a     ← fails, 404
2. google      model-x     ← fails, not entitled
3. openrouter  model-b     ← answers; the caller sees only this
```

Two models per provider by default, capped at eight attempts overall, both
configurable above. Vendors alternate before one is retried, so a provider that
is entirely down does not consume two slots before another is tried.

Across requests it also learns: a model that refuses on entitlement, credit or
capability is set aside for an hour, so later requests skip straight past it.

**Streaming is the exception.** Once the first chunk has reached the client the
answer is committed — switching providers would splice two different
completions together — so a stream that dies mid-flight surfaces as an error
rather than being silently rescued.

## The playground

The dashboard has a **Playground** card: pick a model or a routing alias, type
a prompt, and see which model actually answered, what it cost, how long it took
and whether the reply came from cache.

It is the quickest way to see routing work — asking `auto:code` and watching a
different model answer than `auto:chat` tells you more than any amount of
configuration reading. Where a request fails it shows the gateway's own
explanation, naming every provider tried and why each declined.

Runs are attributed to no client key, so experimenting does not distort
anyone's usage figures, and replies are capped at 2048 tokens.

## Routing aliases

Name one of these instead of a model and the gateway chooses:

| Model | Asks for |
|---|---|
| `auto` | Best fit for whatever the prompt looks like |
| `auto:code` | A model suited to programming |
| `auto:chat` | A general conversational model |
| `auto:fast` | Lowest latency |
| `auto:cheap` | Lowest cost |
| `auto:free` | Zero-priced models only |

`auto:code` and `auto:chat` state the task outright, which is more reliable
than inferring it from the prompt — a question about a function might read
either way.

## What "intelligent routing" does and does not mean

The gateway learns from its own traffic, on a schedule set by
`LATENCY_FEEDBACK_INTERVAL`, looking back over `OBSERVATION_WINDOW`. What it
measures, per model and per provider:

- **whether calls succeed** — the strongest signal, and the one that keeps a
  model that has started failing from being chosen again
- **how quickly they return**, median, successes only, so a provider that
  fails in 50ms does not look fast

**It does not measure answer quality, and does not claim to.** The gateway sees
requests and responses; it never sees whether an answer was correct, useful or
well written. Any ranking of "best model" here means most reliable and fastest
for the price — nothing more. Real quality ranking would need a feedback signal
that does not exist in this system.

### Marking models yourself

Inference from a model's name is a guess. It cannot know that a particular
general-purpose model is the best coder available on your account, nor that a
model with `code` in its name is one you would rather never use.

The dashboard's **Available Models** table has a **Use for** column with
`coding` and `chat` buttons. Marking a model is a statement, and it is treated
as one:

> **Once any model is marked for a task, automatic routing for that task uses
> only the marked models.**

So marking three models for coding means `auto:code` will use those three and
nothing else, in the order the scores put them. Marking nothing leaves routing
to infer, exactly as before — tagging is opt-in per task, and marking models
for coding does not restrict chat.

Naming a model directly still works whether it is marked or not: a mark
governs what the router chooses, not what a caller may ask for.

If every marked model becomes unusable the request fails rather than quietly
falling back, and the error says the marks are why — a stale mark list is a
likely cause and an easy fix.

Two further inputs are inference rather than measurement:

- **Published prices**, where a provider states them. An unstated price is
  treated as mid-range, never as zero.
- **Task fitness**, from an operator's marks where they exist, then a model's
  declared tasks, then its name: `deepseek-coder` is preferred for code and
  demoted for chat, a `-base` or `fim` model is never preferred for either.
  Inference only orders candidates; only a mark excludes one.

A small sample is discounted rather than trusted, so one unlucky call does not
condemn a model.

## Watching for new models

Every provider is re-read on `DISCOVERY_INTERVAL`. Set it to `24h` for a daily
sync. Each refresh reports what arrived and what was withdrawn, in the log and
on the dashboard, and `first_seen` is preserved across refreshes so a model
available since last month does not look new every time.

## How routing picks a model

Requests are classified by task (code generation, analysis, creative,
conversation), then the models that serve that task are ranked by the
configured objective. Cost ranking blends input and output prices at a 3:1
ratio, so a model that is cheap to read and ruinous to write is not flattered.
Latency ranking prefers the median latency actually measured over the last hour
and falls back to the catalogue estimate until there is enough traffic to
measure. Failover walks the ranking one provider at a time, and a provider that
fails repeatedly is taken out of rotation until its cooldown expires.

## The semantic cache

A request whose prompt is close enough to one already answered is served from
storage instead of a provider. It is deliberately conservative:

- Entries are scoped to the requested model, and nothing else. The cache is
  global: any client key may be served a completion produced for any other.
- A hit must clear `CACHE_THRESHOLD`; the nearest stored entry is not an answer
  unless it is actually near.
- Requests carrying `tools`, `functions`, `response_format`, `seed`, `n`, or
  `logprobs` bypass the cache in both directions, because a cached plain-text
  completion is the wrong answer to them.
- Requests above `CACHE_MAX_TEMPERATURE` are never cached or served.

Hits are logged to `usage_logs` with `cache_hit = true` and zero tokens, so
they do not distort the latency average on the dashboard.

## Deploying

See [DEPLOY.md](DEPLOY.md) for Dokploy deployment. Migrations run automatically
on startup; any Postgres 13+ works. `pgvector` is optional — without it the
semantic cache stays off and everything else runs unchanged.

## Development

```bash
go test ./...
```

Tests that need a database skip unless `TEST_DATABASE_URL` is set:

```bash
docker run -d --name cr-test -e POSTGRES_PASSWORD=test -e POSTGRES_USER=test -e POSTGRES_DB=test -p 55432:5432 pgvector/pgvector:pg16
TEST_DATABASE_URL='postgres://test:test@localhost:55432/test?sslmode=disable' go test ./...
```
