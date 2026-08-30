-- Dashboard sign-in by email and password.
--
-- Until now the dashboard was guarded by ADMIN_TOKEN: one shared secret, held
-- by everyone who needed access, with no way to tell who did what or to revoke
-- one person without changing it for all of them. That is adequate for a
-- single operator and poor for anyone else.
--
-- ADMIN_TOKEN is deliberately kept working for programmatic access. Scripts
-- and curl should not have to hold a session, and forcing them through a login
-- flow would push people towards storing an email and password in a shell
-- history instead.

CREATE TABLE IF NOT EXISTS users (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Stored lowercased so addresses are unique without regard to case;
    -- 320 is the maximum length an email address may have.
    email         VARCHAR(320) NOT NULL UNIQUE,
    password_hash BYTEA        NOT NULL,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    last_login_at TIMESTAMPTZ,
    -- Disabled rather than deleted, so a departed user's sessions can be cut
    -- off while any record referring to them stays intact.
    disabled_at   TIMESTAMPTZ
);

-- Only the hash of a session token is stored, for the same reason only the
-- hash of a client key is: a database dump must not hand over live sessions.
CREATE TABLE IF NOT EXISTS sessions (
    token_hash   BYTEA       PRIMARY KEY,
    user_id      UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at   TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id);
CREATE INDEX IF NOT EXISTS idx_sessions_expiry ON sessions(expires_at);
