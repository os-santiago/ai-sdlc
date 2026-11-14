# Portable contract surface

These documents define the policy and event vocabulary consumed by the
Kubernetes/OpenShift runtime. They are not an implementation of GitHub
Actions.

| Asset | Purpose |
| --- | --- |
| `ai-sdlc.schema.json` | Contract schema for `.ai-sdlc.yaml` |
| `normalized-event.schema.json` | Portable work-item event shape |
| `definition-of-ready.md` | Admission criteria |
| `risk-taxonomy.md` | Deterministic risk classification vocabulary |
| `trust-ladder.md` | Bounded autonomy levels and evidence gates |
| `org-policy.md` | Organization policy floors |
| `labels.md` | Portable lifecycle labels |
| `injection-defense.md` | Untrusted-input handling rules |
| `kill-switch.md` | Emergency-stop semantics |
| `threat-model.md` | Security assumptions and boundaries |

GitHub Actions-specific execution contracts are preserved only by the
`deprecated-github-actions-runtime-2026-11-14` tag.
