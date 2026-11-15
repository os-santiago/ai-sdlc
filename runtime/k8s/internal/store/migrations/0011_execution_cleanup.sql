-- Cleanup may delete only acknowledged UIDs. It can never reopen an execution.
ALTER TABLE execution_lifecycles
 ADD COLUMN cleanup_state text NOT NULL DEFAULT 'pending' CHECK (cleanup_state IN ('pending','complete','uncertain')),
 ADD COLUMN cleanup_attempts integer NOT NULL DEFAULT 0 CHECK (cleanup_attempts BETWEEN 0 AND 3),
 ADD COLUMN cleanup_after timestamptz NOT NULL DEFAULT clock_timestamp();
CREATE INDEX execution_cleanup_pending ON execution_lifecycles(cleanup_after) WHERE cleanup_state='pending';
