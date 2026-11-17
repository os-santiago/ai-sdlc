# Replace a reviewer after a contained failure

This administrative operation changes only `review.configJSON.workerImage` in
an installation with a completed approval-window transition. It preserves the
generator image, controller images, model quotas, eight-tool reviewer limit,
credentials, trust, approvals, resource identities and workflow history.

Use an independently reviewed worker publication from a successful exact-revision
`main` container build. The selected source must preserve the original 34-file
migration catalog and pin SCC to a full commit SHA. Publication metadata and the
pulled image digest establish artifact identity; they are not SLSA certification.

```bash
./install.sh runtime-upgrade reviewer-image \
  --cluster ORIGINAL_CLUSTER --provider podman \
  --target-run REVIEWED_CONTAINER_RUN --target-revision FULL_MAIN_SHA
./install.sh runtime-upgrade reviewer-image-status \
  --cluster ORIGINAL_CLUSTER --provider podman
```

The runtime must already have stopped all executions and model budgets. Execution,
verification and review claims must have expired; cleanup must be complete. Every
captured candidate must retain its passed verification and failed independent
review. Pending authenticated delivery, model handoff, unbound running work or any
publication and later-stage history prevents the operation. The installer reads
these records; it never completes, retries, deletes or renews them.

The migrator selected from that same target publication must support the separate
`hold-reviewer-upgrade-fence` protocol. Its SQL history predicate is shared with
the installer. The original pre-candidate fence remains unchanged and rejects
this history. Unacknowledged execution effects are rejected; review/verification
effects require either an acknowledgement or an explicit original `not_sent_at`
fact. Cleanup alone never proves an operation was not sent.

Before changing Kubernetes resources, the installer authenticates the publication,
pulls the immutable image, checks original installation custody, and captures the
history under database write exclusion. It pauses producers, creates a new private
backup and restores that backup into a disposable database. Equality checks cover
all retained data, schema, permissions and role identities. The five activation
phases keep intake closed until the final step.

`reviewer-image-upgrade.json` and `reviewer-image-backup/` record the separate
operation. Its deadline is at most 30 minutes and cannot outlive the existing
approval documents. Repeating a command never renews that deadline or repeats an
ambiguous Helm send. An incomplete pause, backup or transition requires preserving
the journal and investigating the recorded state. There is no automatic rollback
or second reviewer update in this bounded transition.

An active installation report proves administrative readiness, not workflow or E2E
success. A new request must enter through the ordinary issue intake. The original
failed review remains failed.

Validation combines actual Helm rendering with
`validate_upgrade_migration.py --exercise-reviewer-history`: published migrations,
a disposable PostgreSQL database containing a captured candidate, passed
verification and failed review, negative controls for active authority and
downstream work, and a real backup restoration. An installed CLI run and a later
autonomous issue remain separate evidence requirements.
Supply `--reviewer-fence-image` with the immutable candidate migrator to exercise
actual acquisition, checks, writer exclusion, release, rejection by the legacy
protocol and rejection of an unacknowledged execution effect. This fixture uses
the candidate helper locally; it does not publish or apply an installation.
The [retention fixture result](../evidence/2026-11-17-reviewer-history.json)
records the observed restoration and rejected negative controls.
