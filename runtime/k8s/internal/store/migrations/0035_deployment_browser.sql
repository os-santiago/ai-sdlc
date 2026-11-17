-- Browser observations cannot authorize issue input, generation or deployment.
-- Every creation is bound to the existing finite deployment claim and phase.
CREATE TABLE deployment_browser_intents(
 id text PRIMARY KEY CHECK(id ~ '^browser-[a-f0-9]{64}$'),
 deployment_id text NOT NULL REFERENCES deployment_requests(id),
 phase text NOT NULL CHECK(phase IN('baseline','advance','rollback')),
 claim_token uuid NOT NULL,
 request bytea NOT NULL CHECK(octet_length(request) BETWEEN 1 AND 4096),
 request_digest text NOT NULL CHECK(request_digest ~ '^[a-f0-9]{64}$'),
 snapshot bytea NOT NULL CHECK(octet_length(snapshot) BETWEEN 1 AND 4096),
 rollout bytea NOT NULL CHECK(octet_length(rollout) BETWEEN 1 AND 4096),
 profile_digest text NOT NULL CHECK(profile_digest ~ '^[a-f0-9]{64}$'),
 pod_digest text NOT NULL CHECK(pod_digest ~ '^[a-f0-9]{64}$'),
 attempt_nonce text NOT NULL CHECK(attempt_nonce ~ '^[a-f0-9]{64}$'),
 expires_at timestamptz NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(deployment_id,phase),
 CHECK(expires_at>created_at AND expires_at<=created_at+interval '30 seconds')
);
CREATE TABLE deployment_browser_sends(
 id text PRIMARY KEY REFERENCES deployment_browser_intents(id),
 claim_token uuid NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE deployment_browser_pods(
 id text PRIMARY KEY REFERENCES deployment_browser_sends(id),
 pod_uid uuid UNIQUE,
 not_sent boolean NOT NULL DEFAULT false,
 recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK((not_sent AND pod_uid IS NULL) OR (NOT not_sent AND pod_uid IS NOT NULL))
);
CREATE TABLE deployment_browser_evidence(
 id text PRIMARY KEY REFERENCES deployment_browser_pods(id),
 evidence bytea NOT NULL CHECK(octet_length(evidence) BETWEEN 1 AND 8192),
 digest text NOT NULL CHECK(digest ~ '^[a-f0-9]{64}$'),
 recorded_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
ALTER TABLE deployment_browser_intents ENABLE ROW LEVEL SECURITY;
ALTER TABLE deployment_browser_intents FORCE ROW LEVEL SECURITY;
CREATE POLICY browser_read ON deployment_browser_intents FOR SELECT USING(current_user IN('aisdlc_deployer','aisdlc_release_manager','aisdlc_orchestrator'));
CREATE POLICY browser_insert ON deployment_browser_intents FOR INSERT WITH CHECK(current_user='aisdlc_deployer' AND EXISTS(SELECT 1 FROM deployment_requests q WHERE q.id=deployment_browser_intents.deployment_id AND q.claim_token=deployment_browser_intents.claim_token AND q.state='claimed' AND q.expires_at>=deployment_browser_intents.expires_at));
ALTER TABLE deployment_browser_sends ENABLE ROW LEVEL SECURITY;
ALTER TABLE deployment_browser_sends FORCE ROW LEVEL SECURITY;
CREATE POLICY browser_read ON deployment_browser_sends FOR SELECT USING(current_user IN('aisdlc_deployer','aisdlc_release_manager','aisdlc_orchestrator'));
CREATE POLICY browser_insert ON deployment_browser_sends FOR INSERT WITH CHECK(current_user='aisdlc_deployer' AND EXISTS(SELECT 1 FROM deployment_browser_intents i JOIN deployment_requests q ON q.id=i.deployment_id WHERE i.id=deployment_browser_sends.id AND i.claim_token=deployment_browser_sends.claim_token AND q.claim_token=i.claim_token AND q.state='claimed' AND i.expires_at>clock_timestamp()));
ALTER TABLE deployment_browser_pods ENABLE ROW LEVEL SECURITY;
ALTER TABLE deployment_browser_pods FORCE ROW LEVEL SECURITY;
CREATE POLICY browser_read ON deployment_browser_pods FOR SELECT USING(current_user IN('aisdlc_deployer','aisdlc_release_manager','aisdlc_orchestrator'));
CREATE POLICY browser_insert ON deployment_browser_pods FOR INSERT WITH CHECK(current_user='aisdlc_deployer' AND EXISTS(SELECT 1 FROM deployment_browser_intents i JOIN deployment_requests q ON q.id=i.deployment_id WHERE i.id=deployment_browser_pods.id AND q.claim_token=i.claim_token AND q.state='claimed' AND ((deployment_browser_pods.not_sent AND q.expires_at>clock_timestamp()) OR (NOT deployment_browser_pods.not_sent AND i.expires_at>clock_timestamp()))));
ALTER TABLE deployment_browser_evidence ENABLE ROW LEVEL SECURITY;
ALTER TABLE deployment_browser_evidence FORCE ROW LEVEL SECURITY;
CREATE POLICY browser_read ON deployment_browser_evidence FOR SELECT USING(current_user IN('aisdlc_deployer','aisdlc_release_manager','aisdlc_orchestrator'));
CREATE POLICY browser_insert ON deployment_browser_evidence FOR INSERT WITH CHECK(current_user='aisdlc_deployer' AND EXISTS(SELECT 1 FROM deployment_browser_intents i JOIN deployment_requests q ON q.id=i.deployment_id WHERE i.id=deployment_browser_evidence.id AND EXISTS(SELECT 1 FROM deployment_browser_pods p WHERE p.id=i.id AND NOT p.not_sent) AND q.claim_token=i.claim_token AND q.state='claimed' AND i.expires_at>clock_timestamp()));
REVOKE ALL ON deployment_browser_intents,deployment_browser_sends,deployment_browser_pods,deployment_browser_evidence FROM PUBLIC;

-- Containment is independent of the expired smoke. It records absence of only
-- the originally acknowledged Pod; it cannot create browser/health evidence.
CREATE TABLE deployment_browser_cleanups(
 id text PRIMARY KEY REFERENCES deployment_browser_pods(id),
 pod_uid uuid NOT NULL,
 recorded_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
ALTER TABLE deployment_browser_cleanups ENABLE ROW LEVEL SECURITY;
ALTER TABLE deployment_browser_cleanups FORCE ROW LEVEL SECURITY;
CREATE POLICY browser_cleanup_read ON deployment_browser_cleanups FOR SELECT USING(current_user IN('aisdlc_deployer','aisdlc_release_manager','aisdlc_orchestrator'));
CREATE POLICY browser_cleanup_insert ON deployment_browser_cleanups FOR INSERT WITH CHECK(current_user='aisdlc_deployer' AND EXISTS(SELECT 1 FROM deployment_browser_pods p JOIN deployment_browser_intents i ON i.id=p.id WHERE p.id=deployment_browser_cleanups.id AND NOT p.not_sent AND p.pod_uid=deployment_browser_cleanups.pod_uid AND i.expires_at<=clock_timestamp()));
REVOKE ALL ON deployment_browser_cleanups FROM PUBLIC;
