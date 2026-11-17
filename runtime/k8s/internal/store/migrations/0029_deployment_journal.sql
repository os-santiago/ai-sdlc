-- Inactive until explicit deployment provisioning. Immutable authority and facts.
CREATE TABLE deployment_requests(
 id text PRIMARY KEY CHECK(id ~ '^deploy-[a-f0-9]{64}$'),
 promotion_id text NOT NULL UNIQUE REFERENCES promotion_records(request_id),
 payload_digest text NOT NULL CHECK(payload_digest ~ '^[a-f0-9]{64}$'),
 payload bytea NOT NULL CHECK(octet_length(payload) BETWEEN 1 AND 4096),
 receipt bytea NOT NULL CHECK(octet_length(receipt) BETWEEN 1 AND 4096),
 configuration bytea NOT NULL CHECK(octet_length(configuration) BETWEEN 1 AND 327680),
 expires_at timestamptz NOT NULL,
 state text NOT NULL DEFAULT 'queued' CHECK(state IN('queued','claimed','deployed','rolled-back','quarantined')),
 claim_token uuid,
 reason text NOT NULL DEFAULT '' CHECK(reason IN('','expired','revoked','uncertain','rejected','unhealthy')),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK((state='queued' AND claim_token IS NULL) OR state='quarantined' OR (state IN('claimed','deployed','rolled-back') AND claim_token IS NOT NULL)),
 CHECK((state='quarantined')=(reason<>''))
);
CREATE TABLE deployment_intents(
 request_id text NOT NULL REFERENCES deployment_requests(id),
 kind text NOT NULL CHECK(kind IN('advance','rollback')),
 claim_token uuid NOT NULL,
 before_snapshot bytea NOT NULL CHECK(octet_length(before_snapshot) BETWEEN 1 AND 4096),
 request_digest text NOT NULL CHECK(request_digest ~ '^[a-f0-9]{64}$'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(request_id,kind)
);
CREATE TABLE deployment_sends(
 request_id text NOT NULL,
 kind text NOT NULL,
 claim_token uuid NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(request_id,kind),
 FOREIGN KEY(request_id,kind) REFERENCES deployment_intents(request_id,kind)
);
CREATE TABLE deployment_receipts(
 request_id text NOT NULL,
 kind text NOT NULL,
 digest text NOT NULL CHECK(digest ~ '^[a-f0-9]{64}$'),
 receipt bytea NOT NULL CHECK(octet_length(receipt) BETWEEN 1 AND 8192),
 recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(request_id,kind),
 FOREIGN KEY(request_id,kind) REFERENCES deployment_sends(request_id,kind)
);
CREATE TABLE deployment_health(
 request_id text NOT NULL,
 kind text NOT NULL,
 digest text NOT NULL CHECK(digest ~ '^[a-f0-9]{64}$'),
 health bytea NOT NULL CHECK(octet_length(health) BETWEEN 1 AND 8192),
 recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(request_id,kind),
 FOREIGN KEY(request_id,kind) REFERENCES deployment_receipts(request_id,kind)
);
ALTER TABLE deployment_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE deployment_requests FORCE ROW LEVEL SECURITY;
CREATE POLICY deployment_owner ON deployment_requests USING(current_user='aisdlc_deployer') WITH CHECK(current_user='aisdlc_deployer');
ALTER TABLE deployment_intents ENABLE ROW LEVEL SECURITY;
ALTER TABLE deployment_intents FORCE ROW LEVEL SECURITY;
CREATE POLICY deployment_read ON deployment_intents FOR SELECT USING(current_user='aisdlc_deployer');
CREATE POLICY deployment_insert ON deployment_intents FOR INSERT WITH CHECK(current_user='aisdlc_deployer' AND EXISTS(SELECT 1 FROM deployment_requests q WHERE q.id=deployment_intents.request_id AND q.state='claimed'));
ALTER TABLE deployment_sends ENABLE ROW LEVEL SECURITY;
ALTER TABLE deployment_sends FORCE ROW LEVEL SECURITY;
CREATE POLICY deployment_read ON deployment_sends FOR SELECT USING(current_user='aisdlc_deployer');
CREATE POLICY deployment_insert ON deployment_sends FOR INSERT WITH CHECK(current_user='aisdlc_deployer' AND EXISTS(SELECT 1 FROM deployment_requests q WHERE q.id=deployment_sends.request_id AND q.state='claimed'));
ALTER TABLE deployment_receipts ENABLE ROW LEVEL SECURITY;
ALTER TABLE deployment_receipts FORCE ROW LEVEL SECURITY;
CREATE POLICY deployment_read ON deployment_receipts FOR SELECT USING(current_user='aisdlc_deployer');
CREATE POLICY deployment_insert ON deployment_receipts FOR INSERT WITH CHECK(current_user='aisdlc_deployer' AND EXISTS(SELECT 1 FROM deployment_requests q WHERE q.id=deployment_receipts.request_id AND q.state='claimed'));
ALTER TABLE deployment_health ENABLE ROW LEVEL SECURITY;
ALTER TABLE deployment_health FORCE ROW LEVEL SECURITY;
CREATE POLICY deployment_read ON deployment_health FOR SELECT USING(current_user='aisdlc_deployer');
CREATE POLICY deployment_insert ON deployment_health FOR INSERT WITH CHECK(current_user='aisdlc_deployer' AND EXISTS(SELECT 1 FROM deployment_requests q WHERE q.id=deployment_health.request_id AND q.state='claimed'));
CREATE POLICY deployment_history_reader ON promotion_requests FOR SELECT USING(current_user='aisdlc_deployer');
CREATE POLICY deployment_history_reader ON promotion_records FOR SELECT USING(current_user='aisdlc_deployer');
CREATE POLICY deployment_history_reader ON provenance_requests FOR SELECT USING(current_user='aisdlc_deployer');
CREATE POLICY deployment_history_reader ON provenance_records FOR SELECT USING(current_user='aisdlc_deployer');
CREATE POLICY deployment_history_reader ON build_requests FOR SELECT USING(current_user='aisdlc_deployer');
CREATE POLICY deployment_history_reader ON built_records FOR SELECT USING(current_user='aisdlc_deployer');
CREATE POLICY deployment_history_reader ON build_slots FOR SELECT USING(current_user='aisdlc_deployer');
CREATE POLICY deployment_history_reader ON build_plans FOR SELECT USING(current_user='aisdlc_deployer');
CREATE POLICY deployment_history_reader ON build_contexts FOR SELECT USING(current_user='aisdlc_deployer');
CREATE POLICY deployment_history_reader ON build_effects FOR SELECT USING(current_user='aisdlc_deployer');
CREATE POLICY deployment_history_reader ON build_observations FOR SELECT USING(current_user='aisdlc_deployer');
CREATE POLICY deployment_history_reader ON merged_records FOR SELECT USING(current_user='aisdlc_deployer');
CREATE POLICY deployment_history_reader ON merge_requests FOR SELECT USING(current_user='aisdlc_deployer');
CREATE POLICY deployment_history_reader ON published_records FOR SELECT USING(current_user='aisdlc_deployer');
CREATE POLICY deployment_history_reader ON publication_requests FOR SELECT USING(current_user='aisdlc_deployer');
CREATE POLICY deployment_history_reader ON intake_heads FOR SELECT USING(current_user='aisdlc_deployer');
CREATE POLICY deployment_history_reader ON intake_revisions FOR SELECT USING(current_user='aisdlc_deployer');
CREATE POLICY deployment_history_reader ON intake_cursors FOR SELECT USING(current_user='aisdlc_deployer');
CREATE POLICY deployment_history_reader ON intake_observations FOR SELECT USING(current_user='aisdlc_deployer');
CREATE POLICY deployment_history_reader ON work_items FOR SELECT USING(current_user='aisdlc_deployer');
REVOKE ALL ON deployment_requests,deployment_intents,deployment_sends,deployment_receipts,deployment_health FROM PUBLIC;
