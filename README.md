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
  [headless contract](docs/engine_contract.md) (`--prompt-file`,
  `--audit-log`, `--summary-file`, `--no-commit`) can slot in.

## Adopt this runtime

A consumer repo needs three things:

1. **Contract** — `.ai-sdlc.yaml` at the repo root. Only `spec` is
   required; every other field resolves from runtime defaults (see
   `spec/ai-sdlc.schema.json`, archetypes under `examples/`):

   ```yaml
   spec: "1.0.0"
   autonomy:
     level: auto-merge-low   # conservative ceiling; raise as trust accrues
   ```

2. **Caller workflow** — `.github/workflows/ai-sdlc.yml` dispatching the
   reusable stages on `@main`:

   ```yaml
   name: ai-sdlc
   on:
     issues: { types: [labeled] }

   permissions:
     contents: write
     issues: write
     pull-requests: write
     # `models` is a workflow permission scope for accessing GitHub Models
     # (retired 2026-07-30) — it only gated the retired zero-secret path.
     # For current usage, set `MODEL_API_KEY` and omit this permission.

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

   Review, automerge, ci-repair, sweep and verify wire the same way —
   [docs/runtime.md](docs/runtime.md) has each stage's caller block.

3. **App + secrets** — install the `ai-sdlc` GitHub App on the repo and
   expose `AI_SDLC_APP_ID` + `AI_SDLC_APP_PRIVATE_KEY` to it (org secrets
   scoped to the consumer repos are the intended setup). Every mutation
   stage mints a per-run installation token — fail-closed, no PAT or
   `GITHUB_TOKEN` fallback, so pushes and merges always re-trigger CI.
   `MODEL_API_KEY` is required for inference: unset → the engine
   attempts `GITHUB_TOKEN` against GitHub Models, which was retired
   2026-07-30, so the run fails. Set it to a provider API key and
   configure a provider URL (the `provider_base_url` input or the
   `AI_SDLC_PROVIDER_URL` repository variable) pointing at a live
   OpenAI-compatible endpoint. Set the repo variable `AI_SDLC_OFF=1`
   to halt the pipeline without disabling workflows.

## Documentation

- [docs/runtime.md](docs/runtime.md) — `workflow_call` runtime stages
  reference for adopters.
- [docs/positioning.md](docs/positioning.md) — interop posture: the
  governance layer over pluggable engines, the conformance path, and
  the explicit non-goals.
- [docs/engine_contract.md](docs/engine_contract.md) — headless engine
  contract and certification: required flags, exit-code classes, adapter
  registry, conformance-suite guide, per-stage engine selection.
- [spec/README.md](spec/README.md) — index of the normative contract
  surface (schema, labels, trust ladder, DoR, risk taxonomy).
- [ADR-0001](docs/architecture/adr/0001-homedir-ai-sdlc-sunset-and-cutover.md) —
  homedir-ai-sdlc sunset and canonical-runtime cutover decision record.

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
