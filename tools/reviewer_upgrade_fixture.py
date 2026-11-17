"""Real contained-review history and restore checks on an owned disposable DB."""
import json
import secrets
import uuid
import time

import reviewer_upgrade_history
from reviewer_image_upgrade import ReviewerImageUpgrade
from runtime_upgrade_backup import Restore
from diagnostic_upgrade_data import Compatibility
import upgrade_backup_data as data
from upgrade_fence_client import Fence


def exercise(database, provider, image, directory, catalog, plan_id, report, save,
             *, fence_image=None, connection=None, port=None):
    # The caller already owns an isolated PostgreSQL CID and its migration
    # fixture. These seeds and negative mutations never target an installation.
    database.sql("UPDATE execution_requests SET expires_at=clock_timestamp()-interval '1 hour';"
                 "UPDATE execution_lifecycles SET cleanup_state='complete';")
    work, allocation, pod, claim = [str(uuid.uuid4()) for _ in range(4)]
    digest, tree = secrets.token_hex(32), secrets.token_hex(32)
    request, verification, review = ['execution-'+digest, 'verification-'+digest, 'review-'+digest]
    database.sql(f"""
INSERT INTO work_items(plan_id,id,uuid,issue_numbers,acceptance_criteria,status)
 VALUES('{plan_id}','contained-review','{work}','[1]','["retain failed review"]','running');
INSERT INTO execution_requests(id,work_item_uuid,attempt,repository,authority_revision,payload_digest,payload,receipt,expires_at,state,stop_reason)
 VALUES('{request}','{work}',1,'fixture/app','{digest}','{digest}',decode('00','hex'),decode('00','hex'),clock_timestamp()-interval '1 hour','stopped','expired');
INSERT INTO execution_lifecycles(request_id,claim_token,claim_until,binding_digest,source_digest,source_bytes,namespace,pod_name,config_name,phase,pod_uid,cleanup_state)
 VALUES('{request}','{claim}',clock_timestamp()-interval '2 hours','{digest}','{digest}',1,'ai-sdlc-executors','executor-{work}-1','assignment-{secrets.token_hex(16)}','candidate-captured','{pod}','complete');
INSERT INTO model_budgets(work_item_uuid,repository,source_sha,policy_digest,model,max_calls,max_input_bytes,max_output_bytes,expires_at,state)
 VALUES('{work}','fixture/app','{'a'*40}','{digest}','fixture',16,2097152,4194304,clock_timestamp()-interval '1 hour','stopped');
INSERT INTO model_allocations(id,work_item_uuid,attempt,pod_uid,max_calls,max_input_bytes,max_output_bytes,response_limit,expires_at)
 VALUES('{allocation}','{work}',1,'{pod}',16,2097152,4194304,262144,clock_timestamp()-interval '1 hour');
INSERT INTO execution_models(request_id,allocation_id,config_digest) VALUES('{request}','{allocation}','{digest}');
INSERT INTO execution_candidates(request_id,allocation_id,pod_uid,source_sha,source_tree_sha256,candidate_tree_sha256,export_sha256,export_bytes,delta_sha256,delta,changes,model_calls)
 VALUES('{request}','{allocation}','{pod}','{'a'*40}','{digest}','{tree}','{digest}',1,'{digest}',decode('00','hex'),1,1);
INSERT INTO verification_requests(id,candidate_id,payload_digest,payload,receipt,expires_at,state,claim_token,claim_until,cleanup_state,proof)
 VALUES('{verification}','{request}','{digest}',decode('00','hex'),decode('00','hex'),clock_timestamp()-interval '1 hour','passed','{claim}',clock_timestamp()-interval '2 hours','complete',decode('00','hex'));
INSERT INTO verification_handoffs(candidate_id,command_id,payload_digest,state) VALUES('{request}','{verification}','{digest}','sent');
INSERT INTO review_runs(id,verification_id,candidate_id,work_item_uuid,behavior_digest,profile_digest,configuration_digest,assignment_digest,expires_at,state,claim_token,claim_until,reason,cleanup_state)
 VALUES('{review}','{verification}','{request}','{work}','{digest}','{digest}','{digest}','{digest}',clock_timestamp()-interval '1 hour','failed','{claim}',clock_timestamp()-interval '2 hours','uncertain','complete');
INSERT INTO review_model_calls(run_id,sequence,token,pod_uid,request_digest,input_bytes,state,response_digest,output_bytes,observation,handed_off_at)
 VALUES('{review}',1,'{str(uuid.uuid4())}','{pod}','{digest}',1,'handed-off','{digest}',1,decode('00','hex'),clock_timestamp()-interval '2 hours');
""")
    reviewer_upgrade_history.quiet(database)
    item = object.__new__(ReviewerImageUpgrade)
    item.catalog = catalog
    item.database = lambda: database
    proof = item.snapshot()
    item.record = {'sourceProof': proof, 'sourceRoleIdentities': Compatibility(database).roles()}
    controls = (
        ("UPDATE work_items SET status='ready' WHERE id='contained-review';", 'queued work'),
        ("UPDATE execution_requests SET expires_at=clock_timestamp()+interval '1 hour';", 'live execution authority'),
        ("UPDATE execution_requests SET claim_token='"+claim+"',claim_until=clock_timestamp()+interval '1 hour';", 'live execution claim'),
        ("UPDATE execution_lifecycles SET cleanup_state='pending';", 'pending executor cleanup'),
        ("UPDATE model_budgets SET state='active';", 'active model budget'),
        ("UPDATE review_runs SET cleanup_state='uncertain';", 'uncertain review cleanup'),
        ("UPDATE review_runs SET expires_at=clock_timestamp()+interval '1 hour',claim_until=clock_timestamp()+interval '30 minutes';", 'live review claim'),
        ("UPDATE review_runs SET state='claimed',reason='';", 'active review'),
        ("UPDATE review_model_calls SET state='completed',handed_off_at=NULL;", 'pending model handoff'),
        ("DELETE FROM verification_handoffs;", 'missing verification handoff'),
        ("UPDATE review_runs SET work_item_uuid=(SELECT uuid FROM work_items WHERE id='retained');", 'foreign review binding'),
        ("UPDATE verification_requests SET expires_at=clock_timestamp()+interval '1 hour';", 'live verification authority'),
        (f"INSERT INTO publication_handoffs(review_id,command_id,payload_digest) VALUES('{review}','publication-{digest}','{digest}');", 'downstream publication intent'),
        (f"INSERT INTO authorized_outbox(producer,event_id,target,operation,receipt,payload) VALUES('worker','fixture-{digest}','orchestrator','fixture','{{}}',decode('00','hex'));", 'pending authenticated delivery'),
        (f"INSERT INTO review_effects(run_id,kind,token,resource_name) VALUES('{review}','export','{str(uuid.uuid4())}','review-fixture');", 'unacknowledged review effect'),
        (f"INSERT INTO verification_effects(request_id,kind,token,resource_name) VALUES('{verification}','tests','{str(uuid.uuid4())}','verify-{digest[:32]}-tests');", 'unacknowledged verification effect'),
    )
    rejected = []
    for mutation, name in controls:
        # Mutation executes successfully before evaluating the read-only oracle;
        # SQL constraint errors cannot masquerade as rejection by the guard.
        sql = reviewer_upgrade_history.QUERY
        result = database.sql('BEGIN;' + mutation + sql + 'ROLLBACK;')
        if result != 'f\n':
            raise ValueError('Reviewer history control was not rejected: ' + name)
        rejected.append(name)
        item.retained()
    artifacts, captured = database.export()
    if captured != proof:
        raise ValueError('Contained-review export changed original history')
    directory.mkdir(mode=0o700)
    backup = {'archives': data.write_archives(directory, artifacts), 'retainedData': proof}
    def persist(record):
        report['reviewerHistoryRestore'] = record.get('restore')
        save()
    restore = Restore(provider, image, directory, backup, persist)
    try:
        restore.start()
        restore.execute()
    finally:
        restore.close()
    item.retained()
    if fence_image is not None:
        try:
            with Fence(provider, fence_image, connection, port, catalog, expires_at=time.time()+90):
                raise AssertionError('Pre-candidate fence accepted captured-review history')
        except ValueError:
            pass
        # The identical SQL must fail without exclusion, and succeed only by
        # catching PostgreSQL lock_not_available while the real fence is held.
        writer = "SET lock_timeout='150ms'; DO $$ BEGIN UPDATE work_items SET status=status; RAISE EXCEPTION 'writer was not excluded'; EXCEPTION WHEN lock_not_available THEN NULL; END $$;"
        try:
            database.sql(writer)
        except ValueError:
            pass
        else:
            raise ValueError('Unfenced writer control did not execute')
        with Fence(provider, fence_image, connection, port, catalog,
                   expires_at=time.time()+90, contained_review=True) as fence:
            fence.check()
            database.sql(writer)
            item.retained()
            fence.check()
        database.sql('UPDATE work_items SET status=status;')
        item.retained()
        pending = str(uuid.uuid4())
        try:
            database.sql(f"INSERT INTO execution_effects(request_id,kind,token) VALUES('{request}','export','{pending}');")
            try:
                with Fence(provider, fence_image, connection, port, catalog,
                           expires_at=time.time()+90, contained_review=True):
                    raise AssertionError('Pending execution effect admitted by reviewer fence')
            except ValueError:
                pass
        finally:
            database.sql(f"DELETE FROM execution_effects WHERE token='{pending}';")
        item.retained()
        report.update(reviewerFenceAcquiredCheckedReleased=True,
            reviewerFenceExcludedWriters=True, reviewerFenceReleasedWriters=True,
            preCandidateFenceRejectedReviewHistory=True,
            reviewerFenceRejectedPendingExecutionEffect=True,
            reviewerFenceImage=fence_image,
            reviewerFenceImageOrigin='locally built candidate helper; not published or installed')
    report.update(reviewerHistoryRestored=True, failedReviewRetained=True,
        passedVerificationRetained=True, capturedCandidateRetained=True,
        reviewerHistoryNegativeControls=rejected,
        reviewerHistoryScope='disposable PostgreSQL; no workflow retry or installed update')
    save()
