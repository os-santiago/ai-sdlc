CREATE TABLE terminal_requests(
 id text PRIMARY KEY CHECK(id ~ '^complete-[a-f0-9]{64}$'),
 deployment_id text NOT NULL UNIQUE REFERENCES deployment_requests(id),
 payload_digest text NOT NULL CHECK(payload_digest ~ '^[a-f0-9]{64}$'),
 payload bytea NOT NULL CHECK(octet_length(payload) BETWEEN 1 AND 4096),
 receipt bytea NOT NULL CHECK(octet_length(receipt) BETWEEN 1 AND 4096),
 configuration bytea NOT NULL CHECK(octet_length(configuration) BETWEEN 1 AND 327680),
 plan bytea NOT NULL CHECK(octet_length(plan) BETWEEN 1 AND 16384),
 expires_at timestamptz NOT NULL,
 state text NOT NULL DEFAULT 'queued' CHECK(state IN('queued','claimed','completed','quarantined')),
 claim_token uuid,
 completed_at timestamptz,
 reason text NOT NULL DEFAULT '' CHECK(reason IN('','expired','revoked','uncertain','rejected')),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK((state='queued' AND claim_token IS NULL) OR state='quarantined' OR (state IN('claimed','completed') AND claim_token IS NOT NULL)),
 CHECK((state='quarantined')=(reason<>'')),
 CHECK((state='completed')=(completed_at IS NOT NULL))
);
CREATE TABLE terminal_intents(
 request_id text NOT NULL REFERENCES terminal_requests(id),
 step text NOT NULL CHECK(step IN('tag','draft','publish','close')),
 claim_token uuid NOT NULL,
 request bytea NOT NULL CHECK(octet_length(request) BETWEEN 1 AND 16384),
 request_digest text NOT NULL CHECK(request_digest ~ '^[a-f0-9]{64}$'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(request_id,step)
);
CREATE TABLE terminal_sends(
 request_id text NOT NULL,
 step text NOT NULL,
 claim_token uuid NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(request_id,step),
 FOREIGN KEY(request_id,step) REFERENCES terminal_intents(request_id,step)
);
CREATE TABLE terminal_receipts(
 request_id text NOT NULL,
 step text NOT NULL,
 digest text NOT NULL CHECK(digest ~ '^[a-f0-9]{64}$'),
 receipt bytea NOT NULL CHECK(octet_length(receipt) BETWEEN 1 AND 8192),
 recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(request_id,step),
 FOREIGN KEY(request_id,step) REFERENCES terminal_sends(request_id,step)
);
ALTER TABLE terminal_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE terminal_requests FORCE ROW LEVEL SECURITY;
CREATE POLICY terminal_owner ON terminal_requests USING(current_user='aisdlc_release_manager') WITH CHECK(current_user='aisdlc_release_manager');
ALTER TABLE terminal_intents ENABLE ROW LEVEL SECURITY;
ALTER TABLE terminal_intents FORCE ROW LEVEL SECURITY;
CREATE POLICY terminal_read ON terminal_intents FOR SELECT USING(current_user='aisdlc_release_manager');
CREATE POLICY terminal_insert ON terminal_intents FOR INSERT WITH CHECK(current_user='aisdlc_release_manager' AND EXISTS(SELECT 1 FROM terminal_requests q WHERE q.id=terminal_intents.request_id AND q.state='claimed'));
ALTER TABLE terminal_sends ENABLE ROW LEVEL SECURITY;
ALTER TABLE terminal_sends FORCE ROW LEVEL SECURITY;
CREATE POLICY terminal_read ON terminal_sends FOR SELECT USING(current_user='aisdlc_release_manager');
CREATE POLICY terminal_insert ON terminal_sends FOR INSERT WITH CHECK(current_user='aisdlc_release_manager' AND EXISTS(SELECT 1 FROM terminal_requests q WHERE q.id=terminal_sends.request_id AND q.state='claimed'));
ALTER TABLE terminal_receipts ENABLE ROW LEVEL SECURITY;
ALTER TABLE terminal_receipts FORCE ROW LEVEL SECURITY;
CREATE POLICY terminal_read ON terminal_receipts FOR SELECT USING(current_user='aisdlc_release_manager');
CREATE POLICY terminal_insert ON terminal_receipts FOR INSERT WITH CHECK(current_user='aisdlc_release_manager' AND EXISTS(SELECT 1 FROM terminal_requests q WHERE q.id=terminal_receipts.request_id AND q.state='claimed'));
REVOKE ALL ON terminal_requests,terminal_intents,terminal_sends,terminal_receipts FROM PUBLIC;
