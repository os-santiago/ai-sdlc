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

## Adopt this runtime

A consumer repo needs three things: a contract file, a thin caller
workflow, and the GitHub App.

1. **Contract** — commit `.ai-sdlc.yaml` at the repo root. Start from an
   archetype in [`examples/`](examples/) (library = conservative,
   internal-app = aggressive) and set `verify.commands` to the repo's
   real checks. Every stage validates it against
   [`spec/ai-sdlc.schema.json`](spec/ai-sdlc.schema.json) before acting —
   an invalid contract fails fast; nothing dispatches on implicit
   defaults.
2. **Caller workflow** — commit `.github/workflows/ai-sdlc.yml` wiring
   repo events to the reusable stages:

   ```yaml
   name: ai-sdlc
   on:
     issues: { types: [labeled] }

   permissions:
     contents: write
     issues: write
     pull-requests: write
     models: read   # GitHub Models default for the engine

   jobs:
     intake:
       if: github.event.label.name == 'ready-to-implement'
       uses: os-santiago/ai-sdlc/.github/workflows/ai-sdlc-intake.yml@main
       with:
         repo: ${{ github.repository }}
         issue_number: ${{ github.event.issue.number }}
       secrets: inherit

     implement:
       needs: intake
       if: needs.intake.outputs.dispatch == 'true'
       uses: os-santiago/ai-sdlc/.github/workflows/ai-sdlc-implement.yml@main
       with:
         repo: ${{ github.repository }}
         issue_number: ${{ github.event.issue.number }}
       secrets: inherit
   ```

   AI review, the merge gate, ci-repair and the stall-sweep wire up the
   same way (`uses: os-santiago/ai-sdlc/.github/workflows/*@main`) — see
   [docs/runtime.md](docs/runtime.md) for each stage's caller snippet.
3. **App + secrets** — install the `ai-sdlc` GitHub App on the repo and
   expose `AI_SDLC_APP_ID` + `AI_SDLC_APP_PRIVATE_KEY` to the caller
   (org secrets scoped to consumer repos are the intended setup). The
   mutating stages mint a per-run installation token and **fail closed**
   without them — no PAT fallback, so pushes and PRs always re-fire CI.
   `MODEL_API_KEY` is optional: set it for a direct provider, or leave it
   unset for the GitHub Models path (`GITHUB_TOKEN` + `models: read`).

Then create the trigger label once (`gh label create
ready-to-implement`) and apply it to a DoR-ready issue — intake validates
and queues it, implement opens the PR.

## Documentation

- [docs/runtime.md](docs/runtime.md) — `workflow_call` runtime stages
  reference for adopters.

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
