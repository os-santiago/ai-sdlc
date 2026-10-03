# AI-SDLC Runtime — reusable GitHub Actions

The pipeline stages as `workflow_call` workflows. A consuming repo drops a
thin caller workflow + `.ai-sdlc.yaml` and gets the autonomous loop.

## Workflows

| Workflow | Stage | Entry |
|---|---|---|
| `ai-sdlc-intake.yml` | DoR validation (R1–R6 per `spec/definition-of-ready.md`), `scc:queued`/`scc:not-ready` labels | `issues.labeled` in caller |
| `ai-sdlc-implement.yml` | `scc` headless run → branch → PR (`Closes #N`) + manifest/audit artifacts | called on `scc:queued` |
| `ai-sdlc-automerge.yml` | merge-gate decision table (spec/risk-taxonomy.md): green+mergeable → squash+delete; failing → repair signal; pending/conflict → `needs-human` | `check_run.completed` / `pull_request` / called |
| `ai-sdlc-sweep.yml` | scheduled stall-sweep: enumerates open pipeline PRs and feeds each to `ai-sdlc-automerge.yml` — no green-idle PRs | `schedule` (every 30 min) / `workflow_dispatch` |
| `ai-sdlc-verify.yml` | post-merge verify commands (resolved contract `verify.commands`); failure → issue (or revert when enabled) | post-merge |

Every stage first runs the `load-config` composite action
(`.github/actions/load-config/`) and acts only on the resolved contract —
see [Contract resolution](#contract-resolution).

## Contract resolution

`.ai-sdlc.yaml` is the runtime contract, not decoration. Each stage
resolves it before acting:

1. **Locate** `.ai-sdlc.yaml` at the target ref (sparse checkout of just
   that file in intake/automerge; the existing checkout in implement/verify).
2. **Validate** with `tools/validate-ai-sdlc-config.sh` against
   `spec/ai-sdlc.schema.json`, plus a secret scan (below).
3. **Resolve** every field with this precedence:

   **workflow input (override) > `.ai-sdlc.yaml` > runtime defaults**

   Workflow inputs are *override-only*: an empty string (or `-1` for
   numeric inputs) means "unset", so the contract value wins. Maps
   (`labels`) merge key-by-key; lists (`verify.commands`, skip labels) are
   replaced whole.
4. **Emit** step outputs and a `contract.json` artifact
   (`ai-sdlc-contract-<job>-<attempt>`, 14 days) recording the resolved
   values, the per-field source (`input` / `contract` / `default`) and
   `org_policy: not-applied`.

| Stage | Consumes | Overridable inputs |
|---|---|---|
| intake | `triggers.issue_label`, `triggers.skip_labels`, `labels.*` | `trigger_label` |
| implement | `model.primary`, `model.fallback`, `engine.max_seconds`, `engine.max_steps`, `labels.*` | `model`, `fallback_model`, `max_seconds`, `max_steps` |
| automerge | `merge.max_repairs`, `merge.pending_max_minutes`, `merge.skip_labels`, `merge.method`, `escalation.label` | `max_repairs`, `pending_max_minutes`, `merge_method` |
| verify | `verify.commands` | `verify_commands` |

`model.fallback` is used **once**, only when the primary route fails with a
provider error (engine exit 20); both attempts' audit logs are uploaded.

### Fail-fast

A missing or invalid contract fails the stage before any mutation, with an
`::error title=ai-sdlc contract (scc:config-error)::` annotation and the
validator output in `error.txt` (uploaded alongside `contract.json`).
Intake and implement additionally label the issue `scc:not-ready` and
comment the error, so nothing is dispatched on implicit defaults.

### Secrets are never contract values

The validator rejects credential-named keys (`apiKey`, `token`,
`password`, `secret`, `privateKey`, …) and credential-shaped values
(GitHub/OpenAI/Anthropic/NVIDIA/Slack/AWS/Google keys, private-key blocks,
JWTs, `Bearer …`, `user:pass@` URLs, inline `KEY=literal` assignments).
Only the offending path is reported, never the value. Environment
references such as `$TOKEN` / `${TOKEN}` are allowed — credentials belong
in GitHub secrets.

### Org policy (deferred)

v1 resolves **defaults → repo → input** only. The org-baseline layer of
`spec/org-policy.md` (floors/ceilings, union semantics, autonomy caps) is a
documented no-op hook: the `org-config` input is accepted, emits a
warning, and `contract.json` records `org_policy: not-applied`.

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
| `trigger_label` | `''` → `triggers.issue_label` | label that arms dispatch |

### ai-sdlc-implement
| Input | Default | Purpose |
|---|---|---|
| `repo`, `issue_number` | — | target |
| `model` | `''` → `model.primary` | engine model route |
| `fallback_model` | `''` → `model.fallback` | one-shot fallback on provider/auth failure (exit 20/21) — see [Model fallback](#model-fallback) |
| `provider_base_url` | `https://models.github.ai/inference` | OpenAI-compatible inference endpoint |
| `base` | `main` | base branch (contract is read from here) |
| `max_seconds` | `-1` → `engine.max_seconds` | engine wall-clock budget per run (exit 22 on cap) |
| `max_steps` | `-1` → `engine.max_steps` | engine tool-step budget per run |

| Output | Purpose |
|---|---|
| `exit_code` | final engine exit (fallback run when it fired, else primary) |
| `model` | model id of the run that produced `exit_code` |
| `primary_exit` | engine exit of the primary-model run |
| `fallback_used` | `true` when the fallback run fired |

### ai-sdlc-automerge
| Input | Default | Purpose |
|---|---|---|
| `repo`, `pr_number` | — (required) | target |
| `base` | `main` | ref the contract is read from |
| `max_repairs` | `-1` → `merge.max_repairs` | repair rounds before escalation |
| `pending_max_minutes` | `-1` → `merge.pending_max_minutes` | pending-checks cap before escalation |
| `merge_method` | `''` → `merge.method` | `squash` \| `merge` \| `rebase` |

### ai-sdlc-verify
| Input | Default | Purpose |
|---|---|---|
| `repo` | — (required) | target |
| `base` | `main` | branch verified (contract is read from here) |
| `verify_commands` | `''` → `verify.commands` | newline-separated commands |
| `setup_commands` | `''` | dependency install before verify |
| `revert_on_failure` | `false` | revert the merge commit on failure |

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
unreachable from GHA runners by design (ADR-0001). Per-repo selection comes
from the resolved contract (`model.primary`/`model.fallback`); workflow
inputs are the override surface.

## Stall-sweep (`ai-sdlc-sweep.yml`)

The merge gate is event-driven, so a missed event (or a PR that went green
while nothing was listening) would stall silently. The sweep closes that gap
with the GHA equivalent of Hermes' `pr_merger` poll. **Golden rule: no
pipeline PR stays open unresolved** — every one is either merged or carries
`needs-human` with a cause comment.

- **Trigger:** `schedule` at `7,37 * * * *` (every 30 min) plus
  `workflow_dispatch` (`pr_number` to sweep one PR, `max_prs` to shrink the
  bound). GitHub only runs `schedule` from the default branch and may delay
  it under load; public repos with 60 days of inactivity get scheduled
  workflows disabled.
- **Discovery:** open PRs on base `main` whose head branch matches
  `^(feat|fix)/issue-[0-9]+` (what `ai-sdlc-implement` pushes) or that carry
  the `ai-sdlc` label (opt-in for human-branch PRs). Drafts and skip labels
  (`needs-human`, `hold`, `do-not-merge`, `wip`) are dropped *before* the
  bound so human-owned PRs can't starve the scan; the gate re-checks them
  anyway. Least-recently-updated first, **max 20 per run**.
- **Gate:** a matrix job (`max-parallel: 4`, `fail-fast: false`) calls
  `ai-sdlc-automerge.yml` once per PR with `merge.*` settings from
  `.ai-sdlc.yaml`. The decision table lives only in the gate — the sweep
  never decides.
- **Idempotent repair signal:** the gate stamps each repair comment with
  `<!-- ai-sdlc:repair-signal sha=<head> -->`. A re-poll on the same head
  SHA holds (`repair-awaiting`) instead of re-commenting; no new push within
  `pending_max_minutes` → escalate (`repair-stalled`); `max_repairs`
  distinct SHAs signaled → escalate (`repairs-exhausted`).
- **Cost:** GitHub bills each job rounded up to a whole minute. One sweep
  is 1 min (discover) + 1 min per PR fed to the gate, so
  `48 × (1 + N)` runner-min/day for N actionable PRs (idle repo ≈ 48 min/day;
  hard ceiling `48 × 21` ≈ 1,008 min/day). Free on public repos.

### Consuming repos

Copy `ai-sdlc-sweep.yml` into the repo and point the `gate` job at
`os-santiago/ai-sdlc/.github/workflows/ai-sdlc-automerge.yml@main`. Merges
made with `GITHUB_TOKEN` don't fire `push` workflows (e.g. post-merge
verify) — provide `AI_SDLC_TOKEN` if that matters.

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
