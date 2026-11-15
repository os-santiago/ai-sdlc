-- Provider completion is not confirmation of publication to the executor.
-- Preserve response metadata when a completed, undelivered result is abandoned.
ALTER TABLE model_calls ADD COLUMN response_handed_off_at timestamptz;
ALTER TABLE model_calls DROP CONSTRAINT model_calls_check1;
ALTER TABLE model_calls ADD CONSTRAINT model_calls_response_metadata CHECK (
    (output_bytes IS NULL AND response_sha256 IS NULL AND response_status IS NULL AND state <> 'completed')
    OR
    (output_bytes IS NOT NULL AND response_sha256 IS NOT NULL AND response_status IS NOT NULL AND state IN ('completed','ambiguous'))
);
ALTER TABLE model_calls ADD CONSTRAINT model_calls_handoff_completed CHECK (
    response_handed_off_at IS NULL OR state = 'completed'
);
CREATE INDEX model_calls_handoff_expiring ON model_calls(expires_at)
    WHERE state = 'completed' AND response_handed_off_at IS NULL;
