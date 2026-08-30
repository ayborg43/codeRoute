# CodeRouter

OpenAI-compatible API gateway (drop-in for VSCode/Cursor)

- Multi-provider routing with intelligent failover + circuit breaker
- BYOK with encrypted key storage in PostgreSQL
- Smart routing (task detection, latency/cost optimization, semantic caching)
- IoT integration (MQTT/HTTP bridge, edge inference, telemetry pipeline)
- Lightweight self-hosted deployment on VPS via Dokploy (<50MB RAM)
- Observability (logging, health dashboard, usage analytics)
- ~~Multi-tenant user management with rate limiting~~ — withdrawn. Tenancy and
  the per-tenant caps were removed in migration 007; a client key is now a bare
  credential with no allowance, and the semantic cache is global.

