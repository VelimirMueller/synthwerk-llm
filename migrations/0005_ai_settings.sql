-- AI settings of an org (synthwerk#39). Runs at every start: idempotent.
--
-- One row per org (tenant_id = the Zitadel org id of the token). No row: the org
-- uses the service defaults. An owner or admin replaces the row with PUT
-- /v1/ai-settings; version goes up by one per change and guards against lost
-- updates. No user id is stored: the audit list (identity) keeps who changed it.
CREATE TABLE IF NOT EXISTS llm.org_settings (
    tenant_id           text        PRIMARY KEY,
    model_alias         text        NOT NULL CHECK (model_alias ~ '^[a-z][a-z0-9_-]{0,63}$'),
    allowed_models      text[]      NOT NULL CHECK (cardinality(allowed_models) BETWEEN 1 AND 32),
    system_prompt       text        NOT NULL DEFAULT '' CHECK (char_length(system_prompt) <= 4000),
    max_input_chars     integer     NOT NULL CHECK (max_input_chars BETWEEN 1 AND 8000),
    user_tokens_per_day bigint      NOT NULL DEFAULT 0 CHECK (user_tokens_per_day BETWEEN 0 AND 1000000000000),
    temperature         double precision CHECK (temperature BETWEEN 0 AND 2),
    version             bigint      NOT NULL CHECK (version >= 1),
    updated_at          timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE llm.org_settings ENABLE ROW LEVEL SECURITY;
ALTER TABLE llm.org_settings FORCE ROW LEVEL SECURITY;

-- A transaction sees and writes the row of its own org only. The handler lets
-- only owners and admins write (token role + identity membership check).
DROP POLICY IF EXISTS org_only ON llm.org_settings;
CREATE POLICY org_only ON llm.org_settings
    USING (tenant_id <> '' AND tenant_id = current_setting('synthwerk.tenant_id', true))
    WITH CHECK (tenant_id <> '' AND tenant_id = current_setting('synthwerk.tenant_id', true));

-- Erase (store.EraseUser): an org that closed with the account loses its settings.
-- synthwerk.erase_orgs is the comma list of 0004.
DROP POLICY IF EXISTS erase ON llm.org_settings;
CREATE POLICY erase ON llm.org_settings FOR DELETE
    USING (tenant_id = ANY (string_to_array(NULLIF(current_setting('synthwerk.erase_orgs', true), ''), ',')));
DROP POLICY IF EXISTS erase_read ON llm.org_settings;
CREATE POLICY erase_read ON llm.org_settings FOR SELECT
    USING (tenant_id = ANY (string_to_array(NULLIF(current_setting('synthwerk.erase_orgs', true), ''), ',')));
