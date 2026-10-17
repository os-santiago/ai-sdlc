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

## Failure classes and alert routing (normative)

Every ESCALATE — and every stage failure that pages a human — carries a
canonical **failure class**. The class decides WHO is notified via the
contract's `escalation.routes` map (`failure-class → route`), so a
supply-chain halt and a stalled dep-update no longer land identically.

| Class | Emitted when | Decision-table source |
|---|---|---|
| `conflict` | `mergeable=CONFLICTING` survives the update-branch attempt | conflict → ESCALATE |
| `checks-stalled` | checks pending past `merge.pending_max_minutes` | pending-timeout → ESCALATE |
| `repair-stalled` | a repair signal on the head produced no new head within the cap | repair-stalled → ESCALATE |
| `repairs-exhausted` | the bounded repair budget is consumed with checks still red | repairs-exhausted → ESCALATE |
| `repair-failed` | a repair attempt produced no pushable fix (no-changes, engine/provider exit, push rejected) | ci-repair escalate |
| `review-blocked` | an external/AI blocking verdict holds past the wait cap | external-review-blocking → ESCALATE |
| `review-fix-failed` | the review→fix pass stalled or was never dispatched | review-fix-stalled / review-fix-not-dispatched → ESCALATE |
| `supply-chain` | dependency violations on dep manifests/lockfiles | dep violations → ESCALATE |
| `contract-error` | `.ai-sdlc.yaml` fails schema validation on the base ref (`scc:config-error`) | ci-repair base-fault short-circuit |
| `verify-failure` | post-merge `verify.commands` fail on the merge commit | verify stage |
| `security` | secret-scan / credential-material findings | runtime scanners |
| `policy-hold` | `pr:risk-critical` awaiting human `pr:risk-accepted` | risk-critical → HOLD |
| `unknown` | no pattern matched — the reserved fallback class | any failure surface |

**Route resolution** (implemented by `scripts/alert-router.js`):

```
route = escalation.routes.<class>
      → escalation.routes.default          (reserved fallback class)
      → escalation.contacts/notify_channels (synthesized default)
```

A route is `{mentions: [@handles|org/team], labels: [...], channel: <id>}`.
An undeclared or unmatched class ALWAYS resolves to the default route —
alerting is never silent. Every emitted notification carries an audit
marker (`<!-- ai-sdlc:alert-route class=… key=… -->`); re-notifications of
the same class+signature on the same thread are deduplicated on that
marker so repeat escalations don't page twice for one failure.

## Notes for implementers

- Tier assignment SHOULD be computed from the diff (files/lines/surfaces),
  not self-declared by the author-agent. Reference implementation:
  `scripts/risk-classifier.js` + `ai-sdlc-classify.yml` — deterministic
  path/size/content signals applied on `pull_request` events; the
  classifier source must come from a trusted ref, never the PR head.
- Dep-update fast path (issue #177): a diff bounded to dependency
  manifests/lockfiles carrying only patch|minor bumps with no new direct
  deps classifies `pr:risk-low` (`scripts/dep-update.js`
  `classifyDepUpdateDiff`/`depUpdateVerdict`). Majors, unknown specs, new
  deps and non-dep files fail closed onto the generic path (dep surfaces →
  high).
- The decision table maps to the merger's `decidePrAction` semantics;
  divergences between runtime behavior and this table are bugs.
