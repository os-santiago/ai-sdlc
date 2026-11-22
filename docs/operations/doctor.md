# Read-only doctor and issue-correlated status

`./install.sh doctor` reports installation health; `./install.sh status --issue N`
(or `python3 tools/issue_status.py`) traces a single issue through the delivery
chain. Both are strictly read-only: they contain no stage-advance, resume,
approve or dispatch capability and issue no mutating Kubernetes subcommand —
only `get` and a fixed `exec ai-sdlc-postgres-0 psql` whose statements always
run inside `BEGIN READ ONLY`.

Neither tool ever prints Secret data, environment values, issue title or body
prose, model prompts or transcripts, or source file contents. Output is
bounded to statuses, enum-constrained reasons, stable reason codes,
timestamps, counts and digests. Secret checks probe names only
(`get secret <name> -o name`); `data`/`stringData` are never requested.

## Usage

```bash
./install.sh doctor --provider podman
./install.sh status --provider podman --issue 435 --repository os-santiago/ai-sdlc
python3 tools/issue_status.py --provider podman --issue 435
```

`doctor` exits nonzero when any check fails. `status --issue` exits zero when
the observation succeeded; the JSON stages carry the per-stage state.

## Doctor checks

| Check | Evidence | Failure codes |
| --- | --- | --- |
| `configuration` | `ai-sdlc-config`/`ai-sdlc-authority` ConfigMaps, runtime mode, installed repository/environment scope | `not-installed`, `misconfigured`, `scope-mismatch` |
| `immutable-artifacts` | Component images equal the committed `images.lock.json` digests | `image-mismatch` |
| `database-migrations` | `schema_migrations` count equals the committed migration files | `migrations-incomplete`, `observation-denied` |
| `component-readiness` | Ready replicas of the six components plus PostgreSQL | `not-installed`, `not-ready` |
| `app-scopes` | Presence of the GitHub client credential Secret name | `credentials-missing` |
| `intake-connectivity` | `intake_cursors` watermark, next poll and bounded failure count | `connectivity-degraded` |
| `model-connectivity` | Credential Secret name presence plus the persisted model-call status histogram (401/403 → `authority-denied`, 429 → `rate-limited`, 5xx → `connectivity-degraded`) | `credentials-missing`, `rate-limited`, `authority-denied`, `connectivity-degraded` |

Connectivity evidence is the recorded journal, not a live probe: the doctor
never mints tokens, calls the model, or creates probe resources
(`tools/network_probe.py` is intentionally not used).

## Issue-correlated status

The status traces the latest intake revision for `repository#issue` through
`revision → admission → plan → execution → verification → review →
publication → merge → release → deployment`, keyed by the revision's
`correlation_id`. Stages that do not exist yet report `unavailable`; stages
that exist but are sealed by row-level security for the observation role
report `observation-denied` instead of failing the whole report.

### Stable reason codes

`none`, `in-progress`, `unavailable`, `issue-not-observed`,
`observation-denied`, `awaiting-human`, `dependency-blocked`,
`authority-denied`, `credentials-missing`, `rate-limited`,
`budget-exhausted`, `evidence-failed`, `changes-requested`, `expired`,
`rejected`, `uncertain`, `stopped`, `not-installed`, `misconfigured`,
`image-mismatch`, `migrations-incomplete`, `not-ready`, `scope-mismatch`,
`connectivity-degraded`, `stale`, `ok`.

The six required distinctions map directly: blocked work items →
`dependency-blocked`; `authority-revoked` stops and rejected admissions →
`authority-denied`; absent credential Secret names → `credentials-missing`;
HTTP 429 responses in the model ledger → `rate-limited`; stopped/exhausted
model budgets or the 16-attempt execution bound → `budget-exhausted`;
failed verification, review, publication, merge or executor evidence →
`evidence-failed`.

### Metrics

`intakeLagSeconds`, `queueAgeSeconds`, `retryCounts` (intake, execution,
verification, review), `activeWork` (queued/claimed/running) and
`terminalFailures` per bounded stage key — fixed-name aggregates only, so
label cardinality cannot grow with cluster content.

## Validation

`tools/test_install_doctor.py` and `tools/test_issue_status.py` assert that
every cluster call is a `get` or the fixed read-only `psql` exec, that every
SQL body begins `BEGIN READ ONLY`, that injection-shaped scopes are rejected
before SQL construction, that Secret values never reach output, and that all
emitted reason codes come from the bounded set. Functional validation against
a live cluster requires an installed runtime; unverified environments must
be reported explicitly.
