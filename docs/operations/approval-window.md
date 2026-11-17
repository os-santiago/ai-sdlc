# Install a new finite approval window

An expired test-environment approval stops new delivery. It does not become
current because an installer is rerun. After a complete diagnostic and 24-tool
update, an operator can supply two independently reviewed public documents:
the provenance trust and Saferus release target. This initial operation changes
only their `approvedAt` and `expiresAt` fields. Keys, CAs, scopes, destinations,
Kubernetes UIDs, image pins, profiles, budgets and all other fields stay exact.

```bash
./install.sh runtime-upgrade reapprove \
  --cluster ORIGINAL_CLUSTER --provider podman \
  --provenance-trust /reviewed/new-provenance-trust.json \
  --release-target /reviewed/new-release-target.json

./install.sh runtime-upgrade reapprove-status \
  --cluster ORIGINAL_CLUSTER --provider podman
```

Review the current original signer, registry, application and TLS identities
before providing new dates. The new approval must start after the old approval,
already be effective and extend the previous expiry. Each window lasts at most
24 hours; the unchanged runtime compilers also enforce certificate validity and
their original scope restrictions. The installer never generates approvals,
selects another key, replaces the application or automatically extends a date.

The source must have a completed, active `tool-budget-upgrade.json`, no unresolved
transition or recovery and a verified, cleaned-up backup. Only contained failed
work with stopped requests and no pending authenticated delivery is supported.
An incomplete or expired administrative intent is not a source for reapproval.
This is a bounded initial lifecycle operation, not recurring automatic rotation.

If the source approvals expired, the administrative observer still requires the
original complete Pod inventory, Running containers, templates, resource UIDs,
credentials and database authority. It can observe unavailable readiness solely
to suspend that exact source. This observation does not establish readiness or
permit delivery. New approvals must pass the original public compilers and live
registry/application checks before the first administrative intent is recorded.

A new protected `approval-window.json` binds the original completed operation,
both supplied documents, all rendered phases and a fixed administrative deadline
of at most 30 minutes within the new approvals. The previous operation and
approvals remain unchanged. Source polling is captured under the first write
fence using the same [retained-history protocol](image-lock-upgrades.md#apply-the-reviewed-generator-tool-budget).
The installer stops producers, captures a fresh private backup, proves restoration
in isolated PostgreSQL and confirms cleanup before staging the new dates.
It then warms downstream controllers with frozen policies, validates readiness
under the new approvals and opens intake last.

Existing execution, build, promotion and deployment records retain their original
authority, timestamps and outcomes. No workflow rows are written, failed work is
not retried, and a new request must still enter through an issue. Completing this
operation does not prove issue-to-production E2E success.

Reapplying the command must supply the same documents. It never renews the
administrative deadline, repeats an uncertain Helm operation or overwrites an
incomplete backup. An already-converged Helm target can only be observed. Keep
the original records and resources if an operation stops; this recipe cannot
return to expired approvals or renew an incomplete operation. A subsequent
reapproval of an already renewed installation requires a separately implemented
lifecycle transition.

## Validation scope

Tests render the actual chart and compare a counterfactual that restores the
old dates, reject changes to other fields and invalid windows, preserve original
credential bindings and verify that ordinary activation still requires readiness.
Shared pause, backup and transition tests cover exclusion, retained history,
uncertain responses and intake ordering. Actual expired-source observation,
approved reactivation and the user-visible application must be verified on the
installed runtime; isolated fixtures do not establish those results.
