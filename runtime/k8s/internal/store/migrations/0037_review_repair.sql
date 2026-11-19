-- One original issue may derive one repair. Original candidate, review,
-- allocation, budget counters and deadlines remain immutable historical facts.
ALTER TABLE execution_requests DROP CONSTRAINT execution_requests_stop_reason_check;
ALTER TABLE execution_requests ADD CONSTRAINT execution_requests_stop_reason_check
 CHECK(stop_reason IN ('','aborted','expired','authority-revoked','uncertain','candidate-captured'));

CREATE TABLE review_repair_handoffs (
    review_id text PRIMARY KEY REFERENCES review_feedback(review_id),
    work_item_uuid uuid NOT NULL REFERENCES work_items(uuid),
    outcome text NOT NULL CHECK(outcome IN ('forwarded','not-authorized','expired','exhausted','stale','terminal-rejection')),
    payload bytea,
    payload_digest text,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK((outcome='forwarded' AND payload IS NOT NULL AND payload_digest IS NOT NULL)
       OR (outcome<>'forwarded' AND payload IS NULL AND payload_digest IS NULL)),
    CHECK(payload IS NULL OR octet_length(payload) BETWEEN 1 AND 16384),
    CHECK(payload_digest IS NULL OR payload_digest ~ '^[a-f0-9]{64}$')
);
CREATE UNIQUE INDEX review_repair_one_handoff ON review_repair_handoffs(work_item_uuid) WHERE outcome='forwarded';

CREATE TABLE review_repair_stops (
 review_id text PRIMARY KEY REFERENCES review_repair_handoffs(review_id),
 work_item_uuid uuid NOT NULL REFERENCES work_items(uuid),
 payload bytea NOT NULL CHECK(octet_length(payload) BETWEEN 1 AND 4096),
 payload_digest text NOT NULL CHECK(payload_digest ~ '^[a-f0-9]{64}$'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
ALTER TABLE review_repair_stops ENABLE ROW LEVEL SECURITY;
ALTER TABLE review_repair_stops FORCE ROW LEVEL SECURITY;
CREATE POLICY repair_stop_reader ON review_repair_stops FOR SELECT USING(current_user IN ('aisdlc_verifier','aisdlc_orchestrator'));
CREATE POLICY repair_stop_writer ON review_repair_stops FOR INSERT WITH CHECK(current_user='aisdlc_verifier');
REVOKE ALL ON review_repair_stops FROM PUBLIC;

CREATE TABLE execution_repairs (
    work_item_uuid uuid PRIMARY KEY REFERENCES model_budgets(work_item_uuid),
    parent_review_id text NOT NULL UNIQUE REFERENCES review_repair_handoffs(review_id),
    parent_candidate_id text NOT NULL UNIQUE REFERENCES execution_candidates(request_id),
    parent_allocation_id uuid NOT NULL REFERENCES model_allocations(id),
    child_request_id text NOT NULL UNIQUE CHECK(child_request_id ~ '^execution-[a-f0-9]{64}$'),
    child_allocation_id uuid NOT NULL UNIQUE,
    handoff_digest text NOT NULL CHECK(handoff_digest ~ '^[a-f0-9]{64}$'),
    child_payload bytea NOT NULL CHECK(octet_length(child_payload) BETWEEN 1 AND 16384),
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE(work_item_uuid,child_allocation_id),
    CHECK(expires_at>created_at)
);

-- The orchestrator inserts the exact derived allowance once. The worker can
-- consume/stop it, but cannot replace its allocation, limits or expiry.
CREATE TABLE repair_model_budgets (
    work_item_uuid uuid PRIMARY KEY REFERENCES execution_repairs(work_item_uuid),
    allocation_id uuid NOT NULL UNIQUE,
    max_calls integer NOT NULL CHECK(max_calls BETWEEN 1 AND 32),
    max_input_bytes bigint NOT NULL CHECK(max_input_bytes BETWEEN 1 AND 4194304),
    max_output_bytes bigint NOT NULL CHECK(max_output_bytes BETWEEN 1 AND 8388608),
    response_limit integer NOT NULL CHECK(response_limit BETWEEN 1 AND 262144),
    reserved_calls integer NOT NULL DEFAULT 0 CHECK(reserved_calls BETWEEN 0 AND max_calls),
    reserved_input_bytes bigint NOT NULL DEFAULT 0 CHECK(reserved_input_bytes BETWEEN 0 AND max_input_bytes),
    reserved_output_bytes bigint NOT NULL DEFAULT 0 CHECK(reserved_output_bytes BETWEEN 0 AND max_output_bytes),
    expires_at timestamptz NOT NULL,
    state text NOT NULL DEFAULT 'reserved' CHECK(state IN ('reserved','active','stopped')),
    stop_reason text NOT NULL DEFAULT '' CHECK(length(stop_reason)<=64),
    FOREIGN KEY(work_item_uuid,allocation_id) REFERENCES execution_repairs(work_item_uuid,child_allocation_id),
    CHECK(max_output_bytes>=response_limit)
);

ALTER TABLE review_repair_handoffs ENABLE ROW LEVEL SECURITY;
ALTER TABLE review_repair_handoffs FORCE ROW LEVEL SECURITY;
CREATE POLICY repair_handoff_reader ON review_repair_handoffs FOR SELECT
    USING(current_user IN ('aisdlc_verifier','aisdlc_orchestrator','aisdlc_worker'));
CREATE POLICY repair_handoff_writer ON review_repair_handoffs FOR INSERT
    WITH CHECK(current_user='aisdlc_verifier');
ALTER TABLE execution_repairs ENABLE ROW LEVEL SECURITY;
ALTER TABLE execution_repairs FORCE ROW LEVEL SECURITY;
CREATE POLICY repair_lineage_reader ON execution_repairs FOR SELECT
    USING(current_user IN ('aisdlc_orchestrator','aisdlc_worker','aisdlc_verifier','aisdlc_release_manager'));
CREATE POLICY repair_lineage_writer ON execution_repairs FOR INSERT
    WITH CHECK(current_user='aisdlc_orchestrator');
ALTER TABLE repair_model_budgets ENABLE ROW LEVEL SECURITY;
ALTER TABLE repair_model_budgets FORCE ROW LEVEL SECURITY;
CREATE POLICY repair_budget_reader ON repair_model_budgets FOR SELECT
    USING(current_user IN ('aisdlc_orchestrator','aisdlc_worker','aisdlc_verifier','aisdlc_release_manager'));
CREATE POLICY repair_budget_writer ON repair_model_budgets FOR INSERT
    WITH CHECK(current_user='aisdlc_orchestrator');
CREATE POLICY repair_budget_consumer ON repair_model_budgets FOR UPDATE
    USING(current_user='aisdlc_worker') WITH CHECK(current_user='aisdlc_worker');
REVOKE ALL ON review_repair_handoffs,execution_repairs,repair_model_budgets FROM PUBLIC;

-- PostgreSQL row locking requires UPDATE privilege. This deliberately narrow
-- helper acquires only a sealed parent lock; it cannot mutate or expose data.
-- Lineage, current issue and authority are checked by the surrounding inbox
-- transaction after its waits. The caller must retain its original deadline.
CREATE FUNCTION public.lock_repair_parent_budget(parent_work uuid) RETURNS void
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
    PERFORM work_item_uuid FROM public.model_budgets
      WHERE work_item_uuid=parent_work AND state='stopped' AND stop_reason='candidate-captured'
      FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'Repair parent budget unavailable' USING ERRCODE='55000';
    END IF;
    -- Terminal containment takes the same parent -> child -> work order as
    -- model reservation. This helper still cannot update any ledger row.
    PERFORM work_item_uuid FROM public.repair_model_budgets WHERE work_item_uuid=parent_work FOR UPDATE;
END;
$$;
REVOKE ALL ON FUNCTION public.lock_repair_parent_budget(uuid) FROM PUBLIC;

-- Only read access is added to authenticated historical evidence. No new
-- consumer receives permission to edit the parent ledger or a review.
CREATE POLICY repair_orchestrator_reader ON execution_requests FOR SELECT USING(current_user='aisdlc_orchestrator');
CREATE POLICY repair_orchestrator_reader ON execution_lifecycles FOR SELECT USING(current_user='aisdlc_orchestrator');
CREATE POLICY repair_orchestrator_reader ON execution_models FOR SELECT USING(current_user='aisdlc_orchestrator');
CREATE POLICY repair_orchestrator_reader ON execution_candidates FOR SELECT USING(current_user='aisdlc_orchestrator');
CREATE POLICY repair_orchestrator_reader ON model_allocations FOR SELECT USING(current_user='aisdlc_orchestrator');
CREATE POLICY repair_orchestrator_reader ON model_budgets FOR SELECT USING(current_user='aisdlc_orchestrator');
CREATE POLICY repair_orchestrator_reader ON verification_requests FOR SELECT USING(current_user='aisdlc_orchestrator');
CREATE POLICY repair_orchestrator_reader ON review_runs FOR SELECT USING(current_user='aisdlc_orchestrator');
CREATE POLICY repair_orchestrator_reader ON review_effects FOR SELECT USING(current_user='aisdlc_orchestrator');
CREATE POLICY repair_orchestrator_reader ON review_model_calls FOR SELECT USING(current_user='aisdlc_orchestrator');
CREATE POLICY repair_orchestrator_reader ON review_feedback FOR SELECT USING(current_user='aisdlc_orchestrator');
CREATE POLICY repair_orchestrator_reader ON intake_heads FOR SELECT USING(current_user='aisdlc_orchestrator');
CREATE POLICY repair_orchestrator_reader ON intake_revisions FOR SELECT USING(current_user='aisdlc_orchestrator');
CREATE POLICY repair_orchestrator_reader ON intake_cursors FOR SELECT USING(current_user='aisdlc_orchestrator');
CREATE POLICY repair_orchestrator_reader ON intake_observations FOR SELECT USING(current_user='aisdlc_orchestrator');
CREATE POLICY repair_orchestrator_reader ON admission_heads FOR SELECT USING(current_user='aisdlc_orchestrator');
CREATE POLICY repair_orchestrator_reader ON admissions FOR SELECT USING(current_user='aisdlc_orchestrator');
CREATE POLICY repair_orchestrator_reader ON publication_handoffs FOR SELECT USING(current_user='aisdlc_orchestrator');
CREATE POLICY repair_capacity_reader ON model_calls FOR SELECT USING(current_user IN ('aisdlc_orchestrator','aisdlc_verifier'));
CREATE POLICY repair_worker_feedback_reader ON review_feedback FOR SELECT USING(current_user='aisdlc_worker'
    AND EXISTS(SELECT 1 FROM public.execution_repairs r WHERE r.parent_review_id=review_id));
