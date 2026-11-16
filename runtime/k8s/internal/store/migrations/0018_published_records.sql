-- Terminal publication evidence is a fact, never a renewed execution lease.
-- Existing rows are deliberately not backfilled from mutable journal state.
CREATE TABLE published_records (
    publication_id text PRIMARY KEY REFERENCES publication_requests(id),
    payload_digest text NOT NULL CHECK(payload_digest ~ '^[a-f0-9]{64}$'),
    payload bytea NOT NULL CHECK(octet_length(payload) BETWEEN 1 AND 16384),
    receipt bytea NOT NULL CHECK(octet_length(receipt) BETWEEN 1 AND 4096),
    proof_digest text NOT NULL CHECK(proof_digest ~ '^[a-f0-9]{64}$'),
    proof bytea NOT NULL CHECK(octet_length(proof) BETWEEN 1 AND 32768),
    effects_digest text NOT NULL CHECK(effects_digest ~ '^[a-f0-9]{64}$'),
    effects bytea NOT NULL CHECK(octet_length(effects) BETWEEN 1 AND 32768),
    authority_expires_at timestamptz NOT NULL,
    published_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK(published_at < authority_expires_at)
);
ALTER TABLE published_records ENABLE ROW LEVEL SECURITY;
ALTER TABLE published_records FORCE ROW LEVEL SECURITY;
CREATE POLICY published_record_reader ON published_records FOR SELECT USING(current_user='aisdlc_release_manager');
CREATE POLICY published_record_writer ON published_records FOR INSERT WITH CHECK(
    current_user='aisdlc_release_manager' AND EXISTS (
        SELECT 1 FROM publication_requests r WHERE r.id=published_records.publication_id
          AND r.state='claimed' AND r.claim_until>clock_timestamp()
          AND r.expires_at>clock_timestamp()
          AND r.expires_at=published_records.authority_expires_at
          AND r.payload_digest=published_records.payload_digest
          AND r.payload=published_records.payload AND r.receipt=published_records.receipt
          AND published_records.published_at>=r.commit_time
    )
);
REVOKE ALL ON published_records FROM PUBLIC;
