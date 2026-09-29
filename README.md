# ai-sdlc — Canonical AI Software Delivery Lifecycle Runtime

Repo-agnostic, GitHub-Actions-native implementation of the AI-SDLC pipeline:
from a labeled issue to a merged, verified pull request — with bounded
autonomy driven by a declarative per-repo contract (`.ai-sdlc.yaml`).

## Layered architecture

```
┌─────────────────────────────────────────────────────────┐
│ doctrine — why & how we build (book, patterns, evidence)│
│   Axel-DaMage/adev (methodology) · adev-pattern-bridge  │
├─────────────────────────────────────────────────────────┤
│ THIS REPO — spec + runtime (the missing piece)          │
│   spec/:  .ai-sdlc.yaml schema · label taxonomy ·       │
│           trust ladder · DoR · injection defense        │
│   actions/: reusable GH workflows (intake → implement → │
│             ci-repair → automerge → verify)             │
├─────────────────────────────────────────────────────────┤
│ engine — the agentic execution loop                     │
│   os-santiago/sc-agent-cli (provider-agnostic CLI)      │
├─────────────────────────────────────────────────────────┤
│ deployments — instances consuming this runtime          │
│   Axel-DaMage/hermes (Discord UX, OmniRoute, self-host) │
│   os-santiago/homedir-ai-sdlc (legacy VPS instance)     │
└─────────────────────────────────────────────────────────┘
```

## Design principles

- **Spec-first**: every behavior is declared in `.ai-sdlc.yaml`; the runtime
  never hardcodes per-repo policy.
- **GHA-native**: compute runs in GitHub Actions (`workflow_call` entrypoints);
  self-hosted control planes (Hermes) are an optional trigger surface.
- **Bounded autonomy**: trust ladder (shadow → suggest → auto-PR →
  auto-merge-low → auto-merge-all → auto-deploy) with per-repo ceilings.
- **Engine-agnostic contract**: sc-agent-cli today; any tool honoring the
  headless contract (`--prompt-file`, `--audit-log`, `--summary-file`,
  `--no-commit`) can slot in.

## Status

Early scaffold. The normative spec issues were transferred from
`Axel-DaMage/adev` — see the [issues tab](../../issues). First deliverable:
`workflow_call`-reusable `issue-implement` workflow driving `scc` headless.

## Non-goals

- Chat/notification UX (Discord etc.) — lives in deployment repos.
- Engine internals — upstream `sc-agent-cli`.
- Org-specific infrastructure (VPS, systemd) — stays in instance repos.

## License

Apache-2.0
