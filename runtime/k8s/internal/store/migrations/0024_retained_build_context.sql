-- No backfill: historical builds lacking exact compilation context cannot be
-- operationally provenance-verified. Context is committed before either create.
CREATE TABLE build_contexts (
 request_id text PRIMARY KEY REFERENCES build_requests(id),
 digest text NOT NULL CHECK(digest ~ '^[a-f0-9]{64}$'),
 context bytea NOT NULL CHECK(octet_length(context) BETWEEN 1 AND 131072),
 recorded_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
ALTER TABLE build_contexts ENABLE ROW LEVEL SECURITY;
ALTER TABLE build_contexts FORCE ROW LEVEL SECURITY;
CREATE POLICY build_context_read ON build_contexts FOR SELECT USING(current_user IN ('aisdlc_build_controller','aisdlc_verifier'));
CREATE POLICY build_context_insert ON build_contexts FOR INSERT WITH CHECK(
 current_user='aisdlc_build_controller' AND EXISTS(
  SELECT 1 FROM build_requests r WHERE r.id=build_contexts.request_id
   AND r.state='claimed' AND r.claim_until>clock_timestamp() AND r.expires_at>clock_timestamp()
   AND NOT EXISTS(SELECT 1 FROM build_effects e WHERE e.request_id=r.id)
   AND NOT EXISTS(SELECT 1 FROM build_plans p WHERE p.request_id=r.id)
   AND build_contexts.recorded_at>=r.created_at AND build_contexts.recorded_at<r.expires_at
 ));
REVOKE ALL ON build_contexts FROM PUBLIC;
