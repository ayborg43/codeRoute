-- Makes the semantic cache usable and the usage log answerable.
--
-- Before this, cache_entries had no tenant column (so a hit could serve one
-- tenant's completion to another), no expiry, and no way to tell a cached
-- response from an upstream one after the fact.

ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS cache_hit BOOLEAN NOT NULL DEFAULT FALSE;

-- cost_usd has existed since 001 but was never written. Nothing to add here;
-- it is populated from the model catalogue's prices from now on.

CREATE INDEX IF NOT EXISTS idx_usage_logs_model_latency
    ON usage_logs(model, created_at) WHERE status = 'success';

-- The cache tables live behind pgvector, which is optional: the gateway also
-- deploys against a managed Postgres without it. Statements stay dynamic so
-- the vector type is resolved at execution rather than at parse time.
DO $$
BEGIN
    IF to_regclass('public.cache_entries') IS NULL THEN
        RAISE NOTICE 'cache_entries absent (no pgvector); semantic cache stays disabled';
        RETURN;
    END IF;

    EXECUTE 'ALTER TABLE cache_entries ADD COLUMN IF NOT EXISTS tenant_id UUID REFERENCES tenants(id) ON DELETE CASCADE';
    EXECUTE 'ALTER TABLE cache_entries ADD COLUMN IF NOT EXISTS prompt TEXT';
    EXECUTE 'ALTER TABLE cache_entries ADD COLUMN IF NOT EXISTS hits INT NOT NULL DEFAULT 0';
    EXECUTE 'ALTER TABLE cache_entries ADD COLUMN IF NOT EXISTS last_hit_at TIMESTAMPTZ';
    EXECUTE 'ALTER TABLE cache_entries ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ';

    -- Entries written before tenancy have no owner and could be served to
    -- anyone. There is no way to attribute them after the fact, so they go.
    EXECUTE 'DELETE FROM cache_entries WHERE tenant_id IS NULL';
    EXECUTE 'ALTER TABLE cache_entries ALTER COLUMN tenant_id SET NOT NULL';

    EXECUTE 'CREATE INDEX IF NOT EXISTS idx_cache_tenant_model ON cache_entries(tenant_id, model)';
    EXECUTE 'CREATE INDEX IF NOT EXISTS idx_cache_expires ON cache_entries(expires_at)';

    -- ivfflat built on an empty table has no centroids to work with and gives
    -- poor recall forever after. HNSW builds incrementally and needs no
    -- training pass, so prefer it where the pgvector version provides it.
    EXECUTE 'DROP INDEX IF EXISTS idx_cache_embedding';
    BEGIN
        EXECUTE 'CREATE INDEX IF NOT EXISTS idx_cache_embedding_hnsw
                 ON cache_entries USING hnsw (embedding vector_cosine_ops)';
    EXCEPTION WHEN OTHERS THEN
        -- pgvector < 0.5.0. Lookups fall back to a sequential scan, which is
        -- fine at the scale a self-hosted gateway caches at.
        RAISE NOTICE 'hnsw unavailable; semantic cache will scan sequentially';
    END;
END $$;
