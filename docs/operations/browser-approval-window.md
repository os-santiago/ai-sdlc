# Renew a completed browser installation's approval window

`runtime-upgrade browser-reapprove` installs a separately reviewed, finite pair
of provenance and release approval documents above a completed schema-35 browser
installation. Use this administrative operation when those documents expire
before a later installation transition. It does not renew historical work,
replay a rejected candidate, or demonstrate autonomous delivery.

The existing browser, reviewer and approval records remain unchanged. Both new
documents must preserve every field except `approvedAt` and `expiresAt`, move
the window forward, and remain within 24 hours and the original registry CA's
validity. Review the complete documents before invoking the command.

Use an exact, clean checkout of the successful main publication that includes
this command and the schema-35-compatible feedback fence:

```bash
./install.sh runtime-upgrade browser-reapprove \
  --cluster YOUR_CLUSTER --provider podman \
  --target-run MAIN_PUBLICATION_RUN --target-revision MAIN_COMMIT \
  --provenance-trust /private/reviewed-provenance.json \
  --release-target /private/reviewed-release-target.json
./install.sh runtime-upgrade browser-reapprove-status \
  --cluster YOUR_CLUSTER --provider podman
```

The publication supplies an authenticated administrative helper. Installed
component images, schema, keys, budgets, browser profile and Kubernetes resource
identities remain fixed. Source observation permits expired approval documents
only for containment; it does not assert runtime readiness. The operation
checks residual execution, acquires the database fence, captures current
history, pauses the ten consumers, exports and restores a private backup in
isolation, and stages the new dates. Intake resumes last after actual readiness
and release/browser checks.

The private `browser-approval-history.json` stores at most eight operations and
32 MiB. Each new operation binds the immutable completed prefix and gets one
nonce, backup directory and deadline. Repeating the same latest documents
selects that operation; it cannot reset its deadline. An incomplete operation
rejects different documents. Completed entries cannot be rewritten or selected
again after a descendant exists. An unknown pause or Helm outcome permits only
the original observation/reconciliation path, never an automatic resend.

A subsequent [feedback transition](review-feedback-upgrade.md) binds the exact
completed approval prefix. Once feedback installation has an intent, browser
reapproval is refused. Schema-36 reapproval and unbounded history rotation are
outside this operation's supported scope. Preserve state and backups when a
check fails; do not edit journals to bypass a deadline or unresolved outcome.

Validation separates journal and chart tests from Kubernetes acceptance. Passing
the local tests alone does not establish a completed installation or issue-to-
production flow.
