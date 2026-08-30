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
   | one provider key | `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, or `GOOGLE_API_KEY` |

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

Check the deployment with:

```bash
curl https://your-domain/health
```

It reports database liveness, which providers have keys, and MQTT status.

## Operational notes

- **Secrets.** `ENCRYPTION_KEY` encrypts every stored provider key. If you
  change it, previously stored keys can no longer be decrypted and the gateway
  will fail to route until they are re-seeded. Back it up.
- **Database exposure.** `docker-compose.dokploy.yml` deliberately publishes no
  host port for Postgres; it is reachable only on the internal network.
- **Rolling deploys.** SIGTERM triggers a 30-second drain. Set Dokploy's stop
  grace period to at least that if you serve long streaming completions.
- **Rate limiting.** There is none yet. A leaked client key means uncapped
  spend against your provider keys, so treat the bootstrap key as a secret and
  do not expose the gateway more widely than you need.
- **IoT.** Leave `MQTT_BROKER` empty unless you are using it; the `/v1/iot`
  HTTP endpoints work either way. The gateway trusts any device the broker
  admits, so use broker-side ACLs.
