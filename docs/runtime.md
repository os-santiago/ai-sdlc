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
| `ai-sdlc-ci-repair.yml` | bounded repair loop on red checks: failing-check log tails → `scc` headless on the PR head → commit + push; one comment per attempt; `needs-human` after `max_repairs` | `check_run.completed` (failure) / `workflow_dispatch` / automerge `repair-signaled` |
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
>>>>>>> origin/main

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
| `fallback_model` | `''` → `model.fallback` | one-shot fallback on provider failure (exit 20) |
| `provider_base_url` | `https://models.github.ai/inference` | OpenAI-compatible inference endpoint |
| `base` | `main` | base branch (contract is read from here) |
| `max_seconds` | `-1` → `engine.max_seconds` | engine wall-clock budget (exit 22 on cap) |
| `max_steps` | `-1` → `engine.max_steps` | engine tool-step budget |

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

### ai-sdlc-ci-repair
| Input | Default | Purpose |
|---|---|---|
| `repo`, `pr_number` | — | target PR |
| `max_repairs` | `3` | attempts per PR before `needs-human` (mirror `.ai-sdlc.yaml` `merge.max_repairs`) |
| `model` | `openai/gpt-4o` | engine model id (provider-native) |
| `provider_base_url` | `https://models.github.ai/inference` | OpenAI-compatible inference endpoint |
| `max_seconds` / `max_steps` | `900` / `200` | engine budgets (same contract as implement) |
| `branch_pattern` | `^(feat\|fix)/issue-[0-9]+` | ERE the head branch must match (pipeline PRs only); empty = any same-repo branch |
| `max_failed_checks` | `5` | failing checks whose output enters the prompt |
| `log_tail_lines` / `log_max_bytes` | `150` / `12000` | per-check log tail bounds |

Outputs: `action` (`repaired` \| `escalated` \| `skipped`) and `reason`.
Secrets: same as implement. `AI_SDLC_TOKEN` is required for the loop to
close — a fix pushed with `GITHUB_TOKEN` does not re-trigger CI (the attempt
comment flags this). Callers grant `models: read` for the GitHub Models
fallback.

## Repair loop (`ai-sdlc-ci-repair.yml`)

Implements the merge-gate row *checks red → repair loop (bounded) →
exhausted → ESCALATE* (`spec/risk-taxonomy.md`). One invocation = at most
one attempt:

1. **Guard** — skip unless the PR is open, not a draft, carries no skip
   label (`needs-human`/`hold`/`do-not-merge`/`wip`), its head lives in the
   base repo (fork heads never run with secrets), the head branch matches
   `branch_pattern`, and at least one check run (latest per name) or commit
   status on the head SHA is red. A head SHA already attempted is skipped.
2. **Budget** — attempts = `<!-- ai-sdlc:ci-repair attempt=N sha=… -->`
   markers on the PR from bots/collaborators. `attempts >= max_repairs` →
   `needs-human` + comment with cause, no engine run.
3. **Context** — failing check names + bounded log tails (Actions job logs;
   check-run output for other apps), wrapped as untrusted data in the
   repair prompt (`spec/injection-defense.md`).
4. **Repair** — checkout the PR head SHA (`persist-credentials: false`),
   same `scc` build + `Configure scc engine provider` step as implement,
   headless run with `--prompt-file --no-commit --audit-log --summary-file
   --max-seconds --max-steps`.
5. **Push** — real mutations (excluding `.github/workflows/`) are committed
   and pushed (non-force) to the PR head branch; the new head re-runs CI,
   and the next red result triggers the next attempt.
6. **Audit** — exactly one marker comment per attempt (attempt N/M, failing
   checks, outcome, failure signature, run link) plus artifact
   `ai-sdlc-ci-repair-<pr>-attempt-<n>` with `ci-repair-manifest.json`
   (`failure: {signature, rootCause}` for future runbook matching), prompt,
   audit log and engine manifest.
7. **Escalate** — any attempt that cannot push a fix (engine no-op/failure,
   push rejected) labels `needs-human` with the cause immediately, since
   nothing would re-trigger the loop. Removing the label hands control back.

Caller wiring (event plumbing is the caller's job):

```yaml
# .github/workflows/ai-sdlc-repair.yml in the consuming repo
name: ai-sdlc-repair
on:
  check_run: { types: [completed] }
  workflow_dispatch:
    inputs:
      pr: { type: number, required: true }

permissions:
  contents: write
  pull-requests: write
  issues: write
  actions: read
  checks: read
  statuses: read
  models: read

jobs:
  ci-repair:
    if: >-
      github.event_name == 'workflow_dispatch' ||
      (github.event.check_run.conclusion == 'failure' &&
       github.event.check_run.pull_requests[0] != null)
    uses: os-santiago/ai-sdlc/.github/workflows/ai-sdlc-ci-repair.yml@main
    with:
      repo: ${{ github.repository }}
      pr_number: ${{ github.event.inputs.pr || github.event.check_run.pull_requests[0].number }}
      max_repairs: 3
    secrets: inherit
```

The automerge gate's `repair-signaled` action points at this workflow as
its dispatch target.
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
(`cancel-in-progress`), per-PR repair budget (`max_repairs`, queued
concurrency so every attempt records its comment). The audit log + run manifest land as workflow
artifacts (`ai-sdlc-run-<issue>`) for postmortem analysis.

## Note on workflow-file edits

`GITHUB_TOKEN`-auth pushes cannot commit files under `.github/workflows/`
— the implement job strips workflow-path changes from the agent diff
before committing (`git add -A -- ':!.github/workflows'`). Repo opt-in
workflow edits remain human-authored.
