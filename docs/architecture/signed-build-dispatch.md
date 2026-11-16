# Internal merged-source dispatch

Release-manager derives the next build exclusively from durable merged history,
current issue observations and installed inner/outer policy. Callers cannot
supply a source, recipe, merge or stage. The operation reloads authority before
signing and rechecks journal/issue fences inside the outbox transaction.

An immutable `build_handoffs` row and the signed `build-merged` outbox record
commit together. One merge can produce one dispatch, including after a lost
caller acknowledgement or concurrent poll. Failed outbox insertion rolls back
the handoff. The relay adds its transport receipt; the build inbox verifies both
receipts and atomically queues the request. This PR provides internal library
composition; it does not activate an installed reconciler or grant a release.

Validation uses a disposable PostgreSQL database with dedicated component
roles: signed outbox-to-inbox delivery, deduplication, concurrent dispatch,
outbox failure rollback, policy reload/freeze, stale issue/cursor and completed
work denial. The full build/vet/test suite passed with 1,518 tests across 43
packages. This is component evidence, not autonomous issue-to-production E2E.
