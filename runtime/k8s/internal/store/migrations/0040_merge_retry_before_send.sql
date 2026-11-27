-- A merge request that provably sent nothing (it stopped before any merge
-- effect was begun, or its only effect is sealed not-sent) fails with reason
-- 'not-sent', and its publication may receive a bounded fresh attempt with a
-- new gate observation and signed command (docs/operations/merge-journal.md).
-- Every other request stays unique per publication: at most one request per
-- publication is queued, claimed, merged, uncertain or expired, so a
-- publication can never be merged twice and an uncertain send is never
-- retried. This only relaxes constraints: the full unique constraint becomes a
-- partial unique index and the reason check admits one more value. It adds no
-- table, column, role, grant or row, so configuration upgrades may apply it
-- (see store.RelaxationMigrations).
ALTER TABLE merge_requests DROP CONSTRAINT merge_requests_publication_id_key;
CREATE UNIQUE INDEX merge_requests_publication_attempt ON merge_requests(publication_id) WHERE reason<>'not-sent';
ALTER TABLE merge_requests DROP CONSTRAINT merge_requests_reason_check;
ALTER TABLE merge_requests ADD CONSTRAINT merge_requests_reason_check
    CHECK(reason IN ('','expired','revoked','uncertain','rejected','not-sent'));
