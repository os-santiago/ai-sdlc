-- Private historical feedback, not permission to repair or publish.
CREATE TABLE review_feedback (
 review_id text PRIMARY KEY REFERENCES review_runs(id),
 decision bytea NOT NULL CHECK(octet_length(decision) BETWEEN 1 AND 32768),
 digest text NOT NULL CHECK(digest ~ '^[a-f0-9]{64}$'),
 recorded_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
ALTER TABLE review_feedback ENABLE ROW LEVEL SECURITY;
ALTER TABLE review_feedback FORCE ROW LEVEL SECURITY;
CREATE POLICY review_feedback_reader ON review_feedback FOR SELECT USING(current_user='aisdlc_verifier');
CREATE POLICY review_feedback_writer ON review_feedback FOR INSERT WITH CHECK(current_user='aisdlc_verifier' AND EXISTS (SELECT 1 FROM review_runs r WHERE r.id=review_id AND r.state='claimed' AND r.cleanup_state='complete' AND r.claim_until>clock_timestamp() AND r.expires_at>clock_timestamp()));
REVOKE ALL ON review_feedback FROM PUBLIC;
