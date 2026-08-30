-- Tenancy and per-tenant caps. Until now a client key was an uncapped
-- credential against whatever provider keys the gateway held.

CREATE TABLE IF NOT EXISTS tenants (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name VARCHAR(120) NOT NULL UNIQUE,
    status VARCHAR(20) NOT NULL DEFAULT 'active',
    requests_per_minute INT NOT NULL DEFAULT 60,
    tokens_per_day BIGINT NOT NULL DEFAULT 1000000,
    created_at TIMESTAMPTZ DEFAULT NOW()
);

ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS tenant_id UUID REFERENCES tenants(id) ON DELETE CASCADE;
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS disabled_at TIMESTAMPTZ;

-- Denormalised onto usage_logs so quota accounting survives key revocation
-- and needs no join on the hot path.
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS tenant_id UUID REFERENCES tenants(id) ON DELETE SET NULL;

-- Keys minted before tenancy existed are adopted by a default tenant.
INSERT INTO tenants (name)
SELECT 'default'
WHERE NOT EXISTS (SELECT 1 FROM tenants WHERE name = 'default');

UPDATE api_keys
SET tenant_id = (SELECT id FROM tenants WHERE name = 'default')
WHERE tenant_id IS NULL;

ALTER TABLE api_keys ALTER COLUMN tenant_id SET NOT NULL;

CREATE INDEX IF NOT EXISTS idx_api_keys_tenant ON api_keys(tenant_id);
CREATE INDEX IF NOT EXISTS idx_usage_logs_tenant_day ON usage_logs(tenant_id, created_at);
