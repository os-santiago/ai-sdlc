# Generated Artifact Publish Contract

This document specifies the contract for publishing generated artifacts in the AI-SDLC pipeline.
It defines the diff-gating algorithm, rolling-PR semantics, and budget/circuit-breaker mechanisms
that govern when and how generated artifacts are published to the repository.

## Scope

This contract applies to all `publish*`-style artifacts produced by the pipeline, including but not limited to:
- Runbook
- Context atlas
- Changelog
- Environment variable catalog

## Diff-gating Algorithm

To avoid publishing semantically identical artifacts that differ only in volatile metadata, the pipeline
shall implement the following diff-gating procedure for each artifact class:

1. **Volatile Pattern Stripping**: Remove all occurrences of declared volatile patterns from the artifact content.
   Volatile patterns include but are not limited to:
   - Timestamps (ISO 8601, Unix epoch, etc.)
   - Run identifiers (UUIDs, GitHub run IDs, etc.)
   - Process identifiers (PIDs)
   - Any other patterns declared as volatile in the artifact's production configuration

2. **Hash Computation**: Compute the SHA-256 hash of the stripped content.

3. **Comparison**: Compare the computed hash with the hash of the last published artifact of the same class
   (stored in the pipeline's internal state or derived from the rolling PR).

4. **Publish Decision**: Publish the artifact (i.e., create or update the rolling PR) only if the computed hash
   differs from the last published hash. If the hashes are identical, skip publishing for this cycle.

This ensures that consecutive cycles with identical semantic content produce zero commits.

## Rolling PR Semantics

For each artifact class, the pipeline shall maintain:
- A single long-lived branch named `artifact/<artifact-class>` (e.g., `artifact/runbook`)
- A single open pull request targeting the default branch, whose head is the aforementioned long-lived branch

The pipeline shall update the rolling PR in place by:
- Committing the new artifact content as a new commit on top of the long-lived branch's existing
  history — either a regular commit, or a merge commit when reconciling with updates on the default branch.
- Force-pushing and any history-rewriting operation on the rolling branch (`git commit --amend`,
  `git reset`, or `git rebase` applied to commits already pushed to the remote) is **strictly
  prohibited**: every update is a regular push that only appends commits, ensuring the branch history
  remains append-only and easy to follow.

The PR title and body shall be updated to reflect the latest artifact version and publication timestamp.

## Budgets and Circuit-Breaker

To prevent resource exhaustion and detect persistent failures, the pipeline shall enforce:

### Per-Cycle Bounds
- **Scan Limit**: Maximum number of files to scan for volatile patterns per cycle (configurable, default: 1000)
- **Write Limit**: Maximum number of bytes to write to the artifact file per cycle (configurable, default: 1MB)

### Failure Handling
- Let `N` be the configured failure threshold (default: 3)
- If the pipeline encounters `N` consecutive failures to publish an artifact class (due to bounds exceeded,
  hash computation errors, or Git operation failures), it shall:
  1. Halt further publish attempts for that artifact class in the current cycle
  2. Escalate the issue by adding the label `publish:circuit-breaker` to the associated rolling PR
  3. Notify configured contacts via the escalation channel

The circuit-breaker resets automatically after a successful publish or when manually cleared by a human.

## Verification

An implementation of this contract shall satisfy the following verification criteria:
- Two consecutive cycles with identical semantic content (after volatile stripping) for any artifact class
  result in zero commits to the rolling PR branch and no PR updates.
- The rolling PR for each artifact class remains open and is updated in place without force-pushing
  or rewriting previously pushed history.
- A `volatile_patterns` entry that is not a valid regular expression is rejected at configuration-load
  time as a configuration error and triggers the escalation path instead of a runtime failure.
- When the scan or write bounds are exceeded, the pipeline halts publishing for that artifact class and
  escalates after `N` consecutive failures.

## Configuration

The following parameters may be configured in `.ai-sdlc.yaml` under a `publish` key:

```yaml
publish:
  volatile_patterns:
    - '\\d{4}-\\d{2}-\\d{2}T\\d{2}:\\d{2}:\\d{2}Z'  # ISO timestamps
    - 'runid:[a-f0-9]+'                            # Example run ID pattern
  bounds:
    scan_limit: 1000
    write_limit: 1048576  # 1MB in bytes
  failure_threshold: 3
```

`volatile_patterns` entries must be valid regular expressions in the dialect supported by the
implementation's regex engine. The pipeline shall validate every configured pattern when the
configuration is loaded, before the first publish cycle. A pattern that fails to compile is a
configuration error: the pipeline shall count it toward the failure threshold, halt publishing for
the affected artifact class, and escalate via the `publish:circuit-breaker` label and configured
contacts — rather than discovering the failure at scan time.

If not specified, the defaults described above shall apply.
