# Org kill-switch (autonomy revocation)

Normative contract for the fleet-wide emergency stop. One control-plane action
MUST be able to revoke autonomy across every enrolled repository; no
single-repository configuration edit per consumer may ever be required in an
incident. The Kubernetes/OpenShift control plane enforces this contract before
it schedules autonomous work.

## Signal channels

Any engaged channel revokes autonomy. Evaluation order is not precedence:
engaged always wins, and an unreadable channel denies on its own.

| Channel | Scope | Mechanism |
|---|---|---|
| Controller signal | fleet or repository | A controller-managed configuration source supplies `engaged` or `clear`. Truthy (`1|true|yes|on|engaged`, case-insensitive) engages; falsy (`0|false|no|off|clear`) disengages; every other non-empty value engages. |
| Control repository issue | fleet | One open `ai-sdlc:off` issue halts every enrolled repository associated with that control repository. |
| Target repository issue | repository | An open `ai-sdlc:off` issue halts the target repository. |

`ai-sdlc:off` is a control label, not an issue-flow label. It belongs on a
dedicated control issue and remains outside the `labels.state_machine` matrix
in [labels.md](labels.md). Pull requests carrying the label do not count.

## Verdict — fail closed

| State | Meaning | Autonomy |
|---|---|---|
| `clear` | every channel read, none engaged | allowed |
| `engaged` | at least one channel positively engaged | **denied** |
| `unreadable` | a channel could not be resolved | **denied** |

Only `clear` permits dispatch. The controller MUST use its trusted deployed
revision to evaluate the switch, never code from a consumer checkout or a
proposed change.

## Enforced stages

Every autonomous path passes the same check before work is scheduled:
**routing is never permission**.

| Stage | Enforcement |
|---|---|
| intake | denial skips DoR and autonomous labeling |
| implementation | denial prevents an engine run and proposal |
| merge | denial prevents autonomous merge, repair, and escalation |
| repair | denial prevents a repair attempt |
| discovery | denial prevents a proposal reaching the merge gate |
| review | a soft denial records `kill-switch` and skips autonomous review work without marking a human change as failed |
| verification | denial prevents autonomous post-merge work |

Risk classification may remain available when it only emits advisory metadata
and schedules no work; denying it would hide risk signal during an incident.

## In-flight work and audit

The check is per work item; there is no forced worker teardown. Work already
inside an engine budget may finish, but it halts at the next gate. A change
opened during a kill is never autonomously merged or repaired while the switch
is engaged.

Every engagement and release MUST be attributable. The control plane records
the action, actor, reason, timestamp, and result in durable audit storage, and
each denied gate emits a durable warning or error event. Engage or release
through the controller's audited administrative interface. Manual equivalent
signals are opening or closing an `ai-sdlc:off` issue, or setting the reviewed
controller-managed signal for a repository or fleet.

Fleet enrollment associates every consumer with a reviewed control repository
or a fleet-level controller signal.
