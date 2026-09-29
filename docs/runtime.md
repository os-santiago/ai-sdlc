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
| `model` | `auto/best-coding` | engine model route |
| `base` | `main` | base branch |
| `max_seconds` | `900` | engine wall-clock budget (exit 22 on cap) |
| `max_steps` | `200` | engine tool-step budget |

| Secret | Purpose |
|---|---|
| `AI_SDLC_TOKEN` | PAT/App token — required when `GITHUB_TOKEN` can't reach the repo or when PR-opened CI must trigger (`GITHUB_TOKEN`-auth pushes don't fire `pull_request`/`push` events) |
| `MODEL_API_KEY` | engine inference key (`OPENAI_API_KEY` surface) |

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

## Bounds

Every mutable step is bounded: job `timeout-minutes`, engine
`max-seconds`/`max-steps`, per-issue `concurrency` group
(`cancel-in-progress`). The audit log + run manifest land as workflow
artifacts (`ai-sdlc-run-<issue>`) for postmortem analysis.

## Note on workflow-file edits

`GITHUB_TOKEN`-auth pushes cannot commit files under `.github/workflows/`
— the implement job strips workflow-path changes from the agent diff
before committing (`git add -A -- ':!.github/workflows'`). Repo opt-in
workflow edits remain human-authored.
