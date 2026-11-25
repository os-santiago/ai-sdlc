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
subsequent publication. A trusted no-send record is valid only before entering
transport. HTTP errors never establish that an operation did not happen.

An unresolved effect of a failed request can later be resolved only by
[reconciliation](publication-reconciliation.md): once the claim has settled, a
read-only forge observation re-derives the journaled request. The observation
records either an observed receipt (the effect exists exactly) or a known no-send
(the forge proves it absent). The request itself stays failed.

Completion requires all four receipts and matching commit/branch/PR evidence.
Its proof explicitly does not authorize merge or deployment. No model content,
credentials or HTTP response body is stored in an effect receipt.

## Validation

`python3 tools/validate_runtime.py --provider podman --output /tmp/publication-validation`
runs the real PostgreSQL concurrency, role isolation, expiry and lost-COMMIT
fixtures. The fault proxy drops actual PostgreSQL COMMIT acknowledgements for
claim, intent, acknowledgement and completion. These tests create no GitHub PR
and do not prove the autonomous E2E.
