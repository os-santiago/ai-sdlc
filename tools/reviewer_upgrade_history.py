"""A contained failed review may be retained, never retried by the installer."""

# These are root journals. Their dependent records have foreign keys; build
# slots are reserved installation configuration, not evidence of executed work.
DOWNSTREAM = ('publication_handoffs', 'publication_requests', 'published_records',
              'merge_requests', 'merged_records', 'build_requests', 'built_records',
              'provenance_requests', 'provenance_records', 'promotion_requests',
              'promotion_records', 'deployment_requests', 'deployment_receipts',
              'deployment_health', 'terminal_requests', 'terminal_receipts',
              'production_handoffs', 'production_records')

QUERY = """SELECT NOT (
 EXISTS(SELECT 1 FROM execution_requests WHERE state<>'stopped'
   OR expires_at>clock_timestamp() OR claim_until>clock_timestamp())
 OR EXISTS(SELECT 1 FROM execution_lifecycles WHERE cleanup_state<>'complete'
   OR claim_until>clock_timestamp())
 OR EXISTS(SELECT 1 FROM model_budgets WHERE state<>'stopped')
 OR EXISTS(SELECT 1 FROM model_calls WHERE state IN ('reserved','started'))
 OR EXISTS(SELECT 1 FROM authorized_outbox WHERE published_at IS NULL AND NOT exhausted)
 OR EXISTS(SELECT 1 FROM authorized_inbox WHERE result IS NULL)
 OR EXISTS(SELECT 1 FROM verification_requests WHERE state<>'passed'
   OR cleanup_state<>'complete' OR expires_at>clock_timestamp()
   OR claim_until>clock_timestamp())
 OR EXISTS(SELECT 1 FROM review_runs WHERE state<>'failed' OR cleanup_state<>'complete'
   OR expires_at>clock_timestamp() OR claim_until>clock_timestamp())
 OR EXISTS(SELECT 1 FROM review_model_calls WHERE state<>'handed-off')
 OR EXISTS(SELECT 1 FROM execution_candidates c
   JOIN execution_requests e ON e.id=c.request_id
   JOIN execution_lifecycles l ON l.request_id=c.request_id
   LEFT JOIN verification_requests v ON v.candidate_id=c.request_id
   LEFT JOIN review_runs r ON r.candidate_id=c.request_id
   WHERE v.id IS NULL OR r.id IS NULL OR r.verification_id IS DISTINCT FROM v.id
     OR r.work_item_uuid IS DISTINCT FROM e.work_item_uuid
     OR l.phase<>'candidate-captured' OR l.pod_uid IS DISTINCT FROM c.pod_uid
     OR NOT EXISTS(SELECT 1 FROM verification_handoffs h
       WHERE h.candidate_id=c.request_id AND h.command_id=v.id AND h.state='sent'))
 OR EXISTS(SELECT 1 FROM work_items w WHERE w.status<>'failed' AND
   (w.status<>'running' OR NOT EXISTS(SELECT 1 FROM execution_requests e
     JOIN execution_candidates c ON c.request_id=e.id
     JOIN review_runs r ON r.candidate_id=c.request_id WHERE e.work_item_uuid=w.uuid)))
 OR NOT EXISTS(SELECT 1 FROM review_runs)
 {downstream}
);"""


def quiet(database):
    suffix = ''.join(' OR EXISTS(SELECT 1 FROM ' + name + ')' for name in DOWNSTREAM)
    if database.sql(QUERY.format(downstream=suffix)) != 't\n':
        raise ValueError('Reviewer update requires expired, cleaned failed review history and no downstream work')
