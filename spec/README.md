# spec/ — Normative AI-SDLC contract

Versioned, declarative surface that every repo opting into the pipeline
implements via `.ai-sdlc.yaml` at its root.

## Contents

| Artifact | Purpose | Issue |
|---|---|---|
| [`ai-sdlc.schema.json`](ai-sdlc.schema.json) | JSON Schema (draft 2020-12) for `.ai-sdlc.yaml` — repo contract: verify commands, autonomy ceiling, review policy, labels, model routing, escalation | #2 |
| `labels.md` | canonical state taxonomy — `scc-*` issue flow vs `pr:*` states | adev→here |
| `trust-ladder.md` | shadow/suggest/auto-PR/auto-merge-low/auto-merge-all/auto-deploy | adev #5 |
| `definition-of-ready.md` | atomicity contract for agent-consumable issues | adev #1 |
| `risk-taxonomy.md` | PR risk classes mapped to review/merge policy | adev #3 |
| `injection-defense.md` | sanitization of issue bodies, web content, dep docs | adev #4 |
| `org-policy.md` | org baseline `.ai-sdlc.yaml` with inheritance/overrides | adev #6 |

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
