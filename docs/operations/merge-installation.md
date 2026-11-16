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
which supplies the runtime's check names and App IDs. The installed ruleset ID
comes only from the verified GitHub response and retained installation receipt.
Installation creates or reuses the exact dedicated rule described in
[`homedir-merge-ruleset.json`](../../runtime/k8s/deploy/github/homedir-merge-ruleset.json).
It never updates or deletes an existing ruleset. The new rule applies only to
`main`, requires strict checks and resolved threads, and has no bypass actors.
Existing protections remain in force, including stricter approval requirements.
The collector includes all effective required checks and rejects ambiguous
check owners, incomplete pages or an insufficient dedicated floor.

**`local --merge` enables autonomous low-risk merge, not only configuration.**
It can create the native rule and installs the owning component's merge loop.
The default remains disabled. Only the controller can consume publication
facts, evaluate current gates and request the single guarded merge. Installation
never merges a PR, closes an issue or deploys application code.

The installer records the returned numeric ruleset ID and manifest digest in
its private installation state, then mounts public policy only into
release-manager. Runtime never discovers authority from a name. A local lock
serializes installation; directory entries and pre-send intent are synced before
POST. A lost response leaves installation uncertain and prevents automatic
retry. Duplicate names, missing/replaced IDs or changed scope stop activation.
Do not delete the receipt to retry an uncertain operation: inspect the remote
rule and recover through a reviewed configuration change. Disabling merge
revokes its runtime capability but retains native protection and the receipt.

GitHub layers applicable rules and enforces the most restrictive constraints;
this additive approach does not overwrite concurrent administrator changes.
See [GitHub ruleset layering](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-rulesets/about-rulesets#about-rule-layering).

Only release-manager receives the merge journal's column grants and runs the
sequential one-minute merge reconciliation loop. There is no merge HTTP route,
external workflow input, new credential, model sidecar or Kubernetes API token.
Its existing isolated publication credential is used for authenticated reads
and a single guarded merge request. The signed authority ceiling becomes
`auto-merge-low`; deployment is still denied.

The controller waits for GitHub checks using durable publication facts, without
extending execution, verification or publication leases. It selects facts from
the current policy revision, issue generation and intake/execution/review
configuration. Obsolete historical facts do not block later eligible work;
selection never replaces the complete proof validation or grants authority.
Activation does not adopt publications created under an earlier policy. A fresh eligible issue is needed for a new cycle.
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
