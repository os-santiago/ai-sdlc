-- Inactive until a separately reviewed installation enables merge authority.
-- Historical publication leases are never extended or adopted here.
CREATE TABLE merge_requests (
    id text PRIMARY KEY CHECK(id ~ '^merge-[a-f0-9]{64}$'),
    publication_id text NOT NULL UNIQUE REFERENCES published_records(publication_id),
    record_digest text NOT NULL CHECK(record_digest ~ '^[a-f0-9]{64}$'),
    gate_digest text NOT NULL CHECK(gate_digest ~ '^[a-f0-9]{64}$'),
    authority_revision text NOT NULL CHECK(authority_revision ~ '^[a-f0-9]{64}$'),
    payload_digest text NOT NULL CHECK(payload_digest ~ '^[a-f0-9]{64}$'),
    payload bytea NOT NULL CHECK(octet_length(payload) BETWEEN 1 AND 16384),
    receipt bytea NOT NULL CHECK(octet_length(receipt) BETWEEN 1 AND 4096),
    gate bytea NOT NULL CHECK(octet_length(gate) BETWEEN 1 AND 16384),
    policy bytea NOT NULL CHECK(octet_length(policy) BETWEEN 1 AND 32768),
    expires_at timestamptz NOT NULL,
    state text NOT NULL DEFAULT 'queued' CHECK(state IN ('queued','claimed','merged','failed')),
    claim_token uuid,
    claim_until timestamptz,
    reason text NOT NULL DEFAULT '' CHECK(reason IN ('','expired','revoked','uncertain','rejected')),
    proof bytea CHECK(proof IS NULL OR octet_length(proof) BETWEEN 1 AND 16384),
    merged_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK((claim_token IS NULL)=(claim_until IS NULL)),
    CHECK(state<>'queued' OR claim_token IS NULL),
    CHECK(state NOT IN ('claimed','merged') OR claim_token IS NOT NULL),
    CHECK(claim_until IS NULL OR claim_until<=expires_at),
    CHECK((state='merged')=(proof IS NOT NULL)),
    CHECK((state='merged')=(merged_at IS NOT NULL)),
    CHECK(merged_at IS NULL OR merged_at<expires_at),
    CHECK((state='failed')=(reason<>''))
);
CREATE TABLE merge_effects (
    request_id text PRIMARY KEY REFERENCES merge_requests(id),
    token uuid NOT NULL UNIQUE,
    request_digest text NOT NULL CHECK(request_digest ~ '^[a-f0-9]{64}$'),
    receipt bytea CHECK(receipt IS NULL OR octet_length(receipt) BETWEEN 1 AND 4096),
    receipt_digest text CHECK(receipt_digest ~ '^[a-f0-9]{64}$'),
    not_sent_at timestamptz,
    completed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK((receipt IS NULL)=(receipt_digest IS NULL)),
    CHECK((receipt IS NULL)=(completed_at IS NULL)),
    CHECK(not_sent_at IS NULL OR completed_at IS NULL)
);
ALTER TABLE merge_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE merge_requests FORCE ROW LEVEL SECURITY;
CREATE POLICY merge_request_owner ON merge_requests USING(current_user='aisdlc_release_manager') WITH CHECK(current_user='aisdlc_release_manager');
ALTER TABLE merge_effects ENABLE ROW LEVEL SECURITY;
ALTER TABLE merge_effects FORCE ROW LEVEL SECURITY;
CREATE POLICY merge_effect_owner ON merge_effects USING(current_user='aisdlc_release_manager') WITH CHECK(current_user='aisdlc_release_manager');
REVOKE ALL ON merge_requests,merge_effects FROM PUBLIC;
CREATE TABLE merged_records (
    merge_id text PRIMARY KEY REFERENCES merge_requests(id),
    payload_digest text NOT NULL CHECK(payload_digest ~ '^[a-f0-9]{64}$'),
    payload bytea NOT NULL CHECK(octet_length(payload) BETWEEN 1 AND 16384),
    receipt bytea NOT NULL CHECK(octet_length(receipt) BETWEEN 1 AND 4096),
    gate bytea NOT NULL CHECK(octet_length(gate) BETWEEN 1 AND 16384),
    policy bytea NOT NULL CHECK(octet_length(policy) BETWEEN 1 AND 32768),
    proof_digest text NOT NULL CHECK(proof_digest ~ '^[a-f0-9]{64}$'),
    proof bytea NOT NULL CHECK(octet_length(proof) BETWEEN 1 AND 16384),
    effect_digest text NOT NULL CHECK(effect_digest ~ '^[a-f0-9]{64}$'),
    effect bytea NOT NULL CHECK(octet_length(effect) BETWEEN 1 AND 8192),
    authority_expires_at timestamptz NOT NULL,
    merged_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK(merged_at<authority_expires_at)
);
ALTER TABLE merged_records ENABLE ROW LEVEL SECURITY;
ALTER TABLE merged_records FORCE ROW LEVEL SECURITY;
CREATE POLICY merged_record_reader ON merged_records FOR SELECT USING(current_user='aisdlc_release_manager');
CREATE POLICY merged_record_writer ON merged_records FOR INSERT WITH CHECK(
    current_user='aisdlc_release_manager' AND EXISTS(
        SELECT 1 FROM merge_requests r WHERE r.id=merged_records.merge_id
          AND r.state='claimed' AND r.claim_until>clock_timestamp()
          AND r.expires_at>clock_timestamp() AND r.expires_at=merged_records.authority_expires_at
          AND r.payload_digest=merged_records.payload_digest
          AND r.payload=merged_records.payload AND r.receipt=merged_records.receipt
          AND r.policy=merged_records.policy AND r.gate=merged_records.gate
          AND merged_records.merged_at>=r.created_at
    )
);
REVOKE ALL ON merged_records FROM PUBLIC;
