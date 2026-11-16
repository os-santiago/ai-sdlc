# Recover an interrupted deployment without replay

The deployer takes a dedicated PostgreSQL session lock before reconciliation.
A competing replica does no work. The lock is released explicitly; an uncertain
acquire/unlock discards the physical connection. Loss of that original backend
cancels the active context. Every API effect still requires the original journal
claim, send fence and deadline; the session lock is not execution authority.

Recovery runs before new work and before loading current advance policy. It
selects only an existing claimed request and retains its original token and
expiry. It authenticates its original signed command, upstream history, current
unchanged target, recorded intent, send and exact API acknowledgement.

| Interrupted point | Recovery |
| --- | --- |
| Claim or advance intent without a durable API ACK | Quarantine; no API operation |
| Send with lost API ACK | Quarantine; never infer success from current image |
| Durable advance ACK, no rollback intent | Contain the acknowledged generation by conditional rollback |
| Rollback intent/send without durable rollback ACK | Quarantine; never send again |
| Durable rollback ACK, completion absent | Recheck exact rollout and TLS health, record completion; no additional PATCH |
| Original deadline or approved target expired | Quarantine; no authority extension |
| Target replaced or workload generation changed | Reject containment; no adoption |
| Completed or quarantined request | No replay |

A restart never resumes candidate advancement. An acknowledged candidate is
rolled back even if it might be healthy; this conservative policy provides a
bounded recovery path while preserving the original effect's custody. Current
advance-policy removal, freeze or issue withdrawal cannot grant work and does
not revoke already-authorized containment. Target removal/change/expiry, database
permission revocation and Kubernetes permission revocation remain stops.

Recovery does not claim that all ambiguous failures can be repaired
autonomously. A missing API ACK cannot be manufactured from an observed image.
An uncertain or expired outcome requires separate operator incident handling;
there is no HTTP endpoint to bypass the journal or manually advance a stage.

Validation uses actual PostgreSQL for twelve crash-point scenarios, exact
claim/expiry preservation, two-controller exclusion and termination of the
original database backend. Controller instances are reconstructed and API
observations are synthetic in these tests: they are not a Kubernetes Pod-restart
or autonomous E2E demonstration. The separate disposable kind fixture exercises
actual conditional patch, rollout failure, rollback and TLS with the installed
restricted ServiceAccount.

The expiry fixture changes the isolated database timestamp using its test administrator; it is not a wall-clock expiry experiment. The adapter's separate delayed-TLS-ACK test verifies that an actual RPC stops at its original caller deadline.
