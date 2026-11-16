# Authenticated merge observations

`internal/mergegate` is an inactive read-only collector for the Homedir pilot.
It neither exposes an input endpoint nor authorizes or performs a merge. A
consumer must first validate a durable publication record with independent
behavior/review evidence, acquire fresh bounded merge authority and validate the
current issue and installation policy. Historical generation leases are not
renewed. `MergeAuthorized` remains false even when the forge observation is ready.

The collector binds the PR number, head, base, branch and Git tree to expected
publication identity. It reads GitHub through fixed authenticated API endpoints
and a fixed GraphQL query. The installed ruleset must apply to main, retain the
configured context/application floor, enforce up-to-date required checks and
review-thread resolution, and have no bypass actors. The pilot floor contains
Quality Summary, CI Summary and Quality Gate Summary from integration 15368.
Zero native approvals is compatible with the operator-authorized low-risk
independent agent review; it does not remove the upstream audit requirement.

Every reported check/status must be complete and nonfailing; required checks
must conclude success from their configured application. Duplicate names from
one application are acceptable only when every observed duplicate satisfies
those conditions. Cross-application collisions are ambiguous. This deliberately
does not select a convenient latest result or erase historical failures.
Active changes-requested reviews and unresolved threads block readiness.
Approval on a different head cannot clear an active request for changes.

Collection is time-bounded and rejects redirects, proxies, oversized or
ambiguous responses, incomplete pagination and identity changes. Mutable
protection, checks/statuses, reviews/threads and PR identity are reread before
returning. This detects changes during collection; it is not an atomic lock on
GitHub. A future writer must use the exact-head precondition and rely on native
protection without administrative bypass at the actual mutation boundary.

The supported bound is 100 check runs, statuses, reviews and threads per query,
16 required check contexts and one explicitly configured repository ruleset.
Larger/ambiguous observations remain blocked until a reviewed expansion. No
partial page is treated as complete.

The optional `TestRealHomedirReadonlyGateFixture` reads the existing closed PR
1580 and native protection with an explicitly supplied private credential file.
It checks API schemas and authentication only. It never reopens or merges that
PR, changes rules, publishes a branch or proves autonomous E2E delivery.
