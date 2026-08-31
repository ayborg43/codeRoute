-- Lets an operator forbid routing from ever choosing a specific model.
--
-- Scoring and probing can only warn that a model looks bad or is unreachable;
-- sometimes an operator already knows a model must never be used — it has
-- been deprecated, is billed against an account nobody wants spent on, or has
-- simply produced bad output — and wants that respected outright, including
-- for a request that names the model directly rather than only automatic
-- routing around it.
--
-- Keyed on provider and model rather than referencing discovered_models, for
-- the same reason model_tags is: it survives a provider's list being
-- rewritten by discovery, and still applies to a model that is temporarily
-- unlisted.

CREATE TABLE IF NOT EXISTS model_blacklist (
    provider   VARCHAR(50)  NOT NULL,
    model      VARCHAR(200) NOT NULL,
    created_at TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (provider, model)
);
