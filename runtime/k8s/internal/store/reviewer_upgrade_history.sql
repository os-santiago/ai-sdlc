-- Closed expired/contained reviewer history; never a workflow retry.
SELECT NOT (
 EXISTS(SELECT 1 FROM public.execution_requests WHERE state<>'stopped'
   OR expires_at>clock_timestamp() OR claim_until>clock_timestamp())
 OR EXISTS(SELECT 1 FROM public.execution_lifecycles WHERE cleanup_state<>'complete'
   OR claim_until>clock_timestamp())
 OR EXISTS(SELECT 1 FROM public.model_budgets WHERE state<>'stopped')
 OR EXISTS(SELECT 1 FROM public.model_calls WHERE state NOT IN ('completed','stopped'))
 OR EXISTS(SELECT 1 FROM public.execution_effects WHERE result IS NULL)
 OR EXISTS(SELECT 1 FROM public.verification_effects WHERE completed_at IS NULL AND not_sent_at IS NULL)
 OR EXISTS(SELECT 1 FROM public.review_effects WHERE completed_at IS NULL AND not_sent_at IS NULL)
 OR EXISTS(SELECT 1 FROM public.authorized_outbox WHERE published_at IS NULL AND NOT exhausted)
 OR EXISTS(SELECT 1 FROM public.authorized_inbox WHERE result IS NULL)
 OR EXISTS(SELECT 1 FROM public.verification_requests WHERE state<>'passed'
   OR cleanup_state<>'complete' OR expires_at>clock_timestamp()
   OR claim_until>clock_timestamp())
 OR EXISTS(SELECT 1 FROM public.review_runs WHERE state<>'failed' OR cleanup_state<>'complete'
   OR expires_at>clock_timestamp() OR claim_until>clock_timestamp())
 OR EXISTS(SELECT 1 FROM public.review_model_calls WHERE state<>'handed-off')
 OR EXISTS(SELECT 1 FROM public.execution_candidates c
   JOIN public.execution_requests e ON e.id=c.request_id
   JOIN public.execution_lifecycles l ON l.request_id=c.request_id
   LEFT JOIN public.verification_requests v ON v.candidate_id=c.request_id
   LEFT JOIN public.review_runs r ON r.candidate_id=c.request_id
   WHERE v.id IS NULL OR r.id IS NULL OR r.verification_id IS DISTINCT FROM v.id
     OR r.work_item_uuid IS DISTINCT FROM e.work_item_uuid
     OR l.phase<>'candidate-captured' OR l.pod_uid IS DISTINCT FROM c.pod_uid
     OR NOT EXISTS(SELECT 1 FROM public.verification_handoffs h
       WHERE h.candidate_id=c.request_id AND h.command_id=v.id AND h.state='sent'))
 OR EXISTS(SELECT 1 FROM public.work_items w WHERE w.status<>'failed' AND
   (w.status<>'running' OR NOT EXISTS(SELECT 1 FROM public.execution_requests e
     JOIN public.execution_candidates c ON c.request_id=e.id
     JOIN public.review_runs r ON r.candidate_id=c.request_id WHERE e.work_item_uuid=w.uuid)))
 OR NOT EXISTS(SELECT 1 FROM public.review_runs)
  OR EXISTS(SELECT 1 FROM public.publication_handoffs)
 OR EXISTS(SELECT 1 FROM public.publication_requests)
 OR EXISTS(SELECT 1 FROM public.published_records)
 OR EXISTS(SELECT 1 FROM public.merge_requests)
 OR EXISTS(SELECT 1 FROM public.merged_records)
 OR EXISTS(SELECT 1 FROM public.build_requests)
 OR EXISTS(SELECT 1 FROM public.built_records)
 OR EXISTS(SELECT 1 FROM public.provenance_requests)
 OR EXISTS(SELECT 1 FROM public.provenance_records)
 OR EXISTS(SELECT 1 FROM public.promotion_requests)
 OR EXISTS(SELECT 1 FROM public.promotion_records)
 OR EXISTS(SELECT 1 FROM public.deployment_requests)
 OR EXISTS(SELECT 1 FROM public.deployment_receipts)
 OR EXISTS(SELECT 1 FROM public.deployment_health)
 OR EXISTS(SELECT 1 FROM public.terminal_requests)
 OR EXISTS(SELECT 1 FROM public.terminal_receipts)
 OR EXISTS(SELECT 1 FROM public.production_handoffs)
 OR EXISTS(SELECT 1 FROM public.production_records)
);