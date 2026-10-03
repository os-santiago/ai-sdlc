# AI-SDLC Runtime — reusable GitHub Actions

The pipeline stages as `workflow_call` workflows. A consuming repo drops a
thin caller workflow + `.ai-sdlc.yaml` and gets the autonomous loop.

## Workflows

| Workflow | Stage | Entry |
|---|---|---|
| `ai-sdlc-intake.yml` | DoR validation (R1–R6 per `spec/definition-of-ready.md`), `scc:queued`/`scc:not-ready` labels | `issues.labeled` in caller |
| `ai-sdlc-implement.yml` | `scc` headless run → branch → PR (`Closes #N`) + manifest/audit artifacts | called on `scc:queued` |
| `ai-sdlc-automerge.yml` | merge-gate decision table (spec/risk-taxonomy.md): green+mergeable → squash+delete; failing → repair signal; pending/conflict → `needs-human` | `check_run.completed` / `pull_request` / called |
| `ai-sdlc-verify.yml` | post-merge verify commands (from `.ai-sdlc.yaml` or input); failure → issue (or revert when enabled) | post-merge |

## Caller example

```yaml
# .github/workflows/ai-sdlc.yml in the consuming repo
name: ai-sdlc
on:
  issues: { types: [labeled] }

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

## Inputs / secrets

### ai-sdlc-intake
| Input | Default | Purpose |
|---|---|---|
| `repo` | — (required) | owner/repo |
| `issue_number` | — (required) | issue to validate |
| `trigger_label` | `ready-to-implement` | label that arms dispatch |

### ai-sdlc-implement
| Input | Default | Purpose |
|---|---|---|
| `repo`, `issue_number` | — | target |
| `model` | `openai/gpt-4o` | engine model id (provider-native) |
| `fallback_model` | `openai/gpt-4o-mini` | model for ONE bounded retry on provider/auth failure (exit 20/21); empty disables — see [Model fallback](#model-fallback) |
| `provider_base_url` | `https://models.github.ai/inference` | OpenAI-compatible inference endpoint |
| `base` | `main` | base branch |
| `max_seconds` | `900` | engine wall-clock budget per run (exit 22 on cap) |
| `max_steps` | `200` | engine tool-step budget per run |

| Output | Purpose |
|---|---|
| `exit_code` | final engine exit (fallback run when it fired, else primary) |
| `model` | model id of the run that produced `exit_code` |
| `primary_exit` | engine exit of the primary-model run |
| `fallback_used` | `true` when the fallback run fired |

| Secret | Purpose |
|---|---|
| `AI_SDLC_TOKEN` | PAT/App token — required when `GITHUB_TOKEN` can't reach the repo or when PR-opened CI must trigger (`GITHUB_TOKEN`-auth pushes don't fire `pull_request`/`push` events). The `ai-sdlc-runtime` GitHub App (#26) is the intended source |
| `MODEL_API_KEY` | optional — direct provider key. When absent, the engine falls back to `GITHUB_TOKEN` against GitHub Models (zero-secret path; callers must grant `models: read`) |

## Provider resolution

The implement step writes `~/.sc-agent/config.json` on the runner — the
engine's single source of truth (no env overrides; lesson carried from the
VPS worker: conflicting flag/env layers caused silent permission loss).
Resolution order:

1. `MODEL_API_KEY` set → `provider_base_url` + `model` as given (direct
   provider: NVIDIA, OpenAI, any OpenAI-compatible endpoint).
2. No `MODEL_API_KEY` → **GitHub Models** default (`models.github.ai`,
   `GITHUB_TOKEN` as key, caller needs the `models: read` permission).

OmniRoute route names (`auto/*`, `devin/*`) are Hermes control-plane only —
unreachable from GHA runners by design (ADR-0001). Per-repo selection will
come from `.ai-sdlc.yaml` `model.primary`/`fallback` once contract
resolution lands (#22); inputs are the override surface today.

## Model fallback

A single dead/quota-exhausted provider must not stall the pipeline. The
engine step runs `model` first; on a provider-class failure it gets
**exactly one** fresh retry with `fallback_model`:

| Primary exit | Action |
|---|---|
| 20 (provider) / 21 (auth) | one fallback run — unless `fallback_model` is empty or equals `model` |
| 0 / 10 / 22 (budget) / 23 (livelock) / 1 | final — never retried (not provider problems) |

The fallback run is fresh and bounded:

- primary artifacts are kept as `scc-audit.primary.jsonl` /
  `scc-manifest.primary.json`; the fallback writes the canonical names;
- the workspace is reset (`git reset --hard` + `git clean`, keeping the
  prompt/issue/`scc-*` files) so partial edits from the failed attempt
  don't leak into the retry;
- `~/.sc-agent/config.json` is re-rendered with the fallback model
  (`.model.model`) — still the single source of truth, no `SC_MODEL`;
- it gets its own `max_seconds`/`max_steps`. There is no loop: total engine
  time ≤ 2 × `max_seconds`, and the job `timeout-minutes` caps everything.

If the fallback also exits 20/21 the issue gets `scc:failed` **and**
`needs-human` plus a comment naming both models/exits (escalation). Any
other fallback exit is labelled like a primary exit.

Every run writes `ai-sdlc-run.json` (artifact + step summary; the PR body
quotes it):

```json
{"v": 1, "model": "openai/gpt-4o-mini", "exit_code": 0,
 "primary_model": "openai/gpt-4o", "primary_exit": 20,
 "fallback_model": "openai/gpt-4o-mini", "fallback_used": true,
 "fallback_reason": "provider", "fallback_exit": 0,
 "budget_per_run": {"max_seconds": 900, "max_steps": 200}}
```

`fallback_model` will be fed from `.ai-sdlc.yaml` `model.fallback` once
contract resolution lands (#22); the input is the override surface today.

## Engine contract

`sc-agent-cli` (`scc`) is built from source on the runner — it is not
published on npm. Headless flags used: `--prompt-file --no-commit
--audit-log --summary-file --max-seconds --max-steps`. Exit codes drive
issue labels:

| Code | Meaning | Label |
|---|---|---|
| 0 | implemented + PR opened | `scc:pr-opened` |
| 10 | no workspace mutations (clean no-op) | `scc:no-changes` |
| 20/21/22/23/1 | provider/auth/budget/livelock/fatal | `scc:failed` |
| 20/21 after fallback | provider/auth on both models | `scc:failed` + `needs-human` |

`exit_code` is the final run's code (see [Model fallback](#model-fallback)).

## Bounds

Every mutable step is bounded: job `timeout-minutes`, engine
`max-seconds`/`max-steps`, per-issue `concurrency` group
(`cancel-in-progress`). The audit log + run manifest land as workflow
artifacts (`ai-sdlc-run-<issue>`, plus `ai-sdlc-run.json` and the
`*.primary.*` files when fallback fired) for postmortem analysis.

## Note on workflow-file edits

`GITHUB_TOKEN`-auth pushes cannot commit files under `.github/workflows/`
— the implement job strips workflow-path changes from the agent diff
before committing (`git add -A -- ':!.github/workflows'`). Repo opt-in
workflow edits remain human-authored.
