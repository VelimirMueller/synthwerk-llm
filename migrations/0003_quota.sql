-- Token quotas per user and per org (#33). Runs at every start: idempotent.
--
-- quota_usage holds one row per org (owner_id = '') and one per user in the org,
-- per window (one UTC day). A send books its estimate in "reserved" in the
-- transaction of the turn; the finish moves it to "used" as the real count. The
-- conditional UPDATE locks the org row first, then the user row, so concurrent
-- sends of one org queue on the org row and never book past the limit.
--
-- The message keeps what it booked (quota_reserved, quota_window), so the finish
-- and the startup sweep settle exactly that amount. Rows from before this
-- migration have quota_window NULL and are never settled.

ALTER TABLE llm.message ADD COLUMN IF NOT EXISTS quota_reserved integer NOT NULL DEFAULT 0;
ALTER TABLE llm.message ADD COLUMN IF NOT EXISTS quota_window date;

CREATE TABLE IF NOT EXISTS llm.quota_usage (
    tenant_id     text   NOT NULL,
    owner_id      text   NOT NULL,
    window_start  date   NOT NULL,
    used          bigint NOT NULL DEFAULT 0 CHECK (used >= 0),
    reserved      bigint NOT NULL DEFAULT 0 CHECK (reserved >= 0),
    PRIMARY KEY (tenant_id, owner_id, window_start)
);

ALTER TABLE llm.quota_usage ENABLE ROW LEVEL SECURITY;
ALTER TABLE llm.quota_usage FORCE ROW LEVEL SECURITY;

-- A transaction sees the row of its org and its own row. Never a peer's row.
DROP POLICY IF EXISTS org_and_owner ON llm.quota_usage;
CREATE POLICY org_and_owner ON llm.quota_usage
    USING (tenant_id = current_setting('synthwerk.tenant_id', true)
       AND owner_id IN ('', current_setting('synthwerk.owner_id', true)))
    WITH CHECK (tenant_id = current_setting('synthwerk.tenant_id', true)
       AND owner_id IN ('', current_setting('synthwerk.owner_id', true)));

-- Startup sweep only (store.SweepStreaming): settles the bookings of answers a
-- crash left in 'streaming'. Like the sweep policy on llm.message it spans all
-- orgs on purpose; only the service's own code sets synthwerk.sweep, in that one
-- transaction.
DROP POLICY IF EXISTS sweep ON llm.quota_usage;
CREATE POLICY sweep ON llm.quota_usage FOR UPDATE
    USING (current_setting('synthwerk.sweep', true) = 'on')
    WITH CHECK (current_setting('synthwerk.sweep', true) = 'on');
DROP POLICY IF EXISTS sweep_read ON llm.quota_usage;
CREATE POLICY sweep_read ON llm.quota_usage FOR SELECT
    USING (current_setting('synthwerk.sweep', true) = 'on');
