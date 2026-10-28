-- Conversations and messages (M2 spec #32 §3.5). Runs at every start: idempotent.
-- Row level security: a transaction sees only the rows of the org and the user it
-- names with SET LOCAL synthwerk.tenant_id / synthwerk.owner_id (store.withOwner).
-- FORCE applies the policies to the table owner too: the service role owns the schema.
-- The org and the user come from the verified token only.

CREATE TABLE IF NOT EXISTS llm.conversation (
    id           uuid        PRIMARY KEY,
    tenant_id    text        NOT NULL,
    owner_id     text        NOT NULL,
    model_alias  text        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    deleted_at   timestamptz
);

CREATE INDEX IF NOT EXISTS conversation_owner ON llm.conversation (tenant_id, owner_id, id);

CREATE TABLE IF NOT EXISTS llm.message (
    id               uuid        PRIMARY KEY,
    conversation_id  uuid        NOT NULL REFERENCES llm.conversation (id),
    tenant_id        text        NOT NULL,
    owner_id         text        NOT NULL,
    role             text        NOT NULL CHECK (role IN ('user', 'assistant')),
    content          text        NOT NULL DEFAULT '',
    status           text        NOT NULL CHECK (status IN ('streaming', 'completed', 'failed', 'canceled')),
    tokens_in        integer,
    tokens_out       integer,
    created_at       timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX IF NOT EXISTS message_owner ON llm.message (tenant_id, owner_id, conversation_id, created_at);
CREATE INDEX IF NOT EXISTS message_streaming ON llm.message (status) WHERE status = 'streaming';

ALTER TABLE llm.conversation ENABLE ROW LEVEL SECURITY;
ALTER TABLE llm.conversation FORCE ROW LEVEL SECURITY;
ALTER TABLE llm.message ENABLE ROW LEVEL SECURITY;
ALTER TABLE llm.message FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS owner_only ON llm.conversation;
CREATE POLICY owner_only ON llm.conversation
    USING (tenant_id = current_setting('synthwerk.tenant_id', true)
       AND owner_id = current_setting('synthwerk.owner_id', true))
    WITH CHECK (tenant_id = current_setting('synthwerk.tenant_id', true)
       AND owner_id = current_setting('synthwerk.owner_id', true));

DROP POLICY IF EXISTS owner_only ON llm.message;
CREATE POLICY owner_only ON llm.message
    USING (tenant_id = current_setting('synthwerk.tenant_id', true)
       AND owner_id = current_setting('synthwerk.owner_id', true))
    WITH CHECK (tenant_id = current_setting('synthwerk.tenant_id', true)
       AND owner_id = current_setting('synthwerk.owner_id', true));

-- Startup sweep only (store.SweepStreaming): marks answers a crash left in
-- 'streaming' as 'failed'. It needs SET LOCAL synthwerk.sweep = 'on' and can only
-- touch rows that are still streaming. The read policy also covers 'failed': an
-- UPDATE checks the new row against the SELECT policies too.
DROP POLICY IF EXISTS sweep ON llm.message;
CREATE POLICY sweep ON llm.message FOR UPDATE
    USING (current_setting('synthwerk.sweep', true) = 'on' AND status = 'streaming')
    WITH CHECK (current_setting('synthwerk.sweep', true) = 'on' AND status = 'failed');
DROP POLICY IF EXISTS sweep_read ON llm.message;
CREATE POLICY sweep_read ON llm.message FOR SELECT
    USING (current_setting('synthwerk.sweep', true) = 'on' AND status IN ('streaming', 'failed'));
