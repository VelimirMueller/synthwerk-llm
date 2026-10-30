-- Own data of one user across all orgs (synthwerk#41). Runs at every start: idempotent.
--
-- Two code paths read or delete the rows of one user in every org. The normal
-- policies (0002, 0003) stop at one org, so each path gets its own narrow policy:
--
-- - Export (store.Export): SET LOCAL synthwerk.export_owner = <sub> in a READ ONLY
--   transaction. SELECT only. The user's conversations, messages and own quota rows.
--   Never the org quota row (owner_id = '').
-- - Erase (store.EraseUser, on identity user.deleted.v1): SET LOCAL
--   synthwerk.erase_owner = <sub> and synthwerk.erase_orgs = <org,org,...> in one
--   transaction. DELETE (+ the SELECT that a DELETE with WHERE needs). The user's
--   rows, and the org quota rows of the orgs that closed with the account.
--
-- Only the service's own code sets these settings, from the verified token (export)
-- or from the validated event (erase). An unset setting is NULL, but a setting used
-- once on a pooled connection reads '' in later transactions: every policy needs a
-- non-empty match, so '' never matches the org rows (owner_id = '').

-- Owner-first lookups across orgs. The (tenant_id, owner_id, ...) indexes do not help.
CREATE INDEX IF NOT EXISTS conversation_owner_any ON llm.conversation (owner_id);
CREATE INDEX IF NOT EXISTS message_owner_any ON llm.message (owner_id);
CREATE INDEX IF NOT EXISTS quota_usage_owner_any ON llm.quota_usage (owner_id);

-- Export: read only.
DROP POLICY IF EXISTS export_read ON llm.conversation;
CREATE POLICY export_read ON llm.conversation FOR SELECT
    USING (owner_id <> '' AND owner_id = current_setting('synthwerk.export_owner', true));
DROP POLICY IF EXISTS export_read ON llm.message;
CREATE POLICY export_read ON llm.message FOR SELECT
    USING (owner_id <> '' AND owner_id = current_setting('synthwerk.export_owner', true));
DROP POLICY IF EXISTS export_read ON llm.quota_usage;
CREATE POLICY export_read ON llm.quota_usage FOR SELECT
    USING (owner_id <> '' AND owner_id = current_setting('synthwerk.export_owner', true));

-- Erase: delete the user's rows in every org.
DROP POLICY IF EXISTS erase ON llm.conversation;
CREATE POLICY erase ON llm.conversation FOR DELETE
    USING (owner_id <> '' AND owner_id = current_setting('synthwerk.erase_owner', true));
DROP POLICY IF EXISTS erase_read ON llm.conversation;
CREATE POLICY erase_read ON llm.conversation FOR SELECT
    USING (owner_id <> '' AND owner_id = current_setting('synthwerk.erase_owner', true));
DROP POLICY IF EXISTS erase ON llm.message;
CREATE POLICY erase ON llm.message FOR DELETE
    USING (owner_id <> '' AND owner_id = current_setting('synthwerk.erase_owner', true));
DROP POLICY IF EXISTS erase_read ON llm.message;
CREATE POLICY erase_read ON llm.message FOR SELECT
    USING (owner_id <> '' AND owner_id = current_setting('synthwerk.erase_owner', true));

-- Erase on quota_usage: the user's own rows, plus the org rows of the closed orgs.
-- synthwerk.erase_orgs is a comma list of Zitadel org ids ([A-Za-z0-9_-], no comma).
DROP POLICY IF EXISTS erase ON llm.quota_usage;
CREATE POLICY erase ON llm.quota_usage FOR DELETE
    USING ((owner_id <> '' AND owner_id = current_setting('synthwerk.erase_owner', true))
        OR (owner_id = '' AND tenant_id <> ''
            AND tenant_id = ANY (string_to_array(NULLIF(current_setting('synthwerk.erase_orgs', true), ''), ','))));
DROP POLICY IF EXISTS erase_read ON llm.quota_usage;
CREATE POLICY erase_read ON llm.quota_usage FOR SELECT
    USING ((owner_id <> '' AND owner_id = current_setting('synthwerk.erase_owner', true))
        OR (owner_id = '' AND tenant_id <> ''
            AND tenant_id = ANY (string_to_array(NULLIF(current_setting('synthwerk.erase_orgs', true), ''), ','))));
