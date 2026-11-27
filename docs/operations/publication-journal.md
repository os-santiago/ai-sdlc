# Publication journal

The inactive publication journal separates release-manager ownership from the
verifier's signed handoff. Installing verification/review does not grant any
publication access. No HTTP route, GitHub credential or controller is activated.

Only a passed, cleaned behavior verification and independent review can back a
handoff. The queue binds its command, candidate, exact proof hashes and original
expiry. A fresh, exclusive 120-second claim cannot be renewed or adopted.

Four ordered effects record an exact HTTP request digest before transport:
Git tree, commit, new branch and pull request. Each acknowledgement records only
checked identifiers. Missing replies stop the claim; an unresolved effect blocks
subsequent publication. On the write path, `SealPublicationNotSent` records a
no-send only when the writer knows it never entered the transport, under the
live claim. HTTP errors never establish that an operation did not happen.

An unresolved effect of a failed request can later be resolved only by
[reconciliation](publication-reconciliation.md) through `ResolvePublicationEffect`.
Once the claim has settled, a read-only forge observation re-derives the
journaled request. It records either an observed receipt (the effect exists
exactly) or a known no-send. A no-send needs positive evidence of absence from
the forge, never an error from the original call: a `404` for a
content-addressed tree or commit while the parent commit stays readable, or an
empty pull request list while the head branch still points at the planned
commit. A missing branch never counts, because a ref can be created and then
deleted, and the journal refuses a branch no-send. The request itself stays
failed.

This is one of two narrow, named exceptions to the rule that an observation never
manufactures a missing receipt (the other is
[merge reconciliation](merge-reconciliation.md#principle-exception-an-observed-merge-receipt)). It applies only when all of these hold:

- the effect is uncertain;
- its request is already `failed`;
- the settle window has passed;
- the observation is read-only and exactly matches the journaled planned digest;
- the receipt is marked `observed: true`.

Such a receipt is never accepted where a live receipt is required (see the
[principle exception](publication-reconciliation.md#principle-exception-an-observed-receipt)).

Completion requires all four receipts and matching commit/branch/PR evidence.
Its proof explicitly does not authorize merge or deployment. No model content,
credentials or HTTP response body is stored in an effect receipt.

## Validation

`python3 tools/validate_runtime.py --provider podman --output /tmp/publication-validation`
runs the real PostgreSQL concurrency, role isolation, expiry and lost-COMMIT
fixtures. The fault proxy drops actual PostgreSQL COMMIT acknowledgements for
claim, intent, acknowledgement and completion. These tests create no GitHub PR
and do not prove the autonomous E2E.
