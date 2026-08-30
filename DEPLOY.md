# Deploying CodeRouter on Dokploy

The gateway applies its own database migrations on boot, drains connections on
SIGTERM, and ships a container healthcheck — so it survives Dokploy's rolling
redeploys without manual schema steps.

## Option A — Compose (bundled Postgres)

Deploys the gateway and its database together. Simplest if you have no database
yet.

1. In Dokploy, **Create Application → Compose**, and point it at this
   repository.
2. Set **Compose Path** to `docker-compose.dokploy.yml`.
3. Paste the variables from [`.env.example`](.env.example) into the
   **Environment** tab. At minimum:

   | Variable | Notes |
   |---|---|
   | `ENCRYPTION_KEY` | Exactly 16, 24, or 32 bytes. `openssl rand -base64 24` |
   | `POSTGRES_PASSWORD` | Any strong value |
   | `DOMAIN` | The hostname you will serve on |
   | `ADMIN_TOKEN` | Enables the dashboard and key management. `openssl rand -hex 32` |

   A provider key is optional here — set `OPENAI_API_KEY`,
   `ANTHROPIC_API_KEY`, or `GOOGLE_API_KEY` to seed one on first boot, or add
   it from the dashboard afterwards.

4. Point your domain's DNS at the VPS, then **Deploy**.

The compose file already carries Traefik labels for `${DOMAIN}` on the
`websecure` entrypoint. If you would rather attach the domain through Dokploy's
UI, delete the `labels:` block from the `coderouter` service so the two do not
compete, and confirm the service port is `8080`.

## Option B — Application + managed database

Use this if you want Dokploy to manage Postgres as its own service, or you are
pointing at an external database.

1. **Create Database → PostgreSQL** in Dokploy and note its internal connection
   string.
2. **Create Application → Dockerfile**, pointed at this repository. The
   `Dockerfile` at the root needs no changes; the exposed port is `8080`.
3. Set `DATABASE_URL` to the database's internal URL, plus the variables from
   the table above (`POSTGRES_PASSWORD` and `DOMAIN` are not needed here —
   attach the domain through the UI).
4. Deploy.

Any Postgres 13 or newer works. `pgvector` is optional: where it is present the
semantic cache table is created, and where it is not that step is skipped and
everything else runs normally.

## First run

The gateway prints a client key **once**, on the first boot against an empty
database:

```
no client keys found; created one (shown once): cr_...
```

Copy it out of the Dokploy logs immediately — only its hash is stored, so it
cannot be recovered. Use it as the API key in your editor, with the base URL
set to `https://your-domain/v1`.

Further keys are minted from the dashboard or the admin API, never from the
logs again.

Check the deployment with:

```bash
curl https://your-domain/health
```

It reports database liveness, which providers have keys, MQTT status, and
whether the semantic cache came up.

## The dashboard

With `ADMIN_TOKEN` set, `https://your-domain/` serves an operator dashboard:
traffic, spend, cache hit rate, a per-model breakdown, and the client keys. It
is where you mint and revoke keys, and add or rotate the upstream provider
keys.

Provider keys are verified against the provider before they are stored, so a
mistyped key is refused there and then. They are never readable back — the
dashboard shows a fingerprint.

The dashboard asks for `ADMIN_TOKEN` and keeps it for the browser session only.
Without `ADMIN_TOKEN` set, both the dashboard's data API and the management API
answer with a clear "disabled" response rather than serving anything.

## Operational notes

- **Secrets.** `ENCRYPTION_KEY` encrypts every stored provider key. If you
  change it, previously stored keys can no longer be decrypted and the gateway
  will fail to route until they are re-entered. The dashboard flags such keys
  as "will not decrypt" rather than hiding them. Back it up.
- **Database exposure.** `docker-compose.dokploy.yml` deliberately publishes no
  host port for Postgres; it is reachable only on the internal network.
- **Rolling deploys.** SIGTERM triggers a 30-second drain. Set Dokploy's stop
  grace period to at least that if you serve long streaming completions.
- **No rate limiting.** This was removed deliberately. A client key carries no
  request rate and no spend cap, so a leaked key means uncapped spend against
  your provider keys until you notice. Watch the dashboard's spend figure,
  revoke a key the moment it leaks (history survives revocation), and consider
  putting the gateway behind an authenticating proxy if it faces the internet.
- **Shared cache.** The semantic cache is global. Any client key may be served
  a completion produced for any other, so do not hand keys to parties whose
  prompts should not mix.
- **Admin token.** `ADMIN_TOKEN` can mint client keys, read all traffic, and
  add or remove provider keys. Treat it as the most sensitive value in the
  deployment, above any individual client key.
- **Cache cost.** With the semantic cache on, every eligible request also buys
  one embedding (fractions of a cent per thousand). That is far cheaper than
  the completion it avoids, but it is not free — set `CACHE_ENABLED=false` if
  your traffic rarely repeats. The cache needs `pgvector`; on a managed
  Postgres without it, it stays off and logs why at startup.
- **IoT.** Leave `MQTT_BROKER` empty unless you are using it; the `/v1/iot`
  HTTP endpoints work either way. The gateway trusts any device the broker
  admits, so use broker-side ACLs.
