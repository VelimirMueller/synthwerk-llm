-- Past conversations (synthwerk#54). Runs at every start: idempotent.
--
-- origin: the synthwerk:origin claim of the widget token that created the
-- conversation, or '' for a user token. A widget token lists, reads and sends only
-- on conversations of its own origin (store.Owner.Origin), so a widget on site A
-- never sees what the same user wrote on site B. A user token sees every origin.
ALTER TABLE llm.conversation ADD COLUMN IF NOT EXISTS origin text NOT NULL DEFAULT '';

-- The list reads one owner's conversations newest first (id is a UUIDv7). A user
-- token reads every origin (first index), a widget token one origin (second index),
-- so neither walks the rows of other origins.
CREATE INDEX IF NOT EXISTS conversation_owner_list ON llm.conversation (tenant_id, owner_id, id DESC)
    WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS conversation_owner_origin_list ON llm.conversation (tenant_id, owner_id, origin, id DESC)
    WHERE deleted_at IS NULL;
