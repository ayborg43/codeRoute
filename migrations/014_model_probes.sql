-- Records whether a model has been confirmed to work for this account.
--
-- Routing previously learned only from real traffic: a model was discovered
-- to be unusable when someone's request hit it and failed. With hundreds of
-- models across a dozen providers, and accounts entitled to only some of them,
-- that meant real requests were doing the discovery — and paying for it in
-- latency and failed calls.
--
-- A probe is a tiny completion sent on a schedule. It costs a token or two and
-- turns "we will find out when someone asks" into "we already know".

CREATE TABLE IF NOT EXISTS model_probes (
    provider    VARCHAR(50)  NOT NULL,
    model       VARCHAR(200) NOT NULL,
    ok          BOOLEAN      NOT NULL,
    checked_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    latency_ms  INT          NOT NULL DEFAULT 0,
    -- The upstream's own words when it refused, so an operator can tell a
    -- spent quota from a model this account may never use.
    failure     TEXT,
    PRIMARY KEY (provider, model)
);

CREATE INDEX IF NOT EXISTS idx_model_probes_ok ON model_probes(ok, checked_at DESC);
