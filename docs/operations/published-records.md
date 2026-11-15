# Durable publication evidence

A PR can outlive the short generation, verification, review and publication
leases while GitHub CI runs. Those leases must not be extended or reused to
merge it. Migration 18 adds a terminal `published_records` snapshot, committed
atomically with the existing `published` transition.

The snapshot binds the original signed command and receipt to the publication
proof and four acknowledged effects. It retains their exact bytes and digests,
the original authority expiry and a PostgreSQL-generated publication timestamp.
The timestamp must precede that expiry. Release-manager can insert the reviewed
columns and read the record; it cannot provide the timestamp, update a record or
delete it. Other operational identities have no access. Disabling publication
revokes these grants along with the existing journal grants.

`ReadPublishedRecord` verifies the terminal journal binding, hashes, ordered
receipts, commit, branch and PR identity. It deliberately does not demand a live
publication claim. Reading a fact grants no merge, release or deployment
capability and never requeues work. Existing publications are not backfilled:
missing, failed, incomplete or inconsistent records fail closed.

A future merge controller must combine this historical fact with independently
obtained **current** authority, current issue eligibility, immutable head/base
identity, native protection, successful required checks from their expected
integration, resolved review threads and no active changes-requested review.
It must not use the expired generation lease or the legacy unauthenticated gate
snapshot as permission. Current-policy changes must be evaluated explicitly.

In the Homedir pilot, GitHub's required status checks were observed on
2026-11-15 with `strict_required_status_checks_policy: false`. An atomic merge
boundary must additionally enforce an up-to-date base through native protection
or an equivalent reviewed mechanism. The head SHA precondition alone does not
pin the base. See [GitHub protected branch semantics](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-protected-branches/about-protected-branches)
and [pull request merge API](https://docs.github.com/en/rest/pulls/pulls#merge-a-pull-request).

This change records evidence only. It does not install or authorize that future
merge controller, Konflux promotion or production deployment.
