-- gen_random_uuid() is built in from Postgres 13, so no uuid-ossp needed.

CREATE TABLE IF NOT EXISTS api_keys (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    key_hash BYTEA NOT NULL,
    encrypted_key BYTEA NOT NULL,
    provider VARCHAR(50) NOT NULL,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    last_used_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS usage_logs (
    id BIGSERIAL PRIMARY KEY,
    api_key_id UUID REFERENCES api_keys(id),
    model VARCHAR(100) NOT NULL,
    tokens_in INT NOT NULL,
    tokens_out INT NOT NULL,
    latency_ms INT NOT NULL,
    cost_usd DECIMAL(10,6),
    provider VARCHAR(50) NOT NULL,
    status VARCHAR(20) NOT NULL,
    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS config (
    key VARCHAR(100) PRIMARY KEY,
    value JSONB NOT NULL,
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_usage_logs_created ON usage_logs(created_at);
CREATE INDEX IF NOT EXISTS idx_api_keys_hash ON api_keys(key_hash);

-- The semantic cache needs pgvector. It is created only where the extension is
-- available, so the gateway also deploys against a managed Postgres without it.
-- Statements are dynamic so the vector type is resolved at execution, not parse.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_available_extensions WHERE name = 'vector') THEN
        EXECUTE 'CREATE EXTENSION IF NOT EXISTS vector';
        EXECUTE 'CREATE TABLE IF NOT EXISTS cache_entries (
            id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
            embedding vector(1536) NOT NULL,
            response TEXT NOT NULL,
            model VARCHAR(100) NOT NULL,
            created_at TIMESTAMPTZ DEFAULT NOW()
        )';
        EXECUTE 'CREATE INDEX IF NOT EXISTS idx_cache_embedding
                 ON cache_entries USING ivfflat (embedding vector_cosine_ops)';
    ELSE
        RAISE NOTICE 'pgvector unavailable; semantic cache table skipped';
    END IF;
END $$;
