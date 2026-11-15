# Install the low-risk merge capability

The Homedir verification installation can explicitly select `"merge": true`
in its installation JSON, or `--merge` with the local entry point. It also
requires `publication`, `independentReview`, the `verification` profile and
`--github-auth gh-cli`. Merge is disabled by default and restricted to
`os-santiago/homedir`. Use reviewed compatible image pins; an old image lock
cannot implement a new capability.

Preview with `./install.sh plan --config <path>`; apply with
`./install.sh local --config <path> --github-auth gh-cli`. The installer verifies
the native GitHub ruleset against
[`homedir-policy.json`](../../runtime/k8s/internal/mergegate/homedir-policy.json),
which also supplies the runtime's check names, App IDs and ruleset ID.
Verification is read-only: it rejects insufficient protection instead of
rewriting an existing ruleset without an atomic concurrency guard. The current
pilot requires strict checks, resolved review threads, zero configured human
approvals and no bypass actors. Additional native rules are retained. A future
additive ruleset installer must receive separate validation before use.

Only release-manager receives the merge journal's column grants and runs the
sequential one-minute merge reconciliation loop. There is no merge HTTP route,
external workflow input, new credential, model sidecar or Kubernetes API token.
Its existing isolated publication credential is used for authenticated reads
and a single guarded merge request. The signed authority ceiling becomes
`auto-merge-low`; deployment is still denied.

The controller waits for GitHub checks using durable publication facts, without
extending execution, verification or publication leases. It selects facts from
the current policy revision; activation does not adopt publications created
under an earlier policy. A fresh eligible issue is needed for a new cycle.
Every attempted merge receives a fresh bounded command, original claim, effect
journal and immutable terminal record. Uncertain effects quarantine the stage.
Generated merge commits retain `[skip release]` so this local pilot does not
trigger Homedir's public production release.

To disable, set `merge` false and apply the reviewed configuration. The signed
ceiling returns to `auto-pr`, the merge grant is removed, the loop is disabled,
and the migrator revokes merge table/column permissions. Historical facts remain;
disabling cannot clear quarantine or replay a terminal attempt. Observe active
work before changing installation capabilities.

## Validation boundaries

`tools/verify_publication_install.py --merge` checks enable, reapply, disable and
re-enable on its fixed disposable kind cluster. It keeps authority frozen,
requires zero workflow requests and verifies readiness and actual PostgreSQL
permissions. It does not modify GitHub protection or prove autonomous merging.
The controller suite uses real PostgreSQL and synthetic forge responses with
the same installed grants. Neither test proves issue-to-production E2E.

Real GitHub publication/merge, artifact construction and provenance, promotion
to the separate `ai-sdlc-production` test namespace, and user behavior validation
remain distinct evidence requirements. Readiness reports
`verified-low-risk-merge-only`; it does not claim deployment capability.
