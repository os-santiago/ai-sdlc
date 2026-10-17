# spec/kill-switch.md — Org kill-switch (autonomy revocation)

Normative contract for the fleet-wide emergency stop (issue #172, epic
#149). One action MUST be able to revoke autonomy across every enrolled
repo; no single-repo config edit per consumer may ever be required in an
incident. Reference implementation: `scripts/kill-switch.js` + the
"Autonomy gate" step in each stage workflow + `ai-sdlc-kill.yml`
(engage/release dispatch). Prior art: Hermes `scripts/kill_switch.js`.

## Signal channels

The gate resolves autonomy from these channels; **any engaged channel
revokes**. Order is evaluation order, not precedence — engaged wins over
clear, unreadable denies on its own.

| # | Channel | Scope | Mechanism |
|---|---|---|---|
| C1 | `AI_SDLC_KILL` / `AI_SDLC_OFF` | repo or org | Actions **variable** (not secret) surfaced to the gate step via `vars.AI_SDLC_KILL` / `vars.AI_SDLC_OFF`. An org-scoped variable visible to enrolled repos is the zero-config fleet channel. Truthy (`1\|true\|yes\|on\|engaged`, case-insensitive) engages; falsy (`0\|false\|no\|off\|clear`) disengages; **any other non-empty value engages** (fail-closed on typos). |
| C2 | open `ai-sdlc:off` issue in the control repo | fleet | The control repo is `vars.AI_SDLC_CONTROL_REPO` (per consumer); unset = the repo itself (channel degenerates to C3). One open control issue halts every enrolled repo pointing at it. |
| C3 | open `ai-sdlc:off` issue in the target repo | repo | Per-repo kill, no extra configuration. |

`ai-sdlc:off` is a control label, not an issue-flow label: it lives on a
dedicated control issue and is outside the `labels.state_machine` matrix
(spec/labels.md). Pull requests carrying the label do not count.

## Verdict — fail-closed

| State | Meaning | Autonomy |
|---|---|---|
| `clear` | every channel read, none engaged | allowed |
| `engaged` | at least one channel positively engaged | **denied** |
| `unreadable` | a channel could not be resolved (API error, missing control repo, invalid repo input, unreachable trusted code) | **denied** |

Only `clear` permits dispatch. A channel that cannot be read MUST deny —
an operator who cannot see the switch must assume it is on. The
`unreadable` denial reason names the blind channel. An authenticated
read of a public repo that fails on permissions falls back to the
anonymous REST read before declaring `unreadable`.

## Enforced stages — the authorization gate

Every dispatch path passes the same check before work is scheduled —
**routing is never permission**. The gate runs `scripts/kill-switch.js
check` fetched from the trusted runtime pin (`os-santiago/ai-sdlc@main`),
never from a consumer checkout or PR head.

| Stage | Enforcement |
|---|---|
| `ai-sdlc-intake` | gate step fails → DoR/labeling skipped, `dispatch` output stays unset |
| `ai-sdlc-implement` | gate step fails → engine never starts, no branch/PR |
| `ai-sdlc-automerge` | gate step fails → no merge, repair signal, or escalation |
| `ai-sdlc-ci-repair` | gate step fails → no repair attempt |
| `ai-sdlc-sweep` (discover) | gate step fails → no PR is fed to the merge gate |
| `ai-sdlc-review` | soft variant (`--soft` + `continue-on-error`): guard resolves `decision=skip` with `reason=kill-switch:*` — review runs on human PRs too, so a kill must not red-mark them |
| `ai-sdlc-verify` | gate step fails → no post-merge revert or issue filing |

`ai-sdlc-classify` is deliberately ungated: it applies advisory `pr:risk-*`
metadata consumed by the merge gate and schedules no work — denying it
would only hide risk signal during an incident.

Hard-fail stages stop via the default step-success chain; `if: always()`
ledger steps still run, so the halt is recorded as evidence.

## In-flight runs

The check is per run — there is no runner teardown or workflow disable.
Runs already inside an engine budget finish on their own and halt at the
**next gate**: a PR opened during a kill is never merged or repaired
while the switch is engaged. Unset the signal and the next event resumes
the loop.

## Audit — who/when

Every engagement and release MUST be attributable:

- the control issue body + comments record `actor` and an ISO-8601
  timestamp; the issue timeline records open/close transitions;
- `ai-sdlc-kill.yml` appends a run-ledger entry (`stage: kill-switch`,
  data: action/actor/reason + the operation result) sealed per issue
  #196;
- each denied gate emits a `::error`/`::warning` annotation and the run
  conclusion goes red (hard stages) — the halt is audible, never silent.

## Operations

Engage / release (canonical, audited):

```bash
gh workflow run ai-sdlc-kill.yml -f action=engage  -f reason="incident-42"   # control repo
gh workflow run ai-sdlc-kill.yml -f action=release -f reason="resolved"
```

Equivalent manual signals (equally honored by the gate):

- open an `ai-sdlc:off` issue in the control/target repo; close it to release;
- `gh variable set AI_SDLC_KILL=true` (repo) or an org-level variable
  visible to enrolled repos.

Fleet enrollment: set `vars.AI_SDLC_CONTROL_REPO=<owner>/<control-repo>`
per consumer (e.g. `os-santiago/ai-sdlc`), or rely on C1 org variables.
The stage token MUST be able to read issues in the control repo — install
the ai-sdlc App there (Issues: read) or keep the control repo public;
an unreadable control point denies by fail-closed.

## Non-goals

- No mid-run engine interruption — budgets (`max_seconds`, `max_steps`)
  remain the in-flight bound.
- No workflow disable — the sweep and merge gate must keep observable
  state so no pipeline PR is left silently unresolved.
