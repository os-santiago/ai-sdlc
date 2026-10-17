# AI-SDLC Runtime — reusable GitHub Actions

The pipeline stages as `workflow_call` workflows. A consuming repo drops a
thin caller workflow + `.ai-sdlc.yaml` and gets the autonomous loop.


## Decision Records

We maintain Architecture Decision Records (ADRs) in the [architecture/adr](./architecture/adr/) directory.
See [ADR-0001: Homedir AI-SDLC Sunset and Cutover](./architecture/adr/0001-homedir-ai-sdlc-sunset-and-cutover.md) for details on the homedir cutover.

See [ADR-0004: Cloud-native (Kubernetes/OpenShift) alternate control plane](./architecture/adr/0004-cloud-native-k8s-openshift-runtime.md)
for adopters who cannot run GitHub Actions. GHA stays the default below;
[`runtime/k8s/`](../runtime/k8s/README.md) is an opt-in alternative reading
the same `.ai-sdlc.yaml` contract and `spec/normalized-event.schema.json`
intake payload. See
[ADR-0005: Durable state, transactional outbox and engine invocation for runtime/k8s](./architecture/adr/0005-runtime-k8s-event-bus-and-engine-invocation.md)
for how that control plane's components are actually wired together
(PostgreSQL + outbox, no message broker) and its current limits (stub
engine only, no GitHub webhook adapter yet).

## Workflows

| Workflow | Stage | Entry |
|---|---|---|
| `ai-sdlc-intake.yml` | DoR validation (R1–R6 per `spec/definition-of-ready.md`), `scc:queued`/`scc:not-ready` labels | `issues.labeled` in caller |
| `ai-sdlc-implement.yml` | `scc` headless run → branch → PR (`Closes #N`) + manifest/audit artifacts | called on `scc:queued` |
| `ai-sdlc-automerge.yml` | merge-gate decision table (spec/risk-taxonomy.md): green+mergeable → squash+delete; failing → repair signal; pending/conflict → `needs-human`; pending review holds only up to `review.wait_max_minutes` → `review_timeout` merge | `check_run.completed` / `pull_request` / called |
| `ai-sdlc-classify.yml` | deterministic diff risk classification (`scripts/risk-classifier.js`): path/size/content signals → `pr:risk-*` label on every `pull_request` event — recomputed on each push | `pull_request` (opened/synchronize) |
| `ai-sdlc-review.yml` | AI review + bounded review-fix convergence: verdict marker per head SHA + `pr:risk-*` tier label; `blocker|major`/`request_changes` → fix pass on the same branch → re-review; `needs-human` after `max_fix_iterations` | `pull_request` (opened/synchronize) / called post-implement |
| `ai-sdlc-sweep.yml` | scheduled stall-sweep: enumerates open pipeline PRs and feeds each to `ai-sdlc-automerge.yml` — no green-idle PRs | `schedule` (every 30 min) / `workflow_dispatch` |
| `ai-sdlc-ci-repair.yml` | bounded repair loop on red checks: failing-check log tails → `scc` headless on the PR head → commit + push; one comment per attempt; `needs-human` after `max_repairs` | `check_run.completed` (failure) / `workflow_dispatch` / automerge `repair-signaled` |
| `ai-sdlc-verify.yml` | post-merge verify commands (resolved contract `verify.commands`); failure → canonical postmortem issue via `scripts/postmortem.js` — signature-deduped: open canonical comments, closed canonical reopens at `postmortem.reopen_min` (spec/postmortem-lifecycle.md) — or revert when enabled | post-merge |

Every stage first runs the `load-config` composite action
(`.github/actions/load-config/`) and acts only on the resolved contract —
see [Contract resolution](#contract-resolution). Every stage also appends a
hash-chained record to the run ledger and uploads the sealed file as an
artifact (`ai-sdlc-ledger-<stage>-…`) — see [Run ledger](#run-ledger).

## Contract resolution

`.ai-sdlc.yaml` is the runtime contract, not decoration. Each stage
resolves it before acting:

1. **Locate** `.ai-sdlc.yaml` at the target ref (sparse checkout of just
   that file in intake/automerge; the existing checkout in implement/verify).
2. **Validate** with `tools/validate-ai-sdlc-config.sh` against
   `spec/ai-sdlc.schema.json`, plus a secret scan (below).
3. **Resolve** every field with this precedence:

   **workflow input (override) > `.ai-sdlc.yaml` > org baseline > runtime defaults**

   The org baseline applies only when `org-config` is set — see
   [Org policy](#org-policy). Workflow inputs are *override-only*: an
   empty string (or `-1` for numeric inputs) means "unset", so the
   contract value wins. Field-level merge between contract layers
   (`spec/org-policy.md`): scalars replace; maps (`labels`,
   `model.routing_hints`) merge key-by-key; guard-rail lists
   (`triggers.skip_labels`, `merge.skip_labels`,
   `verify.required_checks`) are *unions* — strictly additive, a repo can
   add entries but never drop default or org entries; configuration
   lists (`verify.commands`, `authorized_labelers`, …) are replaced
   whole; `autonomy.level` is a ceiling — when the org baseline sets it,
   the resolved level is the *lower* of the org and repo values (repos
   can tighten, never loosen).
4. **Emit** step outputs and a `contract.json` artifact
   (`ai-sdlc-contract-<job>-<attempt>`, 14 days) recording the resolved
   values, the per-field source (`input` / `contract` / `org` /
   `org-pin` / `default`) and the org-policy status (below).

| Stage | Consumes | Overridable inputs |
|---|---|---|
| intake | `triggers.issue_label`, `triggers.skip_labels`, `labels.*` | `trigger_label` |
| implement | `model.primary`, `model.fallback`, `engine.max_seconds`, `engine.max_steps`, `labels.*` | `model`, `fallback_model`, `max_seconds`, `max_steps` |
| review | `review.mode`, `review.ai_reviewer.model`, `review.max_fix_iterations`, `review.external_reviewer`, `merge.skip_labels`, `engine.*`, `escalation.label` | `model`, `external_reviewer`, `max_fix_iterations`, `max_seconds`, `max_steps` |
| automerge | `merge.max_repairs`, `merge.pending_max_minutes`, `merge.skip_labels`, `merge.method`, `review.mode`, `review.wait_max_minutes`, `review.external_reviewer`, `escalation.label` | `max_repairs`, `pending_max_minutes`, `merge_method`, `review_wait_minutes` |
| verify | `verify.commands` | `verify_commands` |

The top-level `engines` map (issue #205) selects the certified adapter a
stage dispatches to — `engines.<stage>` may be a bare engine id or a
stage-definition object `{engine: <id>}`; both normalize to the same
resolved value in `contract.json`. A stage without an entry falls back to
`engine.name` (default `scc`). Stage dispatch to non-`scc` engines lands
with issue #206; until then the field is contract surface only.

`review.ai_reviewer.model` resolves to `model.primary` when unset **or**
when it names an OmniRoute route (`auto/*`, `devin/*`) — those are Hermes
control-plane only and unreachable from GHA runners
([ADR-0001](architecture/adr/0001-homedir-ai-sdlc-sunset-and-cutover.md)).

`labels.state_machine` (spec/labels.md) rides along inside `contract.json`'s
`labels` map — janitor/drift tooling reads it there; no runtime stage
consumes it yet.

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

### Org policy

`load-config` accepts an `org-config` input — the org baseline contract
per `spec/org-policy.md`. Format: **`owner/repo[/path][@ref]`** (the
`uses:`-style reference; path defaults to `.ai-sdlc.yaml`, ref to the
default branch). It is sparse-checked out with the `token` input, which
must read the org repo (an org-installed `AI_SDLC_TOKEN` covers this).
Empty `org-config` = repo contract + defaults, unchanged v1 behavior.

The baseline merges as a layer between defaults and the repo contract
with the collection semantics above: guard-rail lists union (org entries
can never be dropped), maps merge key-wise, `autonomy.level` clamps to
the org ceiling, and scalars are inherited unless the repo sets them.
The baseline may also declare floor fields repos cannot relax:

```yaml
# org baseline .ai-sdlc.yaml (e.g. os-santiago/.ai-sdlc)
spec: "1.0.0"
autonomy:
  level: auto-merge-low          # ceiling for every non-exempt repo
review:
  critical_requires_acceptance: true
policy_floor:
  pin:
    - autonomy.level
    - review.critical_requires_acceptance
    - merge.skip_labels
    - triggers.skip_labels
    - verify.required_checks
  exempt:
    - os-santiago/special-repo   # baseline skipped for this repo
```

- **Pins:** pinned scalars/maps take the org value outright; pinned union
  fields stay additive (org entries guaranteed); `autonomy.level` stays a
  ceiling — tightening below it is still allowed. Pins on fields the
  baseline does not set are recorded but inert.
- **Exempt:** `policy_floor.exempt` matches the target repo by
  `owner/repo` or bare repo name; exempt repos resolve repo + defaults.
- **Repo-side `policy_floor` is ignored** — floors are org-baseline only.
- **Fail-safe:** an unreachable or invalid baseline resolves
  repo + defaults with one warning annotation and
  `org_policy: unavailable` — a broken org file never bricks consumer
  pipelines. A malformed `org-config` input fails the step
  (`scc:config-error`).
- **Audit:** `contract.json` records `org_policy`
  (`applied` \| `exempt` \| `unavailable` \| `not-applied`), the baseline
  sha256 under `_source.org`, `pinned` (declared floors) and `enforced`
  (fields where a floor suppressed a repo value — `_sources` shows
  `org-pin`).

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

The calling repo must see `AI_SDLC_APP_ID` + `AI_SDLC_APP_PRIVATE_KEY`
(org secrets scoped to the consumer repos are the intended setup).
`implement`, `automerge` and `ci-repair` mint a per-run `ai-sdlc` GitHub
App installation token with `actions/create-github-app-token` and **fail
closed** when the secrets are absent — no PAT/`GITHUB_TOKEN` fallback, so
pushes and PRs always fire downstream workflows (issue #26).

## Inputs / secrets

### ai-sdlc-intake
| Input | Default | Purpose |
|---|---|---|
| `repo` | — (required) | owner/repo |
| `issue_number` | — (required) | issue to validate |
| `trigger_label` | `''` → `triggers.issue_label` | label that arms dispatch |
| `org_config` | `''` | org baseline `owner/repo[/path][@ref]` — see [Org policy](#org-policy) |

Outputs: `dispatch`, `ledger_head` (sealed chain head — feed it to the next
stage's `ledger_head` input).

### ai-sdlc-implement
| Input | Default | Purpose |
|---|---|---|
| `repo`, `issue_number` | — | target |
| `model` | `''` → `model.primary` | engine model route |
| `fallback_model` | `''` → `model.fallback` | one-shot fallback on provider/auth failure (exit 20/21) — see [Model fallback](#model-fallback) |
| `provider_base_url` | `''` → `vars.AI_SDLC_PROVIDER_URL` → legacy default | OpenAI-compatible inference endpoint — see [Provider resolution](#provider-resolution) |
| `base` | `main` | base branch (contract is read from here) |
| `max_seconds` | `-1` → `engine.max_seconds` | engine wall-clock budget per run (exit 22 on cap) |
| `max_steps` | `-1` → `engine.max_steps` | engine tool-step budget per run |
| `org_config` | `''` | org baseline `owner/repo[/path][@ref]` — see [Org policy](#org-policy) |
| `ledger_head` | `''` | upstream ledger head (e.g. `needs.intake.outputs.ledger_head`) — seeds the entry `prev`; empty = genesis |

| Output | Purpose |
|---|---|
| `exit_code` | final engine exit (fallback run when it fired, else primary) |
| `model` | model id of the run that produced `exit_code` |
| `primary_exit` | engine exit of the primary-model run |
| `fallback_used` | `true` when the fallback run fired |
| `pr_number` | PR opened by the run (empty when `exit_code != 0`) — feeds post-implement review |
| `ledger_head` | sealed chain head — also stamped on the PR as an `ai-sdlc:ledger-head` marker |

### ai-sdlc-review
| Input | Default | Purpose |
|---|---|---|
| `repo`, `pr_number` | — (required) | target |
| `base` | `main` | ref the contract is read from |
| `pipeline_author` | `ai-sdlc-runtime[bot]` | PR author login treated as pipeline-managed |
| `pipeline_label` | `ai-sdlc` | label that also marks a PR pipeline-managed (human-branch opt-in) |
| `model` | `''` → `review.ai_reviewer.model` → `model.primary` | reviewer model |
| `provider_base_url` | `''` → `vars.AI_SDLC_PROVIDER_URL` → legacy default | OpenAI-compatible inference endpoint — see [Provider resolution](#provider-resolution) |
| `external_reviewer` | `''` → `review.external_reviewer` | external reviewer slug prefix (e.g. `coderabbit`); unset = AI-only |
| `max_fix_iterations` | `-1` → `review.max_fix_iterations` (3) | fix pushes per PR before `needs-human` |
| `max_seconds` / `max_steps` | `-1` → `engine.*` | engine budgets per run (review AND fix) |
| `diff_max_bytes` | `150000` | byte cap on the diff embedded in the review prompt |
| `findings_max` | `20` | findings kept in the verdict record / fix prompt |
| `ledger_head` | `''` | upstream ledger head; empty = recover the `ai-sdlc:ledger-head` marker on the PR |

Outputs: `action` (`reviewed` \| `fix-dispatched` \| `escalated` \|
`skipped`), `verdict` (`clean` \| `blocked` \| `inconclusive` \| `none`),
`reason`, `risk_tier` (`low` \| `medium` \| `high` \| `critical`),
`ledger_head`.

### ai-sdlc-automerge
| Input | Default | Purpose |
|---|---|---|
| `repo`, `pr_number` | — (required) | target |
| `base` | `main` | ref the contract is read from |
| `max_repairs` | `-1` → `merge.max_repairs` | repair rounds before escalation |
| `pending_max_minutes` | `-1` → `merge.pending_max_minutes` | pending-checks cap before escalation |
| `merge_method` | `''` → `merge.method` | `squash` \| `merge` \| `rebase` |
| `org_config` | `''` | org baseline `owner/repo[/path][@ref]` — see [Org policy](#org-policy) |
| `review_wait_minutes` | `-1` → `review.wait_max_minutes` (15) | pending-review cap before merging with `review_timeout` |
| `ledger_head` | `''` | upstream ledger head; empty = recover the `ai-sdlc:ledger-head` marker on the PR |

### ai-sdlc-classify
| Input | Default | Purpose |
|---|---|---|
| `repo`, `pr_number` | — (required) | target |
| `runtime_ref` | `main` | ref of `os-santiago/ai-sdlc` the classifier (`scripts/risk-classifier.js`) is fetched from — pin to the same ref as the workflow; the script is never read from the PR head or consumer repo |
| `diff_max_bytes` | `500000` | byte cap on the unified diff scanned for content signals |

Outputs: `action` (`classified` \| `skipped`), `risk_tier` (`low` \|
`medium` \| `high` \| `critical`).

The stage is deliberately lean — no checkout, no engine, no contract
resolution — so event→label latency stays inside the taxonomy's labeling
budget. It applies the canonical `pr:risk-*` set in one bounded `gh pr
edit` (add winner, remove stale sibling tiers); `pr:risk-accepted` is
human-owned and never touched. Signals (`spec/risk-taxonomy.md`,
ambiguity resolves UP): credential/secret material or policy/supply-chain
surfaces → `critical`; `eval()`/`exec`/shell-injection shapes in added
diff lines, security-adjacent paths, dependency manifests, >800 diff
lines, multi-subsystem, public-API deletions → `high`; docs/tests/
config-text-only → `low`; otherwise `medium`.

### ai-sdlc-verify
| Input | Default | Purpose |
|---|---|---|
| `repo` | — (required) | target |
| `base` | `main` | branch verified (contract is read from here) |
| `verify_commands` | `''` → `verify.commands` | newline-separated commands |
| `setup_commands` | `''` | dependency install before verify |
| `revert_on_failure` | `false` | revert the merge commit on failure |
| `runtime_ref` | `main` | `os-santiago/ai-sdlc` ref to fetch `scripts/postmortem.js` from |
| `org_config` | `''` | org baseline `owner/repo[/path][@ref]` — see [Org policy](#org-policy) |
| `pr_number` | `0` | merged PR — recorded on the ledger entry and used for marker recovery |
| `ledger_head` | `''` | upstream ledger head; empty = recover the `ai-sdlc:ledger-head` marker on `pr_number` |

On verify failure the stage files a **canonical postmortem** through
`scripts/postmortem.js` (issue #191, spec/postmortem-lifecycle.md): the
failure signature `verify / post-merge-verify-failure / <failed commands>`
is hashed; an existing canonical issue gets a bounded recurrence comment
(reopened once post-close recurrences reach `postmortem.reopen_min`,
default 2; closed canonicals dedup for `postmortem.dedup_window_days`,
default 90) instead of a duplicate filing. When the filer can't be fetched
the stage fails open to a plain issue so the failure is never lost.

| Secret | Purpose |
|---|---|
| `AI_SDLC_APP_ID` | **required** by `implement`/`automerge`/`ci-repair`/`review`/`classify` — the `ai-sdlc` GitHub App id; each run mints a repo-scoped installation token (App permissions: contents/pull-requests/issues write, checks read, metadata read). Unset → the stage fails fast with `ai-sdlc auth` error — no PAT/`GITHUB_TOKEN` fallback (issue #26) |
| `AI_SDLC_APP_PRIVATE_KEY` | **required** — the App's PEM private key. Store both App secrets as org secrets scoped to the consumer repos |
| `AI_SDLC_TOKEN` | optional legacy slot — still accepted by `intake`/`verify`/`sweep` when `GITHUB_TOKEN` can't reach the target repo for read/issue calls. Unused by `implement`/`automerge`/`ci-repair` |
| `AI_SDLC_LEDGER_KEY` | optional — run-ledger signing key (PEM, Ed25519 or RSA). Seals are signed when set, unsigned otherwise; unsigned chains stay hash-linked but carry no cryptographic attribution. See [Run ledger](#run-ledger) |
| `MODEL_API_KEY` | API key for the resolved provider endpoint — **required for inference** since GitHub Models retired 2026-07-30. When absent the engine keys with `GITHUB_TOKEN` (legacy zero-secret path; `models: read`), which only GitHub Models ever accepted — see [Provider resolution](#provider-resolution) |

## Provider resolution

The implement step writes `~/.sc-agent/config.json` on the runner — the
engine's single source of truth (no env overrides; lesson carried from the
VPS worker: conflicting flag/env layers caused silent permission loss).
`implement`, `review` and `ci-repair` resolve the endpoint with the same
expression — first non-empty wins:

1. `provider_base_url` workflow input — per-call override.
2. `vars.AI_SDLC_PROVIDER_URL` — repository **variable** (Settings →
   Secrets and variables → Actions → *Variables* tab); the standing
   per-repo endpoint.
3. Legacy default `https://models.github.ai/inference`.

The API key resolves independently: `secrets.MODEL_API_KEY` when set, else
`GITHUB_TOKEN` (the zero-secret path; callers grant `models: read`).

**GitHub Models was retired 2026-07-30** — the legacy default endpoint no
longer serves inference, so `MODEL_API_KEY` **and** a provider URL (the
`provider_base_url` input or `AI_SDLC_PROVIDER_URL`) are both required,
pointing at a live OpenAI-compatible endpoint (NVIDIA NIM, OpenAI, …).
Left unset, the engine reaches the retired endpoint and exits
provider/auth-class (20/21 — one `model.fallback` retry per
[Model fallback](#model-fallback), then `scc:failed` + `needs-human`).

OmniRoute route names (`auto/*`, `devin/*`) are Hermes control-plane only —
unreachable from GHA runners by design
([ADR-0001](architecture/adr/0001-homedir-ai-sdlc-sunset-and-cutover.md)).
Per-repo model selection comes from the resolved contract
(`model.primary`/`model.fallback`); workflow inputs are the override
surface.

### ai-sdlc-ci-repair
| Input | Default | Purpose |
|---|---|---|
| `repo`, `pr_number` | — | target PR |
| `max_repairs` | `3` | attempts per PR before `needs-human` (mirror `.ai-sdlc.yaml` `merge.max_repairs`) |
| `model` | `nvidia/nemotron-3-super-120b-a12b` | engine model id (provider-native) |
| `provider_base_url` | `''` → `vars.AI_SDLC_PROVIDER_URL` → legacy default | OpenAI-compatible inference endpoint — see [Provider resolution](#provider-resolution) |
| `max_seconds` / `max_steps` | `900` / `200` | engine budgets (same contract as implement) |
| `branch_pattern` | `^(feat\|fix)/issue-[0-9]+` | ERE the head branch must match (pipeline PRs only); empty = any same-repo branch |
| `max_failed_checks` | `5` | failing checks whose output enters the prompt |
| `log_tail_lines` / `log_max_bytes` | `150` / `12000` | per-check log tail bounds |
| `ledger_head` | `''` | upstream ledger head; empty = recover the `ai-sdlc:ledger-head` marker on the PR |

Outputs: `action` (`repaired` \| `escalated` \| `skipped`), `reason` and
`ledger_head`.
Secrets: same as implement — `AI_SDLC_APP_ID` + `AI_SDLC_APP_PRIVATE_KEY`
are required (fail-closed). The pushed fix uses the App installation token,
so the new head re-triggers CI and the loop closes. Inference resolves the
same endpoint/key chain as implement — `MODEL_API_KEY` + a provider URL are
required since the GitHub Models retirement.

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
4. **Base-fault diagnosis** — a failing check whose log tail or check-run
   output carries `scc:config-error` means the contract failed validation
   on the BASE ref: every stage resolves `.ai-sdlc.yaml` from base, never
   from the PR head, so no push to the branch can green the run. The
   attempt is recorded as `base-contract-error` and escalated immediately —
   the engine never runs, converting a guaranteed `no_changes` stall into
   an explicit cause (issue #255).
5. **Repair** — checkout the PR head SHA (`persist-credentials: false`),
   same `scc` build + `Configure scc engine provider` step as implement,
   headless run with `--prompt-file --no-commit --audit-log --summary-file
   --max-seconds --max-steps`.
6. **Push** — real mutations (excluding `.github/workflows/`) are committed
   and pushed (non-force) to the PR head branch under the App installation
   token (`persist-credentials: false` + explicit auth header, so the token
   never reaches the workspace the engine reads); the new head re-runs CI,
   and the next red result triggers the next attempt.
7. **Audit** — exactly one marker comment per attempt (attempt N/M, failing
   checks, outcome, failure signature, run link) plus artifact
   `ai-sdlc-ci-repair-<pr>-attempt-<n>` with `ci-repair-manifest.json`
   (`failure: {signature, rootCause}` for future runbook matching), prompt,
   audit log and engine manifest.
8. **Escalate** — any attempt that cannot push a fix (engine no-op/failure,
   push rejected, base-ref contract fault) labels `needs-human` with the
   cause immediately, since nothing would re-trigger the loop. Removing the
   label hands control back.

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

## Review stage (`ai-sdlc-review.yml`)

Implements the *AI review required* rows of `spec/risk-taxonomy.md`
(issue #28) — Hermes `pr_reviewer.js` semantics plus optional external
reviewer ingestion (homedir `coderabbit-integration.sh`). One invocation =
at most one review pass for the current head SHA plus at most one fix
dispatch; the loop closes through `pull_request: synchronize` events, not
an in-run cycle.

1. **Guard** — skip unless the PR is open, not a draft, carries no skip
   label (`merge.skip_labels` + `escalation.label`), its head lives in the
   base repo (fork heads never run with secrets), and it is
   **pipeline-managed**: author == `pipeline_author` (the ai-sdlc App bot
   identity, `ai-sdlc-runtime[bot]` by default) **or** the PR carries
   `pipeline_label` (`ai-sdlc`, the opt-in for human branches) — match on
   author + label per the operator answer on issue #28. `review.mode:
   human` skips the whole stage. A head SHA that already has a verdict
   marker is skipped (idempotent re-polls); a *blocked* verdict on the
   head with no fix marker resumes at the fix dispatch — no second AI
   pass.
2. **External ingestion (optional)** — when `review.external_reviewer`
   names a slug prefix (e.g. `coderabbit`), the guard reads that
   reviewer's check-run conclusion on the head SHA, pull-request reviews
   (`CHANGES_REQUESTED`), and inline-comment count. A blocking external
   signal outranks the AI verdict (fail-safe, operator answer 4). Unset =
   AI-only path.
3. **Diff + risk tier** — checkout the head SHA (`persist-credentials:
   false`), bounded `git diff` vs the merge-base (`diff_max_bytes`), and a
   *deterministic* tier assignment from the file list/sizes/diff content
   by `scripts/risk-classifier.js` — the canonical classifier, fetched
   from the runtime repo pin, never from the head (the taxonomy says the
   tier SHOULD be computed from the diff, never self-declared; ambiguity
   resolves UP). The tier is applied as a `pr:risk-*` label; the
   standalone `ai-sdlc-classify` stage labels every `pull_request` event,
   pipeline-managed or not.
4. **AI review** — the diff is fenced as untrusted data
   (`spec/injection-defense.md`) in a diff-scoped prompt; `scc` runs
   headless (same engine contract as implement/ci-repair) and writes the
   machine verdict `ai-sdlc-review-verdict.json`
   (`{verdict: approve|request_changes|comment, findings: [{severity:
   blocker|major|minor, path, line, summary}]}`).
5. **Verdict** — `request_changes` or any `blocker|major` finding or an
   external blocking signal → `blocked`; `approve`/`comment` → `clean`;
   a missing/malformed verdict file → `inconclusive` (recorded, never
   silently blocks — review infra downtime cannot stall merges). The
   verdict is posted as a best-effort `gh pr review` (falls back to a
   comment on App-authored PRs, which cannot approve themselves) **and**
   as the canonical marker comment
   `<!-- ai-sdlc:review sha=<head> verdict=… -->` — the stateless
   per-head record the merge gate and future runs read.
6. **Fix dispatch** — `blocked` with iterations remaining → the `fix`
   job stamps `<!-- ai-sdlc:review-fix attempt=N sha=<head> -->`
   (at dispatch, so a dead run is detectable), runs `scc` with the
   findings prompt on the SAME head branch, commits real mutations
   (`.github/workflows/` stripped) and pushes under the App installation
   token — the new head re-triggers review. `blocked` with
   `attempts >= max_fix_iterations` (default 3) → `needs-human` + cause
   comment. A fix attempt that produces nothing pushable also escalates
   (nothing would re-trigger the loop).
7. **Merge gate interaction** — `ai-sdlc-automerge.yml` holds a
   mergeable PR while a verdict for the head SHA is pending
   (`review-pending`), while a blocked verdict awaits its fix
   (`review-blocked`/`review-fix-running`), or while a configured
   external reviewer is still running (`external-review-pending`) or
   blocking (`external-review-blocking`). Pending holds expire at
   `review.wait_max_minutes` (default 15) → merge proceeds and the PR
   records `review_timeout`; blocked/fix-running holds escalate to
   `needs-human` past their caps (`review-fix-stalled`,
   `review-fix-not-dispatched`, `external-review-blocking`) — the golden
   rule holds either way: no pipeline PR stays open unresolved.

Caller wiring (event plumbing is the caller's job):

```yaml
# .github/workflows/ai-sdlc.yml in the consuming repo
name: ai-sdlc
on:
  pull_request: { types: [opened, synchronize] }

jobs:
  review:
    # Fork heads carry no secrets — filter them at the caller so they skip
    # cleanly instead of failing the called workflow's auth check.
    if: github.event.pull_request.head.repo.full_name == github.repository
    uses: os-santiago/ai-sdlc/.github/workflows/ai-sdlc-review.yml@main
    with:
      repo: ${{ github.repository }}
      pr_number: ${{ github.event.pull_request.number }}
    secrets: inherit   # AI_SDLC_APP_ID + AI_SDLC_APP_PRIVATE_KEY
```

Post-implement chaining (alternative to the event path — `implement`
exposes `pr_number`):

```yaml
  review:
    needs: implement
    if: needs.implement.outputs.exit_code == '0'
    uses: os-santiago/ai-sdlc/.github/workflows/ai-sdlc-review.yml@main
    with:
      repo: ${{ github.repository }}
      pr_number: ${{ needs.implement.outputs.pr_number }}
    secrets: inherit
```

Artifacts: `ai-sdlc-review-<pr>-<attempt>` (verdict.json, prompt, bounded
diff, external signal, scc audit/manifest) and
`ai-sdlc-review-fix-<pr>-attempt-<n>` (fix manifest, prompt, audit log).

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
`os-santiago/ai-sdlc/.github/workflows/ai-sdlc-automerge.yml@main`. The gate
merges with the `ai-sdlc` App installation token, so merges fire downstream
`push` workflows (e.g. post-merge verify); the App secrets must be visible
to the repo running the sweep (`secrets: inherit`).

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
{"v": 1, "model": "nvidia/nemotron-3.5-lightning-30b-a3b", "exit_code": 0,
 "primary_model": "nvidia/nemotron-3-super-120b-a12b", "primary_exit": 20,
 "fallback_model": "nvidia/nemotron-3.5-lightning-30b-a3b", "fallback_used": true,
 "fallback_reason": "provider", "fallback_exit": 0,
 "budget_per_run": {"max_seconds": 900, "max_steps": 200}}
```

`fallback_model` will be fed from `.ai-sdlc.yaml` `model.fallback` once
contract resolution lands (#22); the input is the override surface today.

## In-run failover (SC_FAILOVER)\n\nThe engine's in-run failover chain (used when the primary model encounters a provider error during a run) is now derived from the resolved contract.\nSpecifically, it is set to the value of `model.fallback` from the contract, provided it is set and not identical to `model.primary`.\nIf the fallback is empty or equals the primary, then no in-run failover is configured (the engine will not retry on provider error beyond the one-shot fallback).\n\nThis replaces the previous hardcoded list of model IDs. The contract resolution step ensures that the failover chain is always validated against the\ncurrent configuration and warns if the failover ID is identical to the primary or a known-unserved id.\n\n[Note: The one-shot fallback (on exit 20/21) is still governed by the `fallback_model` input to the engine step, which is also set from `model.fallback`.\n  However, the in-run failover is a separate mechanism that allows the engine to retry a different model mid-run without exiting and resetting the workspace.]\n## Engine contract

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

## Run ledger

Tamper-evident per-stage run evidence (issue #196, epic #148). Every stage
appends a hash-chained record and seals the file at run completion:

- **Entry** (`append`): `{v, seq, stage, ts, repo, issue, pr, run, data,
  data_sha256, prev, hash}` — `data_sha256` is the SHA-256 of the
  canonicalized stage payload (step outputs plus digests of stage files
  under `data.files`); `prev` is the previous entry's `hash` (genesis =
  `0×64`, or an upstream stage's head); `hash` is the SHA-256 of the
  canonical entry.
- **Seal**: the last record of every ledger file — `{type: "seal", head,
  entries, alg, signed, key_id, sig}`. `head` is the last entry's `hash`;
  when the `AI_SDLC_LEDGER_KEY` secret is set the seal signs
  `{type: "ai-sdlc-run-ledger-seal", head, entries}` — Ed25519 (or
  RSA-SHA256 for RSA PEMs). Absent the secret the seal records
  `signed: false` — the chain stays tamper-evident, attribution needs the
  key.
- **Cross-stage chaining**: stages run in separate workflow invocations, so
  the chain links across runs through the `prev` seed. The caller passes
  `needs.<stage>.outputs.ledger_head` forward (`intake → implement` in
  `ai-sdlc.yml`); implement additionally stamps the sealed head on the PR
  as `<!-- ai-sdlc:ledger-head <64-hex> -->`, and the PR-scoped stages
  (review, automerge, ci-repair, verify) recover the *latest* trusted-author
  marker (`recover-prev`). Stages with no resolvable seed start a genesis
  segment — a fork or a gap is reported, never forged.
- **Artifacts**: each stage uploads
  `ai-sdlc-ledger-<stage>-<id>-<run_attempt>` (14-day retention, same as the
  other run artifacts). Ledger steps run `if: always()` +
  `continue-on-error` — evidence collection can never fail the pipeline.
- **Bounds**: per-file caps — `AI_SDLC_LEDGER_MAX_ENTRIES` (1000),
  `AI_SDLC_LEDGER_MAX_BYTES` (4 MiB), `AI_SDLC_LEDGER_MAX_DATA_BYTES`
  (64 KiB per entry), `AI_SDLC_LEDGER_MAX_FILES` (20) /
  `AI_SDLC_LEDGER_MAX_FILE_BYTES` (8 MiB) for hashed evidence files.

### Verifying a run offline

Download the `ai-sdlc-ledger-*` artifacts for the run set, then:

```bash
# single ledger
node .github/actions/run-ledger/run-ledger.js verify ledger-intake.jsonl \
  --pubkey-file ai-sdlc-ledger.pub.pem

# a whole bundle: files stitch into chains by prev→head matching
node .github/actions/run-ledger/run-ledger.js verify ./artifacts-dir/ \
  --pubkey-file ai-sdlc-ledger.pub.pem
```

`verify` recomputes every `data_sha256` and entry `hash`, checks `prev`
links and seq ordering, and verifies the seal signature and `key_id`
against the public key. Any modification — edited field, dropped or
reordered entry, re-signed seal — exits 1; warnings (unsigned seal,
unresolved cross-file links) exit 0 unless `--strict`.

### Signing key

```bash
node .github/actions/run-ledger/run-ledger.js keygen --out-dir /tmp/ledger-key
# private PEM → GitHub secret AI_SDLC_LEDGER_KEY (org secret scoped to
#   consumer repos — never committed)
# public  PEM → commit/publish; auditors pass it to verify --pubkey-file
```

The key is a dedicated secret (key separation from the App's
`AI_SDLC_APP_PRIVATE_KEY`); `secrets: inherit` callers pass it through
automatically once defined.

## Bounds

Every mutable step is bounded: job `timeout-minutes`, engine
`max-seconds`/`max-steps`, per-issue `concurrency` group
(`cancel-in-progress`), per-PR repair budget (`max_repairs`, queued
concurrency so every attempt records its comment), per-PR review-fix
budget (`review.max_fix_iterations`), the merge-gate review wait cap
(`review.wait_max_minutes`), and byte/finding caps on the review prompt
(`diff_max_bytes`, `findings_max`). The audit log + run manifest land as
workflow artifacts (`ai-sdlc-run-<issue>`, plus `ai-sdlc-run.json` and the
`*.primary.*` files when fallback fired) for postmortem analysis. Those
session files live in the workspace root during a run but are excluded
from the implement stage's `git add` (`:!scc-*.json*`, `:!ai-sdlc-run.json`,
`:!issue.json`, `:!task-prompt.md`) — and root-ignored in this repo's
`.gitignore` — so a run's commit only ever carries real source changes
(issue #68).

## Kill-switch (`AI_SDLC_OFF`)

`AI_SDLC_OFF` is the emergency stop, carried as a **repository variable**
(Settings → Secrets and variables → Actions → *Variables* tab — not a
secret). Set `AI_SDLC_OFF=1` to halt the pipeline: every stage checks the
variable at the top of the run and exits before any mutation, so nothing
new dispatches while in-flight runs finish on their own. The check is
**per run** — no runner teardown, no workflow disable; unset the variable
(or set `0`) and the next event resumes the loop. Prefer it over disabling
workflows during an incident — disabling also silences the sweep and the
merge gate, leaving pipeline PRs unresolved.

## Note on workflow-file edits

The `ai-sdlc` App holds `Workflows: no access`, so App-token pushes cannot
commit files under `.github/workflows/` — the implement job strips
workflow-path changes from the agent diff before committing
(`git add -A -- ':!.github/workflows'`). Repo opt-in workflow edits remain
human-authored.

## Decision records

Architecture decisions live under `docs/architecture/adr/`:

- [ADR-0001: homedir-ai-sdlc sunset and canonical-runtime cutover](architecture/adr/0001-homedir-ai-sdlc-sunset-and-cutover.md)
  — binding record for the homedir cutover (epic #30): `homedir-ai-sdlc`
  sunsets once its evidence gate passes, `os-santiago/homedir` migrates to
  this runtime, and OmniRoute route names (`auto/*`, `devin/*`) stay
  Hermes control-plane only.
