# codeRoute

OpenAI-compatible API gateway that routes requests across multiple LLM
providers, with failover, BYOK key storage, smart model selection, and an
IoT bridge. Self-hosted, Go, backed by PostgreSQL.

## Running

```bash
OPENAI_API_KEY=sk-... ANTHROPIC_API_KEY=sk-ant-... docker compose up --build
```

The gateway prints a client key once on first start; use it as the API key in
your editor and point the base URL at `http://localhost:8080/v1`.

## Endpoints

| Endpoint | Purpose |
|---|---|
| `POST /v1/chat/completions` | OpenAI-compatible completions, streaming supported |
| `GET /v1/models` | Models available given the configured provider keys |
| `POST /v1/iot/inference` | Device inference, edge-first with cloud fallback |
| `POST /v1/iot/telemetry` | Device telemetry ingest |
| `GET /v1/iot/telemetry?device_id=` | Recent readings for a device |
| `GET /health` | Liveness, provider key status, MQTT status |

## Configuration

| Variable | Default | Purpose |
|---|---|---|
| `DATABASE_URL` | local postgres | Connection string |
| `ENCRYPTION_KEY` | committed default | AES key for stored provider keys — **change this** |
| `OPENAI_API_KEY` / `ANTHROPIC_API_KEY` / `GOOGLE_API_KEY` | — | BYOK bootstrap, encrypted into Postgres on startup |
| `ROUTING_MODE` | `auto` | `auto` (route only when the client asks for `auto`), `always`, `off` |
| `ROUTING_OBJECTIVE` | `balanced` | `balanced`, `latency`, or `cost` |
| `MQTT_BROKER` | — | Enables the MQTT bridge |
| `IOT_EDGE_ENDPOINT` | — | OpenAI-compatible local model server, tried before the cloud |

## Deploying

See [DEPLOY.md](DEPLOY.md) for Dokploy deployment. Migrations run automatically
on startup; any Postgres 13+ works, with `pgvector` optional.

See `sould.md` for the intended feature set and what is still outstanding.
