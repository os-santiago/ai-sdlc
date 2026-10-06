# Postmortem Issue Lifecycle

This document specifies the lifecycle and behavior of postmortem issues in the AI-SDLC system.

## Lifecycle States and Transitions

Postmortem issues follow this state diagram:

```
[open] --> [closed]
[closed] --> [reopened]  (when recurrence threshold met)
[closed] --> [filed]
[deferred] --> [filed]
```

### State Definitions

- **open**: Initial state when a postmortem issue is created.
- **closed**: State after resolution; no further action expected unless recurrence occurs.
- **reopened**: State when a closed postmortem recurses beyond the reopen threshold.
- **deferred**: Temporary state for issues awaiting further information or dependencies.
- **filed**: Final archival state for resolved issues that are not expected to recur.

### Transitions

1. `open → closed`: When the postmortem investigation is complete and resolution is documented.
2. `closed → reopened`: When post-close occurrences reach the reopen threshold (`POSTMORTEM_REOPEN_MIN`).
3. `closed → filed`: When the issue is deemed resolved and no further recurrence is expected (alternative to reopen path).
4. `deferred → filed`: When a deferred issue is resolved without needing to reopen.
5. `reopened → closed`: After the reopened issue is investigated and resolved again.

Note: The `deferred` state is used for issues that cannot be processed immediately (e.g., waiting for logs, dependencies) and transitions directly to `filed` when resolved.

## Canonical Identity

Each postmortem issue is identified by a `signatureHash` computed as:

```
signatureHash = normalize(component + class + location)
```

Where:
- `component`: The system or subsystem where the failure occurred (e.g., `auth-service`, `api-gateway`).
- `class`: The failure class or category (e.g., `timeout`, `null-pointer`, `database-deadlock`).
- `location`: The specific location of the failure (e.g., file path, function name, endpoint).

Normalization includes:
- Lowercasing
- Trimming whitespace
- Removing punctuation (except hyphens and underscores)
- Replacing sequences of whitespace with single spaces

**Rule**: There MUST be exactly one canonical issue per `signatureHash` per component. All occurrences of the same failure signature must be linked to this canonical issue.

## Deduplication

When a new postmortem candidate is identified:
1. Search **both open and closed issues** for the same `signatureHash` within the component.
2. Retention window: open issues are always eligible for dedup regardless of age. For closed issues, the window is 90 days from the `closedAt` timestamp.
3. If a matching issue is found within the window:
   - The new occurrence is linked as a recurrence to the canonical issue.
   - A new issue is NOT created.
4. If no matching issue is found within the window, a new canonical issue is created.

**Note**: The dedup window applies only to closed issues, measured from the `closedAt` timestamp — closed issues older than 90 days are not considered for dedup. Open issues are always considered, regardless of when they were created.

## Reopen Policy

Recurrences of a closed postmortem are tracked as comment-only entries on the canonical issue until a threshold is reached.

- **Threshold**: `POSTMORTEM_REOPEN_MIN` (default: 2)
- **Below threshold**: Each recurrence adds a comment to the canonical issue with:
  - Timestamp of recurrence
  - Any new context or logs
  - Link to the source (e.g., monitoring alert, ticket)
- **At or above threshold**: The canonical issue transitions from `closed` to `reopened` and a recurrence comment is added indicating the reopen.

When reopened, the issue follows the normal investigation process. After resolution, it may transition back to `closed` or to `filed`.

## Runbook Ingest

Closing a postmortem issue (transition from `open` to `closed`) triggers an automated process to ingest the failure mode into the corresponding system's runbook.

The runbook ingest process:
1. Extracts the failure mode, root cause, and resolution from the postmortem.
2. Formats the information as a runbook entry.
3. Adds the entry to the runbook for the relevant component.
4. Links the runbook entry back to the postmortem issue for traceability.

This ensures that lessons learned are codified and accessible for future incident response.

## Near-Identical Signatures

Postmortem issues with signatures that are near-identical (but not exactly matching the `signatureHash`) SHOULD be linked as related issues using the "related" issue link type.

**Important**: SignatureHashes are NEVER merged. Even if two signatures are very similar, they remain distinct canonical issues if their normalized `component + class + location` strings differ.

Fuzzy text similarity is out of scope for the dedup mechanism; only exact `signatureHash` matches are considered for deduplication.

## Configuration

The following parameters are configurable per repository or organization:

- `POSTMORTEM_REOPEN_MIN`: Minimum recurrences to trigger reopen (default: 2)
- `POSTMORTEM_DEDUP_WINDOW_DAYS`: Dedup retention window for closed issues, in days (default: 90)

These values MAY be overridden in the repository's `.ai-sdlc.yaml` configuration under a `postmortem` section.
