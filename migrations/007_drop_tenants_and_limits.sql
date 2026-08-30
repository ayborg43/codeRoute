-- Removes multi-tenancy and the per-tenant caps that hung from it.
--
-- 005 introduced tenants so that limits, quota accounting, cache isolation and
-- a suspend switch had somewhere to live. That machinery is being withdrawn:
-- CodeRouter is now a single-occupancy gateway where a client key is simply a
-- credential, carrying no allowance of its own.
--
-- Consequences, recorded here because they are not obvious from the schema:
--   * A leaked client key means uncapped spend against the stored provider
--     keys. Nothing rate-limits or budgets a caller any more.
--   * The semantic cache is now global. Any key may be served a completion
--     produced for any other key.
-- Key revocation survives: api_keys.disabled_at is a property of the key, not
-- of tenancy, and is the only remaining way to cut a caller off.

-- Columns go before the table so dropping it cannot take rows with it.
ALTER TABLE api_keys DROP COLUMN IF EXISTS tenant_id;
ALTER TABLE usage_logs DROP COLUMN IF EXISTS tenant_id;

DROP INDEX IF EXISTS idx_api_keys_tenant;
DROP INDEX IF EXISTS idx_usage_logs_tenant_day;

-- The cache only exists where pgvector does.
DO $$
BEGIN
    IF to_regclass('public.cache_entries') IS NULL THEN
        RETURN;
    END IF;

    -- Existing entries were written under a tenant scope that no longer
    -- applies. Rather than silently widening their audience, drop them and
    -- let the cache refill under the new rules.
    EXECUTE 'DELETE FROM cache_entries';
    EXECUTE 'DROP INDEX IF EXISTS idx_cache_tenant_model';
    EXECUTE 'ALTER TABLE cache_entries DROP COLUMN IF EXISTS tenant_id';
    EXECUTE 'CREATE INDEX IF NOT EXISTS idx_cache_model ON cache_entries(model)';
END $$;

DROP TABLE IF EXISTS tenants;
