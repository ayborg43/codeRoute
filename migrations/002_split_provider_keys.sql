-- 001 conflated two different secrets in api_keys: the client keys editors send
-- to CodeRouter, and the upstream provider keys CodeRouter sends onward.
-- They have different lifecycles, so they get different tables.

CREATE TABLE provider_keys (
    provider VARCHAR(50) PRIMARY KEY,
    encrypted_key BYTEA NOT NULL,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

-- api_keys now holds only client keys, which carry no provider and are stored
-- as a hash alone -- CodeRouter never needs to read them back.
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS name VARCHAR(100);
ALTER TABLE api_keys ALTER COLUMN encrypted_key DROP NOT NULL;
ALTER TABLE api_keys ALTER COLUMN provider DROP NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_api_keys_hash_unique ON api_keys(key_hash);
