# spec/ — Normative AI-SDLC contract

Versioned, declarative surface that every repo opting into the pipeline
implements via `.ai-sdlc.yaml` at its root.

## Contents

| Artifact | Purpose | Issue |
|---|---|---|
| [`ai-sdlc.schema.json`](ai-sdlc.schema.json) | JSON Schema (draft 2020-12) for `.ai-sdlc.yaml` — repo contract: verify commands, autonomy ceiling, review policy, labels, model routing, escalation | #2 |
| [`labels.md`](labels.md) | Issue-label state machine — intake trio exclusivity, transition table, veto-pair matrix (`labels.state_machine`), janitor repair rule; `scc:*` flow vs `pr:*` states | #18 |
| [`trust-ladder.md`](trust-ladder.md) | Six-level progressive autonomy: levels, promotion gates, demotion triggers, freeze | #5 |
| [`definition-of-ready.md`](definition-of-ready.md) | DoR fields, AC format, atomicity rules, R1–R6 machine-checkable validation | #1 |
| [`risk-taxonomy.md`](risk-taxonomy.md) | pr:risk-* tiers + required-evidence matrix + merge-gate decision table | #3 |
| [`injection-defense.md`](injection-defense.md) | Trust-boundary model, fence+scrub convention, forbidden-action rules | #4 |
| [`org-policy.md`](org-policy.md) | Field-level merge, pin-able floor fields, resolution algorithm | #6 |
| [`postmortem-lifecycle.md`](postmortem-lifecycle.md) | Postmortem issue lifecycle — `signatureHash` canonical identity, open+closed dedup on a 90-day `closedAt` window, `POSTMORTEM_REOPEN_MIN` reopen threshold, runbook ingest on close | #20 |

## Versioning

The spec is semver-tagged (`spec/vX.Y.Z` tags). Workflows pin to a spec
version; breaking schema changes bump MAJOR and ship a migration note.

## Validation in CI

Any repo can validate its `.ai-sdlc.yaml` against the schema:

```bash
curl -sL https://raw.githubusercontent.com/os-santiago/ai-sdlc/main/spec/ai-sdlc.schema.json -o /tmp/ai-sdlc.schema.json
bash <(curl -sL https://raw.githubusercontent.com/os-santiago/ai-sdlc/main/tools/validate-ai-sdlc-config.sh) .ai-sdlc.yaml /tmp/ai-sdlc.schema.json
```

Or vendor `tools/validate-ai-sdlc-config.sh` and run it in a CI step —
it auto-resolves python3+PyYAML/jsonschema or npx fallbacks. Exit 0 = valid.

Examples: `../examples/library.ai-sdlc.yaml` (conservative, auto-merge-low
ceiling) and `../examples/internal-app.ai-sdlc.yaml` (aggressive,
auto-merge-all).
