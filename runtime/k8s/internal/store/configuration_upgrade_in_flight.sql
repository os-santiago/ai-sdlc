-- Configuration-only transitions (no migration) admit every completed record
-- of the exact current catalog. They are refused while model-dependent work is
-- in flight: an executing candidate, an open model budget or call, an unclean
-- execution, a claimed or uncleaned verification or review, a started review
-- model call, or an unresolved verification/review effect. Restarted
-- components resume everything else with their normal recovery.
SELECT
  EXISTS(SELECT 1 FROM public.execution_requests WHERE state='claimed')
  OR EXISTS(SELECT 1 FROM public.execution_lifecycles l JOIN public.execution_requests r ON r.id=l.request_id WHERE r.state<>'stopped' OR l.cleanup_state<>'complete')
  OR EXISTS(SELECT 1 FROM public.execution_effects WHERE result IS NULL)
  OR EXISTS(SELECT 1 FROM public.model_budgets WHERE state<>'stopped')
  OR EXISTS(SELECT 1 FROM public.model_calls WHERE state NOT IN('completed','stopped'))
  OR EXISTS(SELECT 1 FROM public.verification_requests WHERE state='claimed' OR (state IN('passed','failed') AND cleanup_state<>'complete'))
  OR EXISTS(SELECT 1 FROM public.verification_effects WHERE completed_at IS NULL AND not_sent_at IS NULL)
  OR EXISTS(SELECT 1 FROM public.review_runs WHERE state='claimed' OR (state IN('passed','failed') AND cleanup_state<>'complete'))
  OR EXISTS(SELECT 1 FROM public.review_model_calls WHERE state='started')
  OR EXISTS(SELECT 1 FROM public.review_effects WHERE completed_at IS NULL AND not_sent_at IS NULL)
