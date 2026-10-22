CREATE SCHEMA IF NOT EXISTS llm;

CREATE TABLE IF NOT EXISTS llm.synthwerk_outbox (
    id            uuid        PRIMARY KEY,
    nats_subject  text        NOT NULL,
    event         jsonb       NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    published_at  timestamptz,
    attempts      integer     NOT NULL DEFAULT 0,
    last_error    text
);

CREATE INDEX IF NOT EXISTS synthwerk_outbox_unpublished
    ON llm.synthwerk_outbox (created_at, id) WHERE published_at IS NULL;
