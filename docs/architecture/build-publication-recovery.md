# Ordered build publication and recovery

This increment extends the inactive build journal. It does not create Kubernetes
workloads, verify signatures, promote images, or deploy an application.

The owning controller must compile a closed plan and persist the exact build
and publication request hashes before sending either request. The plan binds the
actual merged source/tree, signed profile, physical slot custody, definition,
pinned publisher and fixed destination. Publication requires the original build
UID and a fresh terminal observation. Build and publisher identities, container
identities and temporal order must remain distinct and consistent.

The normalized terminal observation is a storage contract. A future trusted
transport must first prove complete admitted Pod/spec/status coverage, original
PipelineRun/TaskRun/Pod ownership, pinned helper images, no restarts, every init
and regular container terminated successfully, and no remaining writer. The
journal's scalar validation cannot establish those Kubernetes facts by itself.

The resulting immutable fact is readable by the independent verifier. Its source,
plan, definition, original UIDs and observation hashes remain bound to the signed
request and its original deadline. `releaseAuthorized` must be false. Publication
success is not cryptographic provenance verification or permission to release.

## Recovery

Recovery atomically stops an interrupted active claim before returning its
identity. It also discovers completed builds whose resources still need cleanup.
Cleanup reads original acknowledged UIDs without consulting current issue or
policy eligibility, allowing recovery after a freeze. It does not return unknown
UIDs for name-based adoption. Unknown creates remain quarantined.

After the future UID-bound transport proves absence of every known resource,
the controller records an immutable cleanup acknowledgement. This prevents a
previously handled request from blocking subsequent recovery selection. Cleanup
never removes the physical-slot consumption record. Removing database credentials
is an installer operation that must complete cleanup before disabling that access.

## Evidence and limits

Real PostgreSQL tests exercise dropped server COMMIT acknowledgements at claim,
build intention, UID acknowledgement, publication intention, completion and
cleanup. They also cover historical deadline tampering, original UID replacement,
revocation, publication before writer termination, replay, immutable records and
independent-reader privileges. These tests use synthetic Kubernetes observations;
they do not prove execution or isolation of a Kubernetes build.

The first full run exposed an invalid negative-test value equal to the fixture's
actual tree and was corrected. A later run lost its disposable PostgreSQL
connection after the new tests passed; the cause was not captured before cleanup.
The validation harness now bounds WAL within its existing 512 MiB tmpfs and records
allowlisted container diagnostics without exposing SQL logs or credentials.
