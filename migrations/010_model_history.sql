-- Remembers when each model was first and last seen.
--
-- Discovery previously replaced a provider's list wholesale, which answered
-- "what is available now" but not "what changed". An operator watching for a
-- newly released model had no way to see one arrive, and a model quietly
-- withdrawn by a vendor left no trace.

ALTER TABLE discovered_models ADD COLUMN IF NOT EXISTS first_seen TIMESTAMPTZ NOT NULL DEFAULT NOW();
ALTER TABLE discovered_models ADD COLUMN IF NOT EXISTS last_seen  TIMESTAMPTZ NOT NULL DEFAULT NOW();

CREATE INDEX IF NOT EXISTS idx_discovered_first_seen ON discovered_models(first_seen DESC);
