-- Caches what each provider says it serves.
--
-- Discovery is a network call per provider, so the result is persisted: a
-- restart routes correctly straight away instead of being unable to resolve
-- any model until the first refresh completes.
--
-- price_known separates "this model is free" from "nobody published a price".
-- Free-only routing depends on that distinction, so it is stored rather than
-- inferred from a zero.

CREATE TABLE IF NOT EXISTS discovered_models (
    provider            VARCHAR(50)  NOT NULL,
    model               VARCHAR(200) NOT NULL,
    input_cost_per_1m   DOUBLE PRECISION NOT NULL DEFAULT 0,
    output_cost_per_1m  DOUBLE PRECISION NOT NULL DEFAULT 0,
    price_known         BOOLEAN NOT NULL DEFAULT FALSE,
    context_length      INT NOT NULL DEFAULT 0,
    discovered_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (provider, model)
);

CREATE INDEX IF NOT EXISTS idx_discovered_free
    ON discovered_models(provider) WHERE price_known AND input_cost_per_1m = 0 AND output_cost_per_1m = 0;
