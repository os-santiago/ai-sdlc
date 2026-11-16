# Inactive deployment authority and durable effects

The deployer journal consumes only a signed `deploy-release` command issued by
release-manager. Its immutable promotion-record hash binds the exact plan,
verified artifact, build, source, issue revision and generation. Its configuration
hash binds installed policies, trust, build profile and original target. The
command ends at the original plan deadline; dispatch never extends that deadline.
Admission reserves fourteen minutes of the fifteen-minute plan for advance,
readiness, health, containment and bounded overhead.

Every authenticated historical reader requires the deployer database identity.
The migrator must explicitly select `ProvisionRolesForDeployment`; ordinary
promotion provisioning removes these optional privileges. Upstream records are
read-only. Schema installation alone does not enable the controller or delivery.

## One attempt and two bounded effects

One promotion can enqueue only one deployment attempt. A durable exclusive claim
cannot be renewed after restart. Before an API call, the controller records the
original snapshot and exact conditional patch digest. Immediately before sending,
the gate rechecks current authority and consumes a unique durable send fence.
After any database wait, and again after that fence commits, it checks current
issue eligibility, installed authority and remaining budget. Lost commit
acknowledgement denies sending; a different process cannot repeat the fence.

An acknowledged API response is an immutable fact bound to the original intent,
send, claim, UID, generation and digest. A policy freeze does not erase that fact.
A matching live image without that acknowledgement is insufficient to adopt the
change or roll it back. Ambiguous API outcomes require quarantine.

A rollback requires the original durable advance acknowledgement and the exact
still-owned candidate generation. It can restore only the independently approved
baseline. Advance freezes or issue withdrawal do not prevent this bounded
containment action, but an expired or replaced installed target does. Rollback
has its own unique intent and send fence. It cannot overwrite a later deployment.

Successful completion requires health evidence bound to the acknowledged
generation, original Service, endpoint owner chain and exact runtime digest.
Candidate success rechecks current issue and policies; containment completion
uses the separate original-target gate. Receipts and health facts cannot be
updated or deleted by the deployer. Expiration or uncertainty quarantines the
attempt instead of issuing new authority.

## Validation boundaries

Tests use real disposable PostgreSQL identities, RLS and transactions. They cover
exclusive claims, immutable facts, lost real COMMIT acknowledgements, restart
replay, denied upstream mutations, freeze/withdrawal, target replacement and
revocation during observed SQL lock waits. Test observations are synthetic and
are not deployment evidence. The separate console adapter fixture uses actual
kind, conditional Kubernetes patches, failed readiness, rollback and TLS.

This increment supplies the journal and shared patch compiler. It does not wire
a running deployer, handoff, GitHub release completion or installation activation.
Those integrations and an actual issue-to-production run remain required.
