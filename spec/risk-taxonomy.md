# PR Risk Taxonomy + Merge-Gate Decision Table (spec)

Normative vocabulary for PR risk and the review evidence required at each
tier. Every adopting repo shares this taxonomy so merge policy is portable
and machine-implementable. Labels: `pr:risk-*` (applied at PR creation,
recomputed on new pushes).

## Risk tiers

| Tier | Label | Signals (any ⇒ tier) |
|---|---|---|
| Low | `pr:risk-low` | docs/comments/tests/config-text-only changes; additive code under ~300 diff lines in one subsystem; no auth/data/infra surface touched |
| Medium | `pr:risk-medium` | behavioral code changes; new dependencies (vetted); refactors under ~800 diff lines; CI/workflow changes that don't alter gates |
| High | `pr:risk-high` | auth/permissions, schema/migrations, deletes of public API surface, security-adjacent config, >800 diff lines, multi-subsystem refactor |
| Critical | `pr:risk-critical` | credential/secret handling, supply-chain gate changes, deploy/payment/data-destruction paths, policy-file changes, workflow files that widen token permissions |

Ambiguity resolves UP: when signals straddle tiers, the higher tier wins.

## Required evidence per tier

| Tier | CI green | AI review | Human |
|---|---|---|---|
| `pr:risk-low` | required | optional (informational) | not required |
| `pr:risk-medium` | required | required (no blocking findings) | not required |
| `pr:risk-high` | required | required + convergence loop clean | recommended sign-off |
| `pr:risk-critical` | required | required + convergence clean | **required**: `pr:risk-accepted` label by a human, always |

## Merge-gate decision table (normative)

```
IF draft OR skip-label (needs-human|hold|do-not-merge|wip) → HOLD (human owns)
IF checks red    → repair loop (bounded) → exhausted → ESCALATE
IF checks pending > pending_max → ESCALATE
IF conflict      → one update-branch attempt → still conflicting → ESCALATE
IF dep violations → ESCALATE (needs-human, supply-chain)
IF risk-critical AND NOT risk-accepted → HOLD
IF risk-high/medium AND blocking review findings unresolved → repair loop
IF checks green AND mergeable AND tier evidence satisfied → MERGE (squash)
```

`ESCALATE` = `needs-human` label + explanatory comment + notification.
`HOLD` = wait for the missing condition; the label state is re-evaluated
every poll (removal hands control back — no sticky latches).

## Notes for implementers

- Tier assignment SHOULD be computed from the diff (files/lines/surfaces),
  not self-declared by the author-agent.
- The decision table maps to the merger's `decidePrAction` semantics;
  divergences between runtime behavior and this table are bugs.
