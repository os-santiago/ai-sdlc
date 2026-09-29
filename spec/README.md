# spec/ — Normative AI-SDLC contract

Versioned, declarative surface that every repo opting into the pipeline
implements via `.ai-sdlc.yaml` at its root.

## Contents (planned)

| Artifact | Purpose | Issue |
|---|---|---|
| `ai-sdlc.schema.json` | JSON Schema for `.ai-sdlc.yaml` (transferred from adev #2) | see issues |
| `labels.md` | canonical state taxonomy — `scc-*` issue flow vs `pr:*` states | adev→here |
| `trust-ladder.md` | shadow/suggest/auto-PR/auto-merge-low/auto-merge-all/auto-deploy | adev #5 |
| `definition-of-ready.md` | atomicity contract for agent-consumable issues | adev #1 |
| `risk-taxonomy.md` | PR risk classes mapped to review/merge policy | adev #3 |
| `injection-defense.md` | sanitization of issue bodies, web content, dep docs | adev #4 |
| `org-policy.md` | org baseline `.ai-sdlc.yaml` with inheritance/overrides | adev #6 |

## Versioning

The spec is semver-tagged (`spec/vX.Y.Z` tags). Workflows pin to a spec
version; breaking schema changes bump MAJOR and ship a migration note.
