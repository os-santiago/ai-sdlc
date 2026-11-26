# Signed publication controller

The controller composes the verifier's signed handoff, immutable candidate,
behavior proof and independent review with current issue observations. Every
external effect repeats the complete authority check under release-manager's own
read-only evidence identity. It never borrows a verifier or worker credential.

Before acquiring a 120-second claim, it reconstructs the candidate from public
pinned source and derives the exact two-file Git tree. The plan fixes the branch,
the installation's [commit identity](publication-writer.md#commit-identity) and
the `[skip release]` marker; the plan digest the claim records binds all three.
Model output cannot supply a URL, shell command, branch, commit identity,
publication waiver or deployment target.

The writer's pre-send callback commits an intent bound to the actual serialized
HTTP body. It checks authority again before transport and after the response.
Tree, commit, branch and PR acknowledgements are ordered and non-replayable.
Unknown results stop publication and quarantine subsequent work. The final proof
requires all four receipts and grants neither merge nor deployment authority.

While quarantined, each poll attempts to [reconcile](publication-reconciliation.md)
at most one eligible uncertain effect by read-only forge observation. Until the
original claim has settled, and while every eligible effect is waiting out its
retry backoff, the poll reports `publication-quarantined` without querying the
forge. Exact existence records the receipt the call would have produced; proven
absence records a known no-send. A missing branch is not proof. Anything else
keeps the quarantine. The original request is closed as failed and
never retried. The poll reports `publication-reconciled` once no uncertain effect
remains.

## Capability boundary

`AI_SDLC_PUBLICATION=true` is a trusted configuration ceiling. It requires
verification mode and independent review, and permits only `publish-candidate`
in addition to their existing operations. Other downstream grants and autonomy
levels remain rejected. This change does not install the option, routes, GitHub
credentials, database grants or a running reconciler. Those require the explicit
installation integration.

## Reproduce the isolated conformance check

Build the execution and review test drivers from the same checkout. Use the
existing `tools/verify_review_flow.py --scenario approve` procedure with
`AI_SDLC_PUBLICATION_FIXTURE=1` and `AI_SDLC_PUBLICATION_REPORT=/tmp/publication.json`.
The procedure requires the owned disposable kind cluster and loopback PostgreSQL
`ai_sdlc_test` database documented by the review fixture. Test-only private
identities never enter executor Pods.

This adds a real signed HTTP inbox and PostgreSQL publication controller after
actual SCC/Kubernetes generation, behavior verification and independent review.
Its forge is deliberately synthetic: it checks success, a lost tree response,
authority revocation and a lost PR response without creating GitHub resources.
The quality workflow runs this bounded extension for its approved scenario and
uploads separate publication evidence. It is not the autonomous issue-to-kind
production E2E.

`tools/validate_runtime.py` additionally runs build, vet, proof-binding tests and
real PostgreSQL journal/fault tests. The GitHub transport has separate bounded
real TLS HTTP tests in `internal/publication`.

## Stop log

When a poll stops, the release manager logs
`publication reconciliation stopped safely: <stage>`. The stage is bounded
controller metadata, never a response body, URL, credential or candidate text:
`authority`, `source`, `tree-plan`, `claim`, `proof`, `commit-proof`,
`quarantined`, `configuration`, `unknown` (a store error before any write), or a write stage (`tree`, `commit`, `branch`, `pull-request`)
with the writer's class appended, for example `tree-status-422`:

- `status-<code>` (or `status-4xx`, `status-5xx`): the forge answered with that
  HTTP status and created nothing the writer accepted.
- `transport`: the connection, TLS handshake or deadline failed; the request may
  or may not have reached the forge.
- `response`: a `201` whose headers or body the writer refused.
- `mismatch`: a decoded `201` that is not the planned object.
- `fence`: current authority was lost after the response, or the caller's
  deadline expired after it arrived.

Every class leaves the effect uncertain, exactly as before; the class only tells
an operator where to look. The merge loop logs
`merge reconciliation stopped safely: <state>`, for example `merge-quarantined`
while an unacknowledged merge effect blocks every later merge.
