-- Only a still-running controller that has not called the Kubernetes operation
-- may seal this fact. NULL UID alone never proves that an effect was not sent.
ALTER TABLE verification_effects ADD COLUMN not_sent_at timestamptz;
ALTER TABLE verification_effects ADD CONSTRAINT verification_effect_not_sent
    CHECK(not_sent_at IS NULL OR (resource_uid IS NULL AND completed_at IS NULL));
