CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE api_keys (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    key_hash BYTEA NOT NULL,
    encrypted_key BYTEA NOT NULL,
    provider VARCHAR(50) NOT NULL,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    last_used_at TIMESTAMPTZ
);

CREATE TABLE usage_logs (
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

CREATE TABLE config (
    key VARCHAR(100) PRIMARY KEY,
    value JSONB NOT NULL,
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE TABLE cache_entries (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    embedding vector(1536) NOT NULL,
    response TEXT NOT NULL,
    model VARCHAR(100) NOT NULL,
    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX idx_cache_embedding ON cache_entries USING ivfflat (embedding vector_cosine_ops);
CREATE INDEX idx_usage_logs_created ON usage_logs(created_at);
CREATE INDEX idx_api_keys_hash ON api_keys(key_hash);

