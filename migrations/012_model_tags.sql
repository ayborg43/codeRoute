-- Lets an operator declare which models are for which kind of work.
--
-- Until now task fitness was inferred from a model's name, which is a guess:
-- it cannot know that a particular general-purpose model happens to be the
-- best coder available on your account, and it cannot know that a model with
-- "code" in its name is one you would rather never use.
--
-- A tag here is a statement, not a hint. Where any model is tagged for a task,
-- routing for that task uses only the tagged ones.
--
-- Tags are keyed on provider and model rather than referencing
-- discovered_models, so they survive a provider's list being rewritten by
-- discovery, and can be applied to a model that is temporarily unlisted.

CREATE TABLE IF NOT EXISTS model_tags (
    provider   VARCHAR(50)  NOT NULL,
    model      VARCHAR(200) NOT NULL,
    task       VARCHAR(32)  NOT NULL,
    created_at TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (provider, model, task)
);

CREATE INDEX IF NOT EXISTS idx_model_tags_task ON model_tags(task);
