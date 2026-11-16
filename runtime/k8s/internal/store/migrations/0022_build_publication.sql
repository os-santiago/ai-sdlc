-- Inactive ordered build/publication facts. No cleanup operation frees a slot.
CREATE TABLE build_plans (
 request_id text PRIMARY KEY REFERENCES build_requests(id),
 digest text NOT NULL CHECK(digest ~ '^[a-f0-9]{64}$'),
 plan bytea NOT NULL CHECK(octet_length(plan) BETWEEN 1 AND 8192),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE build_observations (
 request_id text NOT NULL REFERENCES build_requests(id),
 kind text NOT NULL CHECK(kind IN ('build','publish')),
 digest text NOT NULL CHECK(digest ~ '^[a-f0-9]{64}$'),
 observation bytea NOT NULL CHECK(octet_length(observation) BETWEEN 1 AND 16384),
 recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(request_id,kind)
);
CREATE TABLE built_records (
 request_id text PRIMARY KEY REFERENCES build_requests(id),
 digest text NOT NULL CHECK(digest ~ '^[a-f0-9]{64}$'),
 result bytea NOT NULL CHECK(octet_length(result) BETWEEN 1 AND 8192),
 recorded_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
ALTER TABLE build_plans ENABLE ROW LEVEL SECURITY;
ALTER TABLE build_plans FORCE ROW LEVEL SECURITY;
ALTER TABLE build_observations ENABLE ROW LEVEL SECURITY;
ALTER TABLE build_observations FORCE ROW LEVEL SECURITY;
ALTER TABLE built_records ENABLE ROW LEVEL SECURITY;
ALTER TABLE built_records FORCE ROW LEVEL SECURITY;
CREATE POLICY build_plan_read ON build_plans FOR SELECT USING(current_user IN ('aisdlc_build_controller','aisdlc_verifier'));
CREATE POLICY build_plan_insert ON build_plans FOR INSERT WITH CHECK(current_user='aisdlc_build_controller');
CREATE POLICY build_observation_read ON build_observations FOR SELECT USING(current_user IN ('aisdlc_build_controller','aisdlc_verifier'));
CREATE POLICY build_observation_insert ON build_observations FOR INSERT WITH CHECK(current_user='aisdlc_build_controller');
CREATE POLICY built_record_read ON built_records FOR SELECT USING(current_user IN ('aisdlc_build_controller','aisdlc_verifier'));
CREATE POLICY built_record_insert ON built_records FOR INSERT WITH CHECK(
 current_user='aisdlc_build_controller' AND EXISTS(
  SELECT 1 FROM build_requests r WHERE r.id=built_records.request_id
   AND r.state='claimed' AND r.claim_until>clock_timestamp() AND r.expires_at>clock_timestamp()
   AND built_records.recorded_at>=r.created_at AND built_records.recorded_at<r.expires_at
 ));
CREATE POLICY build_verifier_read ON build_requests FOR SELECT USING(current_user='aisdlc_verifier');
CREATE POLICY build_verifier_read ON build_slots FOR SELECT USING(current_user='aisdlc_verifier');
CREATE POLICY build_verifier_read ON build_effects FOR SELECT USING(current_user='aisdlc_verifier');
REVOKE ALL ON build_plans,build_observations,built_records FROM PUBLIC;

CREATE TABLE build_cleanups (
 request_id text PRIMARY KEY REFERENCES build_requests(id),
 digest text NOT NULL CHECK(digest ~ '^[a-f0-9]{64}$'),
 proof bytea NOT NULL CHECK(octet_length(proof) BETWEEN 1 AND 8192),
 recorded_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
ALTER TABLE build_cleanups ENABLE ROW LEVEL SECURITY;
ALTER TABLE build_cleanups FORCE ROW LEVEL SECURITY;
CREATE POLICY cleanup_reader ON build_cleanups FOR SELECT USING(current_user='aisdlc_build_controller');
CREATE POLICY cleanup_writer ON build_cleanups FOR INSERT WITH CHECK(current_user='aisdlc_build_controller' AND EXISTS(SELECT 1 FROM build_requests r WHERE r.id=build_cleanups.request_id AND r.state IN ('failed','built')));
REVOKE ALL ON build_cleanups FROM PUBLIC;
