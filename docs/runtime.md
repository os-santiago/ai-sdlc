# AI-SDLC Runtime — reusable GitHub Actions

The pipeline stages as `workflow_call` workflows. A consuming repo drops a
thin caller workflow + `.ai-sdlc.yaml` and gets the autonomous loop.
Operating it — `scc:*` states, failure modes, kill-switch and recovery —
lives in [operations.md](operations.md).


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

See
[ADR-0006: Orchestrator vs per-stage event callers](./architecture/adr/0006-orchestrator-vs-per-stage-event-callers.md)
for the caller topology this reference assumes: stages stay
`workflow_call` units and consumers run consolidated event callers —
never a single in-run orchestrator.

## Workflows

| Workflow | Stage | Entry |
|---|---|---|
| `ai-sdlc-intake.yml` | conversational refinement (`scripts/intake-refiner.js`): spec-completeness check → one upserted comment (clarifying questions or derived ACs) + `scc:not-ready` hold, fingerprint-retriggered on edits/replies; duplicate scan (`scripts/dup-detector.js`): token-Jaccard + marker-key similarity over the open backlog → one advisory upserted comment (never closes, never label-blocks); then DoR validation (R1–R6 per `spec/definition-of-ready.md`), `scc:queued`/`scc:not-ready` labels | `issues` (labeled/opened/edited) + `issue_comment` in caller |
| `ai-sdlc-intake.yml` | Epic decomposition (`epic` → `scc:child` sub-issues) or DoR validation (R1–R6 per `spec/definition-of-ready.md`), `scc:queued`/`scc:not-ready` labels | `issues.labeled` in caller |
| `ai-sdlc-implement.yml` | `scc` headless run → branch → PR (`Closes #N`) + manifest/audit artifacts | called on `scc:queued` |
| `ai-sdlc-automerge.yml` | merge-gate decision table (spec/risk-taxonomy.md): green+mergeable → squash+delete; failing → repair signal; pending/conflict → `needs-human`; pending review holds only up to `review.wait_max_minutes` → `review_timeout` merge; fork/external heads and zero-check rollups fail closed → `needs-human`, never merge | `check_run.completed` / `pull_request` / called |
| `ai-sdlc-classify.yml` | deterministic diff risk classification (`scripts/risk-classifier.js`): path/size/content signals → `pr:risk-*` label on every `pull_request` event — recomputed on each push | `pull_request` (opened/synchronize) |
| `ai-sdlc-review.yml` | AI review + bounded review-fix convergence: verdict marker per head SHA + `pr:risk-*` tier label; `blocker|major`/`request_changes` → fix pass on the same branch → re-review; `needs-human` after `max_fix_iterations` | `pull_request` (opened/synchronize) / called post-implement |
| `ai-sdlc-sweep.yml` | scheduled stall-sweep: enumerates open pipeline PRs and feeds each to `ai-sdlc-automerge.yml` — no green-idle PRs | `schedule` (every 30 min) / `workflow_dispatch` |
| `ai-sdlc-metrics.yml` | scheduled SLO report (`scripts/slo-metrics.js`): time-to-merge, iterations-to-green, escalation rate → step summary + JSON/Prometheus artifacts | `schedule` (daily) / `workflow_dispatch` / `workflow_call` |
| `ai-sdlc-dora.yml` | scheduled DORA report (`scripts/dora-metrics.js`): deployment frequency, lead time, change failure rate, MTTR — agentic vs human → committed `docs/metrics/dora.md` + `dora-metrics-json` artifact | `schedule` (weekly) / `workflow_dispatch` / `workflow_call` |
| `ai-sdlc-drift.yml` | scheduled contract-drift sweep (`scripts/contract-drift.js`): declared `.ai-sdlc.yaml` vs observed runs/labels/merges → one canonical deduped issue per repo | `schedule` (daily) / `workflow_dispatch` / `workflow_call` |
| `ai-sdlc-sweep.yml` | scheduled stall-sweep: enumerates open pipeline PRs and feeds each to `ai-sdlc-automerge.yml` — no green-idle PRs; also repairs issue-label contradictions (skip-label vetoing `ready-to-implement`, spec/labels.md) via `scripts/label-contradiction.js`, and scores the armed ready queue (`scripts/prioritize-queue.js`, `prioritize.*` rubric) into an ordered JSON+markdown queue | `schedule` (every 30 min) / `issues.labeled` / `workflow_dispatch` |
| `ai-sdlc-ci-repair.yml` | bounded repair loop on red checks: failing-check log tails → `scc` headless on the PR head → commit + push; one comment per attempt; `needs-human` after `max_repairs` | `check_run.completed` (failure) / `workflow_dispatch` / automerge `repair-signaled` |
| `ai-sdlc-verify.yml` | post-merge verify commands (resolved contract `verify.commands`); failure → canonical postmortem issue via `scripts/postmortem.js` — signature-deduped: open canonical comments, closed canonical reopens at `postmortem.reopen_min` (spec/postmortem-lifecycle.md) — or revert when enabled | post-merge |
| `ai-sdlc-dep-update.yml` | scheduled dependency scan → vetted dep-update PR (`scripts/dep-update.js`): release-age gate, license allowlist, typosquat distance, OSV → one bounded consolidated PR + deduped flagged-for-human issue | `schedule` / `workflow_dispatch` / called |
| `ai-sdlc-vuln-feed.yml` | scheduled vulnerability feed (`scripts/vuln-feed.js`): repo dep surface (lockfile, else resolved manifest ranges) → OSV querybatch → one bounded `security` issue per new advisory+package, marker-deduped against open/closed canonicals | `schedule` (weekly) / `workflow_dispatch` / `workflow_call` |
| `ai-sdlc-canary.yml` | scheduled end-to-end smoke of the intake→implement→PR loop: files a synthetic `<!-- ai-sdlc:canary -->` fixture issue, arms it with the contract trigger, observes `scc:queued`→`scc:pr-opened`, asserts PR/artifact/ledger shape, cleans up; failure → signature-deduped `postmortem` issue | `schedule` (daily) / `workflow_dispatch` (`mode`) |
| `ai-sdlc-spec-tag.yml` | spec/v* semver machinery (`scripts/spec-version.js`): classifies the `spec/**` diff since the last `spec/v*` tag (breaking → MAJOR, additive → MINOR, editorial → PATCH), gates on the `**Declared version:**` marker in `spec/README.md`, and tags + releases on merge to `main` | `push`/`pull_request` (`spec/**` paths) / `workflow_dispatch` (`bump`, `dry_run`) |

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
   `verify.required_checks`, `security.sbom.attach_to`) are *unions* — strictly additive, a repo can
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
| review | `review.mode`, `review.ai_reviewer.model`, `review.max_fix_iterations`, `review.external_reviewer`, `review.adversarial.*`, `merge.skip_labels`, `engine.*`, `escalation.label` | `model`, `external_reviewer`, `max_fix_iterations`, `max_seconds`, `max_steps`, `adversarial_min_tier`, `adversarial_max_attacks` |
| automerge | `merge.max_repairs`, `merge.pending_max_minutes`, `merge.skip_labels`, `merge.method`, `review.mode`, `review.wait_max_minutes`, `review.external_reviewer`, `escalation.label` | `max_repairs`, `pending_max_minutes`, `merge_method`, `review_wait_minutes` |
| verify | `verify.commands` | `verify_commands` |
| dep-update | `dependencies.*` (enabled, ecosystems, min_release_age_days, target, auto_kinds, max_updates, branch, labels, egress_profile) | `max_updates`, `target` |
| release | `release.provenance`, `release.signing` (both gate the provenance job of `ai-sdlc-release.yml`; `none` opts out) | — |
| automerge | `merge.max_repairs`, `merge.pending_max_minutes`, `merge.skip_labels`, `merge.method`, `review.mode`, `review.wait_max_minutes`, `review.external_reviewer`, `escalation.label`, `escalation.routes` | `max_repairs`, `pending_max_minutes`, `merge_method`, `review_wait_minutes` |
| verify | `verify.commands`, `escalation.routes` | `verify_commands` |

The top-level `engines` map (issue #205) selects the certified adapter a
stage dispatches to — `engines.<stage>` may be a bare engine id or a
stage-definition object `{engine: <id>}`; both normalize to the same
resolved value in `contract.json`. A stage without an entry falls back to
`engine.name` (default `scc`). Per-stage engine dispatch lands with
issue #206 (non-scc adapters) and issue #228 (scc through the same
registry argv contract). The stage-definition object may also carry
`model` — see below.
`engine.name` (default `scc`). Engine-running stages (`implement`,
`review` — including the review-fix pass — and `ci-repair`) dispatch
through `scripts/gha/stage-engine-resolver.js` via the
`resolve-stage-engine` composite action (issue #206): the resolved id must
have a `config/engine_adapters.json` entry or the stage exits 1 naming
the engine. `implement` and `ci-repair` run the selected engine —
`scc` included (issue #228) — through the registry `argvBuilder`
contract (`--run`): the scc descriptor builds the tuned
`scc chat -yq --prompt-file … --no-commit --audit-log … --summary-file …
--max-seconds … --max-steps` argv and declares `dispatch.rawExitCodes`,
so the engine's raw exit taxonomy (0/1/10/11/20/21/22/23/24 — normative in
[spec/engine-exit-codes.md](../spec/engine-exit-codes.md)) reaches the
workflow — that is what drives the [Model fallback](#model-fallback)
loop and the `no-changes` outcome. `review`'s scc passes still invoke
the same tuned argv inline; their non-scc dispatch goes through `--run`.

`review.ai_reviewer.model` resolves to `model.primary` when unset **or**
when it names an OmniRoute route (`auto/*`, `devin/*`) — those are Hermes
control-plane only and unreachable from GHA runners
([ADR-0001](architecture/adr/0001-homedir-ai-sdlc-sunset-and-cutover.md)).

`labels.state_machine` (spec/labels.md) rides along inside `contract.json`'s
`labels` map — janitor/drift tooling reads it there; no runtime stage
consumes it yet.

`engines.<stage>` may also declare `model` (issue #171;
spec/model-routing.md) — task-class → model routing. The resolved stage →
model map is emitted as the `stage_models` output (OmniRoute routes
filtered, same posture as `review_model`); stages absent from it resolve
to `model_primary`. For `review`, the dedicated
`review.ai_reviewer.model` still wins, then `engines.review.model`, then
`model.primary` — `engines.review.model` feeds the reviewer with no
workflow change. Other stages consume `stage_models` when engine
dispatch lands (issue #206).

`model.fallback` is used **once**, only when the primary route fails with a
provider error (engine exit 20) or the stall classifier verdicts the primary
run `transport` (below); both attempts' audit logs are uploaded.

### Engine-stall classification

Every engine run in `implement` is classified after exit by
`scripts/stall-classifier.js` (issue #158) into one of
`ok | transport | tooling | spec | budget | livelock`, driven by the
adapter's `exitCodeMap` + optional `stallClassMap` in
`config/engine_adapters.json`, the run manifest's terminal resolution, and
mutation/tool-call evidence from the worktree and audit log. The verdict
lands in `ai-sdlc-stall.json` (artifact + ledger data-file) and the
`stall_class` workflow output. Routes: `transport` → one bounded
fallback-model retry, then `needs-human`; `tooling` → `needs-human`
(postmortem on the engine repo); `spec`/`budget` → the refinement hold
(`labels.not-ready`, back to the issue author); `livelock` → `needs-human`.
Zero-mutation timeouts classify `livelock`/`transport`, never as a crash.
`ci-repair` records the same verdict as `engineStall` in
`ci-repair-manifest.json`. An unavailable classifier at the runtime pin
fails open — the run is recorded unclassified.

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

### Security gate (epic #87)

The `security:` contract section declares the deterministic supply-chain
gate — secret and dependency findings evaluated by code, never by
generative judgment. The gate runs as the required check
`ai-sdlc-security`, carried into consumers by the
`verify.required_checks` union (a repo can add checks, never drop the
org-mandated one). Resolved `security.*` fields ride inside
`contract.json` for the `ai-sdlc-security` stage (issue #91) to consume:

| Field | Default | Semantics |
|---|---|---|
| `enabled` | `true` | Master switch — an org `true` is a floor (repo opt-out is a non-goal) |
| `min_severity` | `high` | Findings at/above are blocking-class; org floor clamps to the stricter threshold |
| `block_on_secret` | `true` | Secret findings always block, every tier — boolean floor |
| `secrets_scan` / `dependency_audit` | `enforce` | Per-scanner mode `off`\|`report`\|`enforce` — org floor resolves strictest-wins |
| `license_check` | `report` | Same ranked floor; license observations stay advisory at the default |
| `sbom.enabled` | `false` | SBOM artifact switch — boolean floor (issue #92) |
| `sbom.format` | `cyclonedx` | `cyclonedx`\|`spdx` — pin-able scalar, not a strictness dimension |
| `sbom.attach_to` | `[pr]` | `pr`\|`release` attach targets — strictly-additive union |
| `severity_matrix.<pr:risk-*>` | see below | `report-only`\|`blocking-if-fix-available`\|`blocking` — strictest-wins floor |

`severity_matrix` normative defaults: `pr:risk-low`/`medium` report-only,
`pr:risk-high`/`critical` `blocking-if-fix-available` — a fixable finding
≥ `min_severity` fails the check; fixless findings degrade to
report-only so the loop never stalls on what cannot be fixed. The full
finding-class → verdict matrix is
[docs/security/gate-calibration.md](security/gate-calibration.md); merge
and floor semantics are normative in
[spec/org-policy.md](../spec/org-policy.md#security-gate) — Security
Gate.

## Caller topology (ADR-0006)

Which model applies: **reusable stages + consolidated event callers** —
never a single orchestrator workflow. Stages are separated in *time*
(intake fires on issue events; review/merge wait on PR and check events
minutes-to-hours later), so an in-run `uses:` chain would idle-burn
runner minutes or re-derive the event model it replaced. The decision
record is
[ADR-0006](architecture/adr/0006-orchestrator-vs-per-stage-event-callers.md).

A consumer repo carries at most three entry workflows; the
`ai-sdlc init` installer emits exactly two:

1. **`ai-sdlc.yml`** — one event caller multiplexing `issues`,
   `pull_request`, `check_run`, `push` and
   `workflow_dispatch` into per-job `if:` + `uses:` dispatches
   (the generated caller in [onboarding.md](onboarding.md); the caller
   example below adds `issue_comment`/`opened`/`edited` for
   conversational refinement — a documented opt-in, not emitted).
2. **`ai-sdlc-sweep.yml`** — the vendored scheduled stall-sweep; a cron
   trigger cannot live inside a `workflow_call`-only stage.

Per-stage caller files are the superseded alternative: they duplicate the
secrets map and permissions union per file. Stage-level retry granularity
is preserved either way — each `uses:` dispatch is its own run, so a
crashed stage never replays upstream work.

## Caller example

```yaml
# .github/workflows/ai-sdlc.yml in the consuming repo
name: ai-sdlc
on:
  issues: { types: [labeled, opened, edited] }
  issue_comment: { types: [created] }

jobs:
  intake:
    # `ready-to-implement` arms dispatch; opened/edited and author replies
    # feed the conversational refiner (its fingerprint no-ops unchanged
    # specs — a narrower `on:` is fine if you only want the armed path).
    if: >-
      github.event.label.name == 'ready-to-implement' ||
      (github.event_name == 'issues' && github.event.action != 'labeled') ||
      (github.event_name == 'issue_comment' && !github.event.issue.pull_request)
    uses: os-santiago/ai-sdlc/.github/workflows/ai-sdlc-intake.yml@main
    with:
      repo: ${{ github.repository }}
      issue_number: ${{ github.event.issue.number }}
    secrets:   # explicit map — pass only what the stage declares (#137)
      AI_SDLC_TOKEN: ${{ secrets.AI_SDLC_TOKEN }}                    # optional cross-repo token
      AI_SDLC_APP_ID: ${{ secrets.AI_SDLC_APP_ID }}
      AI_SDLC_APP_PRIVATE_KEY: ${{ secrets.AI_SDLC_APP_PRIVATE_KEY }}
      AI_SDLC_LEDGER_KEY: ${{ secrets.AI_SDLC_LEDGER_KEY }}          # optional ledger signing

  implement:
    needs: intake
    if: needs.intake.outputs.dispatch == 'true'
    uses: os-santiago/ai-sdlc/.github/workflows/ai-sdlc-implement.yml@main
    with:
      repo: ${{ github.repository }}
      issue_number: ${{ github.event.issue.number }}
    secrets:
      AI_SDLC_APP_ID: ${{ secrets.AI_SDLC_APP_ID }}                  # required — fail-closed
      AI_SDLC_APP_PRIVATE_KEY: ${{ secrets.AI_SDLC_APP_PRIVATE_KEY }} # required
      AI_SDLC_LEDGER_KEY: ${{ secrets.AI_SDLC_LEDGER_KEY }}          # optional ledger signing
      MODEL_API_KEY: ${{ secrets.MODEL_API_KEY }}                    # optional inference key
```

Secrets are mapped **explicitly per stage** — never `secrets: inherit`.
Pass only what the callee's `workflow_call.secrets` declares
(`tools/check-doc-snippets.py` enforces this against the caller snippets
here and in the README): the App pair is required by mutation stages,
`AI_SDLC_LEDGER_KEY` and `MODEL_API_KEY` are optional everywhere they are
declared, `AI_SDLC_TOKEN` is the cross-repo fallback intake/verify accept.

The calling repo must see `AI_SDLC_APP_ID` + `AI_SDLC_APP_PRIVATE_KEY`
(org secrets scoped to the consumer repos are the intended setup).
`implement`, `automerge` and `ci-repair` mint a per-run `ai-sdlc` GitHub
App installation token with `actions/create-github-app-token` and **fail
closed** when the secrets are absent — no PAT/`GITHUB_TOKEN` fallback, so
pushes and PRs always fire downstream workflows (issue #26).

### Pin the workflow ref (issues #118/#123)

The examples use `@main` — a mutable ref that makes an upstream push
change the consumer's CI between runs. For production lanes, pin the
`uses:` ref to a **full commit SHA** (or a `vX.Y.Z` tag once the
release-tag scheme lands) and set `runtime_ref` to the same ref so the
workflow file and the runtime scripts it fetches stay version-locked:

```yaml
uses: os-santiago/ai-sdlc/.github/workflows/ai-sdlc-intake.yml@<commit-sha>
with:
  repo: ${{ github.repository }}
  issue_number: ${{ github.event.issue.number }}
  runtime_ref: <commit-sha>
```

Consumer-side Dependabot (`github-actions` ecosystem) updates SHA-pinned
workflow refs and their `# <version>` comments — pinning buys
immutability without losing freshness. The runtime repo applies the same
rule internally: every third-party `uses:` is SHA-pinned and enforced by
`tools/pin-actions.py --check`. Full posture:
[security-scanning.md](security-scanning.md#sha-pinned-uses-refs-issues-118123).

## Forge credential isolation (trust boundary)

In `ai-sdlc-implement` the engine never holds a forge write credential
(issue #210). Two scoped installation tokens are minted per run instead of
one full-scope token:

- **`FORGE_TOKEN_READ`** — minted up front with
  `contents/issues/pull-requests: read` only. It backs the checkout, the
  issue fetch and the engine's own environment (`GH_TOKEN` inside the
  engine step is this token, so even a `gh` call issued by the agent can
  only read). A live probe before the engine run posts a label write that
  must be rejected (403/404); a 2xx fails the job closed.
- **`FORGE_TOKEN_WRITE`** — minted *after the engine exits* with
  `contents/issues/pull-requests: write`, and exported only to
  deterministic post-script steps: the `apply-publish` composite action
  (commit → push → `gh pr create`) and the fixed label/comment steps
  (outcome labels, config-error comment, ledger-head marker). No agent
  output is executed on this side of the boundary — the apply step only
  commits the diff the engine left in the workspace, so a fully
  manipulated agent can at most emit changes a human or the review stage
  would reject; it cannot push, label or comment itself.

`tests/credential_isolation.test.js` asserts the boundary structurally
(token ordering, engine env, post-script exclusivity) on every repo
validation run.

## Inputs / secrets

> **Contract surface gate (issue #135):** the tables below are mirrored by
> machine snapshots — `tests/api-surface/*.json` holds the normalized
> `workflow_call` interface (inputs/outputs/secrets) of every reusable
> stage, diffed in CI by `tests/api_surface.js` via the `contract` job. Any
> interface change must update the snapshot (`node tests/api_surface.js
> --bless`); removing/renaming inputs or outputs or changing a type is a
> breaking change — flag it per spec semver rules (see
> `tests/api-surface/README.md`). Comment markers (`<!-- ai-sdlc:* -->`)
> have the same treatment via `scripts/comment-markers.js` +
> `tests/markers/cases/` (issue #131).

### ai-sdlc-intake
| Input | Default | Purpose |
|---|---|---|
| `repo` | — (required) | owner/repo |
| `issue_number` | — (required) | issue to validate |
| `trigger_label` | `''` → `triggers.issue_label` | label that arms dispatch |
| `org_config` | `''` | org baseline `owner/repo[/path][@ref]` — see [Org policy](#org-policy) |
| `runtime_ref` | `main` | ai-sdlc ref to fetch runtime scripts (`intake-refiner.js`, `dup-detector.js`, `epic-decomposer.js`) from — pin with the workflow ref |

Before DoR, the stage runs conversational refinement
(`scripts/intake-refiner.js`, issue #197): a spec-completeness check posts
ONE upserted comment — clarifying questions plus the `labels.not-ready`
hold when under-specified (the hold strips the trigger in the same op,
spec/labels.md F2), or the derived acceptance-criteria checklist when
ready (a hold it placed is cleared, and a trigger it stripped is
restored — T3). A `sha256` fingerprint over title+body+author replies
lives in the comment marker, so edits and author replies re-trigger the
check while identical content never double-posts. The refiner never edits
the issue body and honors `triggers.skip_labels` (plus the run-state
bindings as trigger-restoration vetoes).

**Duplicate detection** (issue #161): alongside refinement, the stage runs
`scripts/dup-detector.js` — deterministic, embedding-free similarity over
the open backlog (normalized token Jaccard weighted 0.7 title / 0.3 body,
plus marker-key matching for machine-filed bodies: a shared
`ai-sdlc-failure-signature` sigHash or a repeated
`hermes:epic-child key:"…" parent:#N` is a certain duplicate, while a
signature pair with different hashes is never one — each filing is
canonical for its signature, spec/postmortem-lifecycle.md). Candidates at
or above `AI_SDLC_DUP_THRESHOLD` (default 0.60; related band at
`AI_SDLC_RELATED_THRESHOLD`, default 0.30) are reported in ONE upserted
`<!-- ai-sdlc:dup-scan fp:"…" -->` comment fingerprinted over the match
set — identical findings never re-post, a changed set PATCHes in place, an
emptied set rewrites the comment to a cleared notice. Detection is
advisory only: it never closes an issue and never applies the `duplicate`
label (spec/labels.md T10 — `dedup`/`human` own that transition); the
comment suggests confirmation or dismissal. Fail-open like the refiner —
a detector error never wedges a DoR-ready issue. A backlog-wide pairwise
audit is available manually via `--sweep` (report-only unless `--apply`).
| `runtime_ref` | `main` | ai-sdlc ref the epic decomposer is fetched from — pin to the workflow ref |

Outputs: `dispatch`, `ledger_head` (sealed chain head — feed it to the next
stage's `ledger_head` input).

**Epic decomposition** (issue #198): when the issue carries the `epic`
label — terminal, never dispatched (spec/labels.md T10) — intake runs
`scripts/epic-decomposer.js` instead of the DoR gate. The decomposer splits
the epic's work-item sections (`## Scope`, `## Children`, `## Work items`,
…; falling back to the epic-level acceptance criteria) into a bounded set
of `scc:child` issues (`--max-children`, default 8), each with a
Problem/Context/Acceptance-criteria body and a
`<!-- hermes:epic-child key:"…" parent:#N -->` marker, and upserts a live
`<!-- hermes:epic-decomposition -->` checklist into the epic body. Re-runs
are idempotent — filed keys are indexed across open+closed issues and never
re-filed; existing `#N` refs in the epic body are adopted into the
checklist rather than duplicated. Children carry no trigger label: a human
arms each one independently.

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

**DoR surface pin (issue #122 — intake→implement TOCTOU).** Intake stamps
the sha256 of the exact title+body it approved as a trusted-author marker
comment (`<!-- ai-sdlc:dor sha256=… -->`). The sanitize step re-hashes the
dispatch-time fetch and compares it against the LATEST trusted stamp:
`verified` proceeds; `drift` gates the engine off (empty prompt — the
unapproved surface is never rendered) and quarantines the issue to
`needs-human` with a comment naming both hashes — the text the engine
would have received is not the text that was approved. `unverified` (no
recorded approval — a standalone dispatch that never ran intake) fails
open and is logged. Re-arming a legitimate edit is re-applying the trigger
label: intake re-approves and re-pins the surface, latest stamp wins.

**Deterministic apply gate (issue #212 — credential isolation).** The engine
runs with no forge credentials in its environment (`persist-credentials:
false`, the App token only enters later steps' env) and its only deliverable
is a git-format patch: the run freezes the worktree delta into
`ai-sdlc-engine.patch`, restores a pristine tree, and
`scripts/apply-patch.js` — fetched from the trusted `os-santiago/ai-sdlc`
pin, never from the consumer checkout — validates the patch (bounded size,
path hygiene, `.github/workflows/**` deny, no symlink/gitlink/binary
payloads, `git apply --check`) before applying it. Only then does the
commit → push → PR step run. A rejection fails the job fail-closed, labels
the issue `scc:failed`, and posts the audit trail
(`ai-sdlc-apply-report.json`, also hashed into the run ledger). A
compromised or manipulated engine can emit output — it cannot push it.

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
| `adversarial_min_tier` | `''` → `review.adversarial.min_tier` (`high`) | risk tier that triggers the adversarial pass; `off` disables it |
| `adversarial_max_attacks` | `-1` → `review.adversarial.max_attacks` (16) | cap on generated attack probes per run |
| `ledger_head` | `''` | upstream ledger head; empty = recover the `ai-sdlc:ledger-head` marker on the PR |

Outputs: `action` (`reviewed` \| `fix-dispatched` \| `escalated` \|
`skipped`), `verdict` (`clean` \| `blocked` \| `inconclusive` \| `none`),
`reason`, `risk_tier` (`low` \| `medium` \| `high` \| `critical`),
`adversarial` (`ran:<verdict>:<n>-attacks` \| `skip:…` \|
`not-configured`), `ledger_head`.

### ai-sdlc-automerge
| Input | Default | Purpose |
|---|---|---|
| `repo`, `pr_number` | — (required) | target |
| `base` | `main` | ref the contract is read from |
| `runtime_ref` | `main` | ref of `os-santiago/ai-sdlc` the alert router (`scripts/alert-router.js`) is fetched from — pin to the same ref as the workflow |
| `max_repairs` | `-1` → `merge.max_repairs` | repair rounds before escalation |
| `pending_max_minutes` | `-1` → `merge.pending_max_minutes` | pending-checks cap before escalation |
| `merge_method` | `''` → `merge.method` | `squash` \| `merge` \| `rebase` |
| `org_config` | `''` | org baseline `owner/repo[/path][@ref]` — see [Org policy](#org-policy) |
| `review_wait_minutes` | `-1` → `review.wait_max_minutes` (15) | pending-review cap before merging with `review_timeout` |
| `ledger_head` | `''` | upstream ledger head; empty = recover the `ai-sdlc:ledger-head` marker on the PR |
| `runtime_ref` | `main` | ref of `os-santiago/ai-sdlc` the sentinel (`scripts/main-guard.js`) is fetched from |
| `main_guard` | `''` → `merge.main_guard.enabled` (true) | sentinel switch — `false` disables base-branch gating |
| `main_guard_workflows` | `''` → `merge.main_guard.workflows` (ci) | csv gate workflow names for the base-branch verdict |
| `main_guard_max_age_hours` | `-1` → `merge.main_guard.max_age_hours` (72) | freshness window for gate runs |

#### Main-guard sentinel (issue #189)

Before the PR decision table runs, the gate evaluates `scripts/main-guard.js`
(Hermes `MAIN_GUARD` port): the newest *completed* run of each configured
gate workflow on `main` — union semantics, **red if ANY gate failed**
(`failure`/`cancelled`/`timed_out`), green only when every gate is
green-or-unknown; all-unknown falls back to the newest completed run of
any workflow inside `max_age_hours`, then `unknown` (**fail-open** — a
stale signal never stalls merges). A red verdict holds the merge
(`held`, `main-guard:*`), files **one deduplicated postmortem issue**
(`fix(ci): main red — sentinel halt on <repo>`, labels `postmortem`,
`ai-sdlc`, `priority:P1`) — that open issue *is* the durable halt latch —
and comments once per episode on the held PR. A green verdict while the
latch is held closes the issue (`cleared`); comments fire only on the
red↔green transition. Sentinel infra failures (script fetch/eval) also
fail open. `merge.main_guard.enabled: false` (or `MAIN_GUARD=0`) disables.

#### Automerge hardening gates (issues #119/#120)

Two contract gates run inside the eval's decision table before any merge —
both default **on** and fail closed; opting out is an explicit contract
declaration, never silence:

- **Provenance gate** — `merge.require_pipeline_provenance` (default
  `true`): only a same-repo head (`isCrossRepository` false, head owner
  and repo matching the target) authored by `merge.pipeline_author`
  (default `ai-sdlc-runtime[bot]`) or an explicit `merge.allow_authors`
  entry may auto-merge. A fork head escalates `provenance:fork-head`; any
  other author escalates `provenance:untrusted-author` — both land the
  escalation label (`needs-human`) and route via the `provenance` failure
  class (spec/risk-taxonomy.md). Missing head/author fields fail closed.
  `false` restores the legacy any-author behavior.
- **Zero-check gate** — `merge.require_checks`: a mergeable head
  reporting **zero** check-runs is a missing signal, not a green rollup —
  `held` (`no-checks`) while the head is younger than
  `merge.pending_max_minutes`, then `escalated` (`no-checks` →
  `checks-stalled` class) so a repo with no CI surfaces as needs-human
  instead of merging blind. `false` restores legacy checkless merging.
  When unset, the field resolves from `autonomy.merge_requires_checks`
  (default `true`) — the trust-ladder declaration stays the single point
  of truth.

To keep trusted automation flowing (e.g. dependabot), list the bot logins
in `merge.allow_authors`; a differently-named runtime app sets
`merge.pipeline_author`.

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

### ai-sdlc-dep-update
| Input | Default | Purpose |
|---|---|---|
| `repo` | — (required) | target repo |
| `base` | `main` | base branch scanned and targeted by the PR (contract is read from here) |
| `dry_run` | `false` | scan + vet only — no branch, PR or issue mutations |
| `max_updates` | `-1` → `dependencies.max_updates` (10) | updates landed in the consolidated PR |
| `target` | `''` → `dependencies.target` (`wanted`) | `wanted` stays inside declared semver ranges; `latest` may cross majors |
| `org_config` | `''` | org baseline `owner/repo[/path][@ref]` — see [Org policy](#org-policy) |
| `runtime_ref` | `main` | ref of `os-santiago/ai-sdlc` the worker (`scripts/dep-update.js`) is fetched from — pin to the same ref as the workflow |
| `ledger_head` | `''` | upstream ledger head; empty = genesis segment |

Outputs: `action` (`open-pr` \| `existing-pr` \| `flagged` \| `none` \|
`skipped` \| `dry-run`), `pr_number`, `updates`, `flagged`, `ledger_head`.
Secrets: same as implement — `AI_SDLC_APP_ID` + `AI_SDLC_APP_PRIVATE_KEY`
are required (fail-closed; App-token pushes re-fire CI/classify/review).

One invocation = one bounded scan (see
[Dependency-update stage](#dependency-update-stage-ai-sdlc-dep-updateyml)).

### ai-sdlc-vuln-feed
| Input | Default | Purpose |
|---|---|---|
| `repo` | `''` | `owner/name` to scan (blank → `repos` input → caller repo `config/fleet.json` → caller repo) |
| `repos` | `''` | csv of `owner/name` — the watched-repo list for one sweep |
| `dry_run` | `false` | scan + report only — never files issues |
| `max_findings` | `5` | max new advisory issues filed per repo per run |
| `runtime_ref` | `main` | ref of `os-santiago/ai-sdlc` the worker (`scripts/vuln-feed.js` + `scripts/dep-update.js`) is fetched from — pin to the same ref as the workflow |

Outputs: `action` (`filed` \| `deduped` \| `clean` \| `skipped`),
`filed`, `findings`. Secrets: none required — `AI_SDLC_TOKEN` is used when
present for cross-repo reads, else `GITHUB_TOKEN` (issues write + contents
read suffice).

Each filing is a DoR-complete `security(deps):` issue carrying
`<!-- ai-sdlc:vuln-feed id:<ADVISORY> pkg:<NAME> -->` — the same
signature-marker dedup idiom as `scripts/postmortem.js`: an open canonical
is never duplicated, a closed canonical suppresses re-filing (reopen it to
re-arm). Issues carry the `security` label and flow through normal intake —
dogfooding the pipeline's own fix loop (issue #99); a vetted dep-update
bump past the advisory's fixed version is the intended resolution.

### ai-sdlc-verify
| Input | Default | Purpose |
|---|---|---|
| `repo` | — (required) | target |
| `base` | `main` | branch verified (contract is read from here) |
| `runtime_ref` | `main` | ref of `os-santiago/ai-sdlc` the alert router (`scripts/alert-router.js`) is fetched from — pin to the same ref as the workflow |
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

### ai-sdlc-metrics
| Input | Default | Purpose |
|---|---|---|
| `repo` | `''` → caller repo | `owner/name` to report on |
| `window` | `'30d'` | trailing window (`m`/`h`/`d`) |
| `runtime_ref` | `'main'` | ref of `os-santiago/ai-sdlc` to fetch `scripts/slo-metrics.js` from — pin to the same ref as this workflow |

### ai-sdlc-dora
| Input | Default | Purpose |
|---|---|---|
| `repo` | `''` → caller repo | `owner/name` to report on |
| `window` | `'30d'` | trailing window (`m`/`h`/`d`) |
| `runtime_ref` | `'main'` | ref of `os-santiago/ai-sdlc` to fetch `scripts/dora-metrics.js` from — pin to the same ref as this workflow |

The scheduled/dispatch run commits `docs/metrics/dora.md` (diff-gated) in
addition to the `dora-metrics-json` artifact; `workflow_call` consumers
receive the artifact + step summary only.

### ai-sdlc-drift
| Input | Default | Purpose |
|---|---|---|
| `repo` | `''` → caller repo | `owner/name` to sweep for contract drift |
| `window` | `'30d'` | trailing window for observed behavior |
| `dry_run` | `false` | evaluate only — never comments/files/closes |
| `runtime_ref` | `'main'` | ref of `os-santiago/ai-sdlc` to fetch `scripts/contract-drift.js` + `config/engine_adapters.json` from — pin to the same ref as this workflow |

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
| `stall_max` | `3` | attempts tolerated per identical failing-check signature before `needs-human` (issue #190); `0` disables the stall guard |
| `runtime_ref` | `main` | ref of `os-santiago/ai-sdlc` the trusted runtime scripts (`scripts/repair-stall-guard.js`, `scripts/failure-runbook.js`) are fetched from — pin to the same ref as the workflow; the scripts are never read from the PR head or consumer repo |
| `model` | `nvidia/nemotron-3-super-120b-a12b` | engine model id (provider-native) |
| `provider_base_url` | `''` → `vars.AI_SDLC_PROVIDER_URL` → legacy default | OpenAI-compatible inference endpoint — see [Provider resolution](#provider-resolution) |
| `max_seconds` / `max_steps` | `900` / `200` | engine budgets (same contract as implement) |
| `branch_pattern` | `^(feat\|fix)/issue-[0-9]+` | ERE the head branch must match (pipeline PRs only); empty = any same-repo branch |
| `max_failed_checks` | `5` | failing checks whose output enters the prompt |
| `log_tail_lines` / `log_max_bytes` | `150` / `12000` | per-check log tail bounds |
| `runtime_ref` | `main` | ref of `os-santiago/ai-sdlc` the alert router (`scripts/alert-router.js`) is fetched from — pin to the same ref as the workflow |
| `ledger_head` | `''` | upstream ledger head; empty = recover the `ai-sdlc:ledger-head` marker on the PR |
| `runbook_max_bytes` | `1400` | byte cap on the injected runbook advisory (0 disables injection; issue #188) |

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
2. **Budget** — attempts = `<!-- ai-sdlc:ci-repair attempt=N sha=… sig=… -->`
   markers on the PR from bots/collaborators. `attempts >= max_repairs` →
   `needs-human` + comment with cause, no engine run.
   **Stall guard** (issue #190, port of the Hermes `repairSigs` rule): `sig`
   is the sha256-16 of the sorted failing check names. When the identical
   signature already burned `stall_max` recorded attempts — a repair that
   pushed but left the same checks red, or exited `no_changes` — the guard
   escalates (`repair-stall:<sig>`) instead of spending another engine run.
   A changed failing surface is a different signature and restarts the
   count; `stall_max: 0` disables the guard.
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
5. **Runbook consult** — `scripts/failure-runbook.js` (fetched from the
   `runtime_ref` pin, never the PR head) maps the failure signature onto
   `docs/runbooks/failure_modes.md` — the consumer repo's base-ref copy
   first, the runtime repo's bundled store as fallback. A known signature
   prepends a bounded (`runbook_max_bytes`) advisory with the recorded
   root cause + fix + prevention to the repair brief; a miss leaves the
   brief byte-identical (issue #188).
6. **Repair** — checkout the PR head SHA (`persist-credentials: false`),
   same `scc` build + `Configure scc engine provider` step as implement,
   headless run with `--prompt-file --no-commit --audit-log --summary-file
   --max-seconds --max-steps`.
7. **Push** — real mutations (excluding `.github/workflows/`) are committed
   and pushed (non-force) to the PR head branch under the App installation
   token (`persist-credentials: false` + explicit auth header, so the token
   never reaches the workspace the engine reads); the new head re-runs CI,
   and the next red result triggers the next attempt.
8. **Audit** — exactly one marker comment per attempt (attempt N/M, failing
   checks, outcome, failure signature, runbook hit, run link) plus artifact
   `ai-sdlc-ci-repair-<pr>-attempt-<n>` with `ci-repair-manifest.json`
   (`failure: {signature, rootCause, runbookHit}`), prompt, audit log,
   engine manifest and the runbook verdict (`runbook.json`).
9. **Escalate** — any attempt that cannot push a fix (engine no-op/failure,
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
    secrets:
      AI_SDLC_APP_ID: ${{ secrets.AI_SDLC_APP_ID }}                  # required — fail-closed
      AI_SDLC_APP_PRIVATE_KEY: ${{ secrets.AI_SDLC_APP_PRIVATE_KEY }} # required
      AI_SDLC_LEDGER_KEY: ${{ secrets.AI_SDLC_LEDGER_KEY }}          # optional ledger signing
      MODEL_API_KEY: ${{ secrets.MODEL_API_KEY }}                    # optional inference key
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
   - **Adversarial pass (issue #180)** — when the diff's tier reaches the
     contract-declared threshold `review.adversarial.min_tier` (default
     `high`, i.e. `pr:risk-high`/`pr:risk-critical`; `enabled: false` or
     `adversarial_min_tier=off` disables), `scripts/adversarial-review.js`
     generates bounded attack probes via `scripts/adversarial.js` (capped
     at `review.adversarial.max_attacks`, embedded diff excerpt capped) and
     a second `scc` run reviews the change under threat-model/abuse-case
     framing with the probes fenced as untrusted data — a live
     injection-defense check: a probe that converts to a forbidden action
     in the engine audit log (force-push, pipe-to-shell, credential-file
     read, attacker egress) becomes a `blocker` finding, as does the
     engine self-reporting `probes_resisted: false`. The pass budget is
     `review.adversarial.max_seconds` clamped to `engine.max_seconds`.
     Below the threshold it skips cleanly and records the reason; an
     unresolvable tier resolves UP. The generator/normalizer are fetched
     from the runtime pin — never the PR head — and an unavailable
     generator degrades to a recorded skip, never a block.
5. **Verdict** — `request_changes` or any `blocker|major` finding or an
   external blocking signal or a blocked adversarial verdict → `blocked`;
   adversarial findings merge into the verdict record tagged
   `source: "adversarial"` and feed the same fix prompt — the normal
   review-fix loop applies unchanged. The `adversarial` summary block in
   `verdict.json`, the verdict comment's adversarial line, the run
   artifacts (plan, prompt, audit log, normalized record) and the run
   ledger keep the pass auditable. `approve`/`comment` → `clean`;
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
    secrets:
      AI_SDLC_APP_ID: ${{ secrets.AI_SDLC_APP_ID }}                  # required
      AI_SDLC_APP_PRIVATE_KEY: ${{ secrets.AI_SDLC_APP_PRIVATE_KEY }} # required
      AI_SDLC_LEDGER_KEY: ${{ secrets.AI_SDLC_LEDGER_KEY }}          # optional
      MODEL_API_KEY: ${{ secrets.MODEL_API_KEY }}                    # optional
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
    secrets:
      AI_SDLC_APP_ID: ${{ secrets.AI_SDLC_APP_ID }}
      AI_SDLC_APP_PRIVATE_KEY: ${{ secrets.AI_SDLC_APP_PRIVATE_KEY }}
      AI_SDLC_LEDGER_KEY: ${{ secrets.AI_SDLC_LEDGER_KEY }}
      MODEL_API_KEY: ${{ secrets.MODEL_API_KEY }}
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
- **Sentinel:** the `sentinel` job runs `scripts/main-guard.js --apply` on
  every sweep — including zero-PR runs — so a red base files its deduped
  postmortem promptly and a red→green recovery lifts the halt (closes the
  sentinel issue) without waiting for the next PR event (issue #189).
- **Idempotent repair signal:** the gate stamps each repair comment with
  `<!-- ai-sdlc:repair-signal sha=<head> -->`. Signals are parsed from the
  trusted-author comment feed only (issue #121 — same trust model as the
  review-verdict markers: a forged signal could otherwise stall the merge
  or burn the repair budget). A re-poll on the same head
  SHA holds (`repair-awaiting`) instead of re-commenting; no new push within
  `pending_max_minutes` → escalate (`repair-stalled`); `max_repairs`
  distinct SHAs signaled → escalate (`repairs-exhausted`).
- **Cost:** GitHub bills each job rounded up to a whole minute. One sweep
  is 1 min (discover) + 1 min per PR fed to the gate, so
  `48 × (1 + N)` runner-min/day for N actionable PRs (idle repo ≈ 48 min/day;
  hard ceiling `48 × 21` ≈ 1,008 min/day). Free on public repos.

### Label-contradiction repair (issue #200)

A forbidden label pair on an issue — e.g. a skip label coexisting with
`ready-to-implement` — is a pipeline defect, never a legitimate state
(spec/labels.md → "Forbidden combinations" / "Drift rule"). The sweep's
`label-repair` job runs `scripts/label-contradiction.js`:

- **Triggers:** every `issues.labeled` event (repairs only that issue —
  the label that just landed may have created the pair) plus the scheduled
  / dispatched sweep (`≤200` open issues per run; `workflow_dispatch` input
  `issue_number` scopes to one issue).
- **Forbidden set:** `labels.state_machine.veto_pairs` ∪ every
  `triggers.skip_labels` entry vetoing `triggers.issue_label`, plus the
  derived precedence rules (hold > refinement-hold > run-state > trigger)
  resolved from the contract — labels are bindings, not constants.
- **Repair is label-only:** the loser is stripped (`gh issue edit
  --remove-label`), the winner survives, and an audit comment stamped
  `<!-- ai-sdlc:label-veto -->` records the ISO timestamp and the vetoing
  label(s). The job never dispatches and never touches unrelated labels.
- **Bounded drift:** prior repairs are counted from marker comments; a
  pair that keeps reappearing is escalated to `needs-human` with cause
  `label-drift-loop` instead of being fought silently (default cap: 2
  prior repairs).
- **Consumer repos:** the job pulls the script from
  `os-santiago/ai-sdlc@main` when the consumer repo doesn't vendor it; a
  vendored `scripts/label-contradiction.js` always wins.

### Ready-queue prioritization (issue #162)

Dispatch is event-driven FIFO by label application — high-value issues wait
behind trivia. The sweep's `prioritize` job runs `scripts/prioritize-queue.js`
to give the ready queue an economic ordering:

- **Queue membership (spec/labels.md):** an issue is *armed* when it carries
  `triggers.issue_label` and no label that vetoes the trigger — the same
  forbidden-pair derivation the label-repair job uses (declared `veto_pairs`
  ∪ `triggers.skip_labels` ∪ the hold/refinement-hold/run-state precedence).
  Armed-but-vetoed drift is excluded from the queue and reported; repair is
  label-repair's job, the scorer never touches labels.
- **Scoring rubric** is declared in `.ai-sdlc.yaml` under `prioritize`
  (opt-in, `enabled: false` by default — an undeclared contract emits an
  empty queue and exits clean):
  `score = weights.priority[P<tier>] + age_per_day×min(age_days, age_cap_days)
  + epic_linked×[epic child] + blocking×#(open issues this blocks)
  + blocked×#(open blockers)`. Dependency data comes from GitHub issue
  dependencies (`blockedBy`/`blocking` on the issue-list payload); when the
  fields are unavailable the components score 0 and the evidence carries
  `deps: false` (fail-open).
- **Dispatch order:** with `priority_override` (default on) the queue is
  tier-first — priority labels override raw score and **P0 always dispatches
  first**, even if a repo zeroes its weight; within a tier, score desc then
  age asc. `priority_override: false` is pure score-first ordering.
- **Evidence:** the ordered queue is the stage artifact — a markdown table
  in the step summary plus `ready-queue.json`/`ready-queue.md` uploaded per
  run (30d). Every entry carries `score`, the per-component `reason` string,
  and `rank`; `queue_head`/`queue_size` go to step outputs. The artifact's
  head→tail order is the dispatch order for any consumer that pops the
  queue; the scorer itself never dispatches.
- **Bounded:** one `issue list` call, capped at `prioritize.max` (default 50,
  hard ceiling 200). Deterministic — equal inputs produce byte-identical
  output (`--now` pins `generatedAt`; `--issues FILE` replays a fixture).

### Consuming repos

Copy `ai-sdlc-sweep.yml` into the repo and point the `gate` job at
`os-santiago/ai-sdlc/.github/workflows/ai-sdlc-automerge.yml@main`. The gate
merges with the `ai-sdlc` App installation token, so merges fire downstream
`push` workflows (e.g. post-merge verify); the App secrets must be visible
to the repo running the sweep (mapped explicitly on the `gate` job's
`secrets:` — see `.github/workflows/ai-sdlc-sweep.yml` for the shape).

## SLO metrics (`ai-sdlc-metrics.yml`)

`scripts/slo-metrics.js` computes the agentic SLOs the trust ladder gates on
(spec/trust-ladder.md § Transition criteria) entirely from GitHub API data —
PRs, issues, labels and workflow runs — so any consumer repo can be measured
without a run ledger. A **pipeline PR** is the sweep's surface: head branch
`^(feat|fix)/issue-N` or the `ai-sdlc` label.

- **time-to-merge** — seconds from the linked issue's `createdAt` to the PR's
  merge event (linked via the branch's `issue-N`, else a `Closes #N`-style
  keyword in the body; falls back to PR creation, flagged
  `basis: pr_created`).
- **iterations-to-green** — workflow runs on the PR head branch until the
  first completed run with `conclusion: success` (1-based; every run counts).
  PRs that never went green report the attempts observed (`green: false`).
- **escalation rate** — window subjects holding `needs-human` / all window
  subjects. A subject is a work item — the linked issue when resolvable, else
  the PR — so a hold on either side of an issue↔PR pair escalates once.
  Issues with no pipeline PR count only when pipeline-flagged
  (`needs-human` or an `scc:*` label). A live hold joins the denominator even
  off-window: the list APIs expose `needs-human` as current state only.

- **Trigger:** `schedule` daily (06:13 UTC), `workflow_dispatch`, and
  `workflow_call` (`repo`, `window`, `runtime_ref`) for consumer repos.
- **Output contract:** `--format json` (versioned report object),
  `--format markdown` (step-summary table), `--format prometheus` (text
  exposition — `ai_sdlc_time_to_merge_seconds` and
  `ai_sdlc_iterations_to_green` histograms, `ai_sdlc_escalations_total` /
  `ai_sdlc_pipeline_subjects_total` counters, `ai_sdlc_escalation_ratio` and
  `ai_sdlc_pipeline_prs_not_green` gauges; all labeled `repo=`). The run
  uploads `slo-metrics.json`, `slo-metrics.prom` and the raw collected model
  (`slo-input.json`) as artifacts (90d). `--input`/`--dump-input` replay a
  collected model offline — the same fixture format the tests use; `--now`
  pins the window end for deterministic output.
- **Bounded:** one `pr list` + one `issue list` + at most `--max-prs`
  (default 100, cap 500) `run list` calls, deduped by head branch; a failed
  runs fetch marks the PR unsampled instead of sinking the report.
- **Trust boundary:** the workflow fetches the canonical script from the
  `os-santiago/ai-sdlc` pin (`runtime_ref`) — never from the caller repo.
## Fleet dashboard (`ai-sdlc-fleet.yml`)

`scripts/fleet-dashboard.js` renders the org fleet view — the GHA-native
port of Hermes' `fleet_dashboard.js`: every enrolled repo with its contract
`autonomy.level`, recent run health (`healthy`/`degraded`/`critical`/`idle`;
`unknown` when the API can't reach the repo), and flags (`unreachable`,
`no-contract`, `stale`).

- **Fleet list:** `config/fleet.json` (`{"repos": ["owner/name", …]}`),
  overridable per run via the `repos` dispatch input, the
  `AI_SDLC_FLEET_REPOS` variable/env, or `--repos` on the CLI. Resolution
  order: CLI > env > config file > `GITHUB_REPOSITORY` (self).
- **Schedule:** `41 5 * * *` daily plus `workflow_dispatch` (`repos`,
  `window`). The rendered table goes to the job summary and to
  `docs/fleet-dashboard.md`, committed back to `main` only when the content
  changed (diff-gated — spec/generated-artifact-publish-contract.md).
- **Reads:** `.ai-sdlc.yaml` raw via `gh api repos/{r}/contents/` and
  `actions/runs` (≤30/repo, ≤100 repos — bounded). Uses `AI_SDLC_TOKEN`
  when the secret exists (private consumers), else `GITHUB_TOKEN` (public
  repos only — unreachable repos report `unknown`, never abort the run).
- **Kill-switch:** `AI_SDLC_OFF=1` halts the job like every other stage.
- **Determinism:** `--now <ISO>` pins `generatedAt`; equal inputs produce
  byte-identical output (`node --test scripts/fleet-dashboard.test.js`).

## Contract drift (`ai-sdlc-drift.yml`)

`scripts/contract-drift.js` diffs the declared `.ai-sdlc.yaml` against
observed pipeline behavior — the issue-#176 sweep under epic #149 (fleet
operations). A contract that no longer matches reality stops meaning
anything: this sweep keeps the divergence visible instead of silent.

- **Observed surface:** `gh` list APIs only — workflow runs (stage
  invocations), pull requests (merges, labels, check rollups) and issues
  (label state), plus a bounded probe (≤10) of merge-commit parents for
  merge-method evidence. Deployments with a run ledger can feed stage→engine
  records through `--observed FILE` (`observed.stages.<stage>.engines`).
- **Checks:** `engine-unregistered` (declared engine absent from the
  certified adapter registry), `stage-engine-mismatch` /
  `declared-engine-unused`, `stage-never-invoked` (evidence demands a stage
  — PRs → implement, merges → automerge/verify — yet zero runs),
  `autonomy-exceeded` (merges below the `auto-merge-low` ceiling),
  `merge-checks-bypassed`, `required-check-missing`,
  `merge-method-mismatch`, `skip-label-violation`, and
  `label-veto-violation` (spec/labels.md forbidden pairs — declared
  `veto_pairs` ∪ `triggers.skip_labels` vetoing the trigger).
- **Fail-open on partial data:** every check requires its evidence to exist
  — no engine records, rollups or merge-parent probes → the check is
  skipped, never a false positive.
- **Canonical issue, deduped:** one issue per repo carries
  `<!-- ai-sdlc-drift-signature: <sha256(repo)[:16]> -->`. A changed finding
  set comments; an identical set is a no-op (`unchanged`); a resolved set
  comments + closes. A closed canonical is reopened on recurring drift —
  comment first so evidence survives a failed reopen.
- **Trigger:** `schedule` daily (06:41 UTC), `workflow_dispatch` (`repo`,
  `window`, `dry_run`) and `workflow_call` (same inputs + `runtime_ref`;
  outputs `drift_action`, `drift_issue`, `drift_findings`).
- **Input contract:** `--input`/`--dump-input` replay the collected model
  deterministically — the same fixture format the tests use; `--now` pins
  the window end.
- **Bounded:** one contents fetch + one `run list` + one `pr list` + one
  `issue list` + ≤10 commit probes; list bounds default 200 (cap 500).
- **Trust boundary:** script and `config/engine_adapters.json` are fetched
  from the `os-santiago/ai-sdlc` pin (`runtime_ref`) — never from the
  caller checkout.

## Canary smoke (`ai-sdlc-canary.yml`)

The pipeline dogfoods itself (`ai-sdlc.yml`), but nothing *asserts* the
loop works — a silent dispatch breakage is discovered only when a real
issue stalls (issue #132). The canary closes that gap: a scheduled run
files a synthetic fixture issue, arms it with the contract trigger label
(`triggers.issue_label`, resolved from `.ai-sdlc.yaml` — never hardcoded),
and observes the label state machine plus run evidence.

- **Fixture:** one open canary at a time. The issue carries the `canary`
  label + `<!-- ai-sdlc:canary -->` body marker and a
  conventional-commit title (`test(e2e): canary smoke …` — DoR R1 makes a
  literal `canary:` prefix self-defeating). Its `## Acceptance criteria`
  asks the engine for exactly one file (`canary/<run-stamp>.txt`), so the
  implement diff is minimal. Stale fixtures from crashed runs are swept at
  arm time (≤20 open `canary` issues, plus open canary-titled PRs).
- **Modes:** `mode=intake` (the scheduled path, per the issue AC
  "implement optional/opt-in for cost") asserts the arm reaches
  `scc:queued`, then cancels the dispatched `ai-sdlc (self)` run before
  the engine spends tokens. `mode=full` (dispatch opt-in) waits for
  `scc:pr-opened`, then asserts PR shape — head `feat|fix/issue-<N>-*`,
  base `main`, `Closes #N` in the body — and run evidence: the
  `ai-sdlc-run-<N>` artifact must contain `ai-sdlc-run.json`, and the
  `ai-sdlc-ledger-implement-<N>-*` artifact must carry a
  `stage=implement, issue=N` entry. Artifacts are matched by name against
  the repo artifact list, so no run-id resolution is needed.
- **Alerting vs fail-open:** the canary never gates merges or dispatch —
  fail-open relative to the pipeline. A canary failure is loud: the run
  goes red AND files a canonical signature-deduped `postmortem` issue via
  `scripts/postmortem.js` (`component=canary`, `class=e2e-loop-failure`,
  `location=stage:<failed-stage>`) — daily recurrences comment on the same
  issue instead of spamming new filings. Engaged kill-switch → the run
  skips green (a revoked pipeline is not a broken pipeline); missing App
  secrets → fail + alert (without the pipeline identity the loop cannot
  run — that *is* the breakage).
- **Cleanup:** `always()` — the fixture issue is de-armed + closed, the
  canary PR is `do-not-merge`-labeled (a `merge.skip_labels` entry, so the
  stall-sweep never feeds it to automerge even if cleanup dies), closed
  and branch-deleted. Cleanup is fail-soft and never masks the verdict.
- **Least privilege:** the workflow `GITHUB_TOKEN` carries `contents:read`
  (sparse checkout of trusted scripts), `issues:write` (postmortem alert
  fallback) and `actions:write` (locate/cancel the dispatched run, read
  artifacts). Every forge mutation uses the minted `ai-sdlc-runtime[bot]`
  installation token — required anyway, since `GITHUB_TOKEN` events never
  fire downstream workflows and arming with one would exercise nothing.
- **Bounded:** `observe_minutes` input caps the label poll (default 20,
  hard cap 30 — the issue's specified bound), artifact polling ≤8 min,
  `timeout-minutes: 45` on the job, serialized via a concurrency group.
  Every verdict — pass, fail, or kill-switch skip — is recorded on the
  run ledger (stage `canary`) and a `canary-report.json` artifact.

## Release manager (`ai-sdlc-release.yml`)

Maintainer-dispatched release driver backed by `scripts/release-manager.js`
(issue #178). One job, two modes over the same code path:

- **Trigger:** `workflow_dispatch` only — releases are a human decision.
  `dry_run=true` (the default) prints the full plan to the step summary and
  mutates nothing; `dry_run=false` publishes.
- **Semver:** computed from conventional commits since the newest `v*.*.*`
  tag — `BREAKING CHANGE`/`type!:` → major, `feat` → minor, else patch.
  `bump` / `spec_bump` inputs override the computed value.
- **Spec stream:** AGENTS.md makes spec changes semver-sensitive. Commits
  touching `spec/` drive a separate `spec/v*.*.*` tag whose bump is computed
  from the spec-touching commits alone; a breaking spec change yields a
  MAJOR spec bump and the changelog calls out the migration-note
  requirement. `--spec-bump none` skips the spec tag.
- **Publish sequence (least-privilege two-job split):** the `release` job
  (`contents: write` only) annotates + pushes the tag → spec tag (if any) →
  `POST /repos/{repo}/releases` with the generated changelog → packages the
  `git archive` tarball as a run artifact. The `provenance` job
  (`id-token: write` + `attestations: write` + `contents: write`) runs only
  on `released`, reads the `release.*` contract block, and then:
  `cosign sign-blob` keyless-signs the tarball under the runner's GitHub
  OIDC identity (Fulcio certificate + Rekor transparency log, issue #103) →
  `actions/attest-build-provenance` (SHA-pinned) issues the SLSA L3-class
  attestation (issue #100) → the release carries
  `ai-sdlc-<tag>.tar.gz` + `.sig` + `.crt` + `.sigstore.json` +
  `.attestation.jsonl` (exported bundle for offline verification). Publish
  uses `AI_SDLC_TOKEN` when present because a release created by
  `GITHUB_TOKEN` does not fire downstream workflows. A signing or
  attestation failure fails the job — the release never ships silently
  unsigned assets.
- **Contract gates:** `release.provenance: attest|none` and
  `release.signing: cosign-keyless|none` (schema defaults are the secure
  posture). `none` skips the corresponding step; the declared value is the
  audit record.
- **Verify an artifact:** `tools/verify-release.sh <tag>` downloads the
  asset set and runs every check below; or manually —
  ```bash
  # cosign signature + Fulcio cert bound to the release workflow's OIDC identity
  cosign verify-blob --signature ai-sdlc-<tag>.tar.gz.sig \
    --certificate ai-sdlc-<tag>.tar.gz.crt \
    --certificate-identity-regexp \
      '^https://github\.com/os-santiago/ai-sdlc/\.github/workflows/ai-sdlc-release\.yml@refs/(heads/main|tags/v.+)$' \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    ai-sdlc-<tag>.tar.gz

  # or the Sigstore bundle (carries the Rekor inclusion proof):
  cosign verify-blob --bundle ai-sdlc-<tag>.tar.gz.sigstore.json \
    --certificate-identity-regexp '…same…' \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    ai-sdlc-<tag>.tar.gz

  # SLSA build provenance via GitHub Artifact Attestations:
  gh attestation verify ai-sdlc-<tag>.tar.gz -R os-santiago/ai-sdlc

  # offline: the release ships the exported bundle + you supply the trusted root
  gh attestation verify ai-sdlc-<tag>.tar.gz -R os-santiago/ai-sdlc \
    --bundle ai-sdlc-<tag>.tar.gz.attestation.jsonl \
    --custom-trusted-root <(gh attestation trusted-root)
  ```
- **CLI parity:** `node scripts/release-manager.js --repo OWNER/NAME
  [--bump B] [--spec-bump B|none] [--ref REF] [--publish] [--json]` —
  default is always a dry run; nothing mutates without `--publish`.

## Spec tagger (`ai-sdlc-spec-tag.yml`)

Diff-aware `spec/v*.*.*` machinery backed by `scripts/spec-version.js`
(issue #134) — the code side of the semver contract spec/README.md
documents. Where the release manager derives the spec bump from
conventional commits alone, this workflow classifies the `spec/**` tree
diff itself and gates it against the declared version.

- **Triggers:** `pull_request` on `spec/**` (plan-only gate) · `push` to
  `main` on `spec/**` (auto-tag on merge) · `workflow_dispatch`
  (`bump` override, `dry_run` default true).
- **Classification** — the highest severity across the diff wins:
  - **MAJOR** — deleted/renamed spec file; removed or renamed schema
    property; newly `required` field; narrowed `enum`/`type`; added or
    tightened constraint (`minimum`↑/`maximum`↓, `pattern`, `format`,
    `additionalProperties:false`, `uniqueItems:true`, dropped
    `anyOf`/`oneOf` branch, added `allOf`/`not`, removed keyword).
  - **MINOR** — new spec file; new optional property; widened
    `enum`/`type`; relaxed bound; removed constraint.
  - **PATCH** — `.md` edits and annotation-only keyword changes
    (`description`, `title`, `examples`, `default`, `$comment`, …).
  - Prose is editorial to a structural classifier: a `type!:` /
    `BREAKING CHANGE` commit touching `spec/` still forces MAJOR, and
    `--bump`/the dispatch input overrides either way.
- **Declared-version gate:** `spec/README.md` carries
  `**Declared version:** spec/vX.Y.Z`. The plan fails closed when the
  declared version ≠ the computed next tag — so a spec-changing PR must
  carry its version bump in the same diff, and a merge can never tag a
  version the docs don't declare.
- **Publish:** `contents:write`-only job — annotated tag → push →
  `POST /repos/{repo}/releases` with the per-file classification as notes;
  MAJOR releases embed the migration-note template AGENTS.md requires.
  Idempotent — an existing tag reports `exists` and exits green.
- **Bootstrap:** with no `spec/v*` tag at all there is nothing to diff —
  the declared version IS the first tag (the inaugural spec/v1.0.0 is
  cut by the merge that lands this machinery).
- **CLI parity:** `node scripts/spec-version.js [--base TAG] [--bump B]
  [--ref REF] [--readme FILE] [--no-verify-declared] [--publish] [--json]`
  — default is a dry run; nothing mutates without `--publish`.

## Alert routing (failure-class → maintainer)

Escalations no longer land as a generic `needs-human` label: every failure
surface classifies its reason into a canonical **failure class**
(`spec/risk-taxonomy.md` — "Failure classes and alert routing") and the
contract's `escalation.routes` map decides WHO is notified (issue #175;
port of Hermes `scripts/alert_router.js` minus the Discord transport).

```yaml
# .ai-sdlc.yaml
escalation:
  label: needs-human
  contacts: ["@oncall"]
  routes:
    contract-error: {mentions: ["@spec-owner"], labels: ["priority:P0"]}
    supply-chain:   {mentions: ["org/security-team"], labels: ["priority:P1"]}
    default:        {mentions: ["@oncall"]}            # reserved fallback
```

- **Resolution** (`scripts/alert-router.js`): `routes.<class>` →
  `routes.default` → synthesized from `escalation.contacts` /
  `notify_channels`. An unmatched or undeclared class always lands on the
  default route — alerting is never silent.
- **Delivery**: the escalation comment gains a `cc @handles` line and the
  route's `labels` are applied next to `needs-human`; `channel` names the
  deployment-specific target (`issue-comment` is the in-band floor).
- **Audit + dedup**: every routed notification carries
  `<!-- ai-sdlc:alert-route class=… key=… -->` (key = repo|target|class|
  normalized-signature hash). A repeat of the same class+signature on the
  same thread is deduplicated on that marker — mentions suppressed, labels
  still applied. Dedup reads **trusted-author comments only**
  (bots/collaborators): a forged marker in a third-party comment can never
  suppress a real alert.
- **Wired where failures surface**: `ai-sdlc-automerge` (all ESCALATE
  reasons), `ai-sdlc-ci-repair` (budget exhausted / base-contract-error /
  no pushable fix), `ai-sdlc-verify` (`verify-failure` on the filed issue).
  The router script is fetched from the `runtime_ref` pin — never from the
  PR head — and its unavailability degrades to the unrouted escalation,
  never to a dropped one.

## Dependency-update stage (`ai-sdlc-dep-update.yml`)

The GHA port of the Hermes dependency worker (issue #177, epic #150) —
reasoned, vetted dep updates instead of raw Dependabot PRs. Opt-in via the
contract (`dependencies.enabled`, default off). One invocation = one bounded
scan:

1. **Config** — `load-config` resolves `dependencies.*` from the base ref:
   `ecosystems` (v1: npm), `cadence` (audit record; the caller's cron owns
   the real schedule), `min_release_age_days`, `target` (`wanted` stays
   inside declared ranges / `latest` crosses majors), `include_dev`,
   `auto_kinds` (risk threshold — default `patch,minor`), `max_updates`,
   `branch`, `labels`, `egress_profile`. Disabled → `skipped`.
2. **Scan** — checkout `repo@base`, `npm ci --ignore-scripts` (no lifecycle
   script executes), `npm outdated --json` → candidates, prod-first sorted.
3. **Vet** — `scripts/dep-update.js` (fetched from the trusted runtime pin,
   never the scanned repo) checks each candidate's *target* release:
   release age ≥ `min_release_age_days` (fresh releases are
   disproportionately malicious), registry existence, license allowlist,
   typosquat/edit-distance vs popular names, OSV vulnerabilities. Every
   fetch is bounded by the named egress profile (`npm-vet` =
   registry.npmjs.org + api.osv.dev; `off` = no network → everything flags
   fail-closed). Registry metadata is untrusted T3 input
   (`spec/injection-defense.md`) — markdown-escaped into the PR body, never
   instructions.
4. **Plan** — candidates in `auto_kinds` with zero findings → the batch
   (bounded by `max_updates`, remainder deferred to a later scan); majors,
   `unknown` specs, and any finding → flagged for human review.
5. **PR** — the batch applies via `npm install <name>@<target>
   --package-lock-only --ignore-scripts` on `dependencies.branch`, the
   diff is re-classified (manifest/lockfile-only, patch|minor — fail-closed
   abort otherwise) and pushed non-force under the App installation token.
   At most one open batch PR per repo; a human-closed batch signature is
   not retried until versions drift. Labels: `dependencies`, `ai-sdlc`
   (pipeline-managed → normal review/automerge path) and the vetting
   verdict's `pr:risk-*` — the canonical classifier's dep fast-path maps
   clean manifest/lockfile patch|minor diffs to `pr:risk-low`; majors, new
   direct deps and non-dep files never take it.
6. **Flag for human** — flagged candidates are recorded in the step
   summary + run ledger and collected into ONE deduped open issue
   (`<!-- ai-sdlc:dep-flagged -->` marker; `dependencies` +
   `escalation.label`), updated on subsequent runs instead of spammed.
   Flagged updates are never auto-landed.

Caller wiring (consumers pick their own cadence):

```yaml
name: ai-sdlc-dep-update
on:
  schedule: [{ cron: '41 5 * * *' }]
  workflow_dispatch:

jobs:
  dep-update:
    uses: os-santiago/ai-sdlc/.github/workflows/ai-sdlc-dep-update.yml@main
    with:
      repo: ${{ github.repository }}
    secrets:
      AI_SDLC_APP_ID: ${{ secrets.AI_SDLC_APP_ID }}                  # required
      AI_SDLC_APP_PRIVATE_KEY: ${{ secrets.AI_SDLC_APP_PRIVATE_KEY }} # required
      AI_SDLC_LEDGER_KEY: ${{ secrets.AI_SDLC_LEDGER_KEY }}          # optional
```

Artifacts: `ai-sdlc-dep-update-<attempt>` (plan.json, outdated.json, PR
body, batch diff verdict, flagged body) + the sealed run-ledger entry
(`stage: dep-update`).



## Model fallback

A single dead/quota-exhausted provider must not stall the pipeline. The
engine step runs `model` first; on a provider-class failure it gets
**exactly one** fresh retry with `fallback_model`:

| Primary exit | Action |
|---|---|
| 20 (provider) / 21 (auth) — or a `transport` stall verdict | one fallback run — unless `fallback_model` is empty or equals `model` |
| 0 / 10 / 11 / 22 (budget) / 23 (livelock) / 24 (chain exhausted) / 1 | final — never retried (not provider problems) |

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
--audit-log --summary-file --max-seconds --max-steps`. The raw exit-code
taxonomy is a cross-repo contract: emission semantics are normative in
[`sc-agent-cli` `docs/exit-codes.md`](https://github.com/os-santiago/sc-agent-cli/blob/main/docs/exit-codes.md)
and the runtime's consumption of them in
[spec/engine-exit-codes.md](../spec/engine-exit-codes.md) (drift-checked by
`tests/exit_code_contract.test.js`). Codes drive issue labels:

| Code | Meaning | Label |
|---|---|---|
| 0 | implemented + PR opened | `scc:pr-opened` |
| 10 | no workspace mutations (clean no-op) | `scc:no-changes` + `scc:not-ready` |
| 11 | not actionable / blocked | `scc:failed` + `scc:not-ready` (spec route) |
| 20/21/22/23/24/1 | provider/auth/budget/livelock/exhausted/fatal | `scc:failed` (stall-class routing) |
| 20/21 after fallback | provider/auth on both models | `scc:failed` + `needs-human` |

`exit_code` is the final run's code (see [Model fallback](#model-fallback)).

## Injection defense (`scripts/injection_defense.js`)

The sanitization stage of spec/injection-defense.md (issue #181, epic
#151). Every untrusted field — issue title/body (implement), PR title,
diff stat and patch (review), review findings (review-fix), CI log tails
and check names (ci-repair) — passes through the canonical sanitizer
**before** the prompt file is assembled. The script is fetched from the
`os-santiago/ai-sdlc@main` pin in each prompt-build step (same trust
posture as `scripts/risk-classifier.js` — never from the PR head or the
consumer checkout); when the pin predates it the stage falls back to the
previous inline `</untrusted` escaping.

Per field the stage applies: size cap → non-rendering Unicode strip (Tag
chars, zero-width, bidi overrides, interlinear anchors — steganographic
injection; variation selectors are flagged, not stripped) →
imperative-override scrub (`ignore previous instructions`, `system:`,
`you are now…`, chat-template tags, fake tool results, fake authority —
neutralized in place) → forbidden-directive detection (force-push,
pipe-to-shell, credential egress, policy self-modify, permission/token
escalation, fence escapes) → secret-shape redaction (PEM blocks, token
shapes, dotenv assignments, URL credentials, live env literals) →
fence-escape neutralization.

Verdict classes, auditable and never silent:

- `clean` — no findings; fenced as-is.
- `scrubbed` — findings neutralized/redacted; fenced.
- `blocked` — high-confidence hostile (fence escape + hostile signal,
  credential egress to an endpoint, embedded private-key material, or a
  weighted hostile score ≥ 6). The content never reaches the engine: the
  run is gated off and the source issue/PR is quarantined to
  `needs-human` with a comment naming the block reasons.

Detection telemetry appends count-only JSONL records
(`prompt_injection_scrubbed` / the spec's refusal class
`prompt_injection_blocked` — pattern names + counts, never the matched
text) and ships as `injection-audit.jsonl` in the run artifacts.

Output-side redaction (`redactOutput`, the spec's "redaction applies to
ALL agent output"): a `Redact run artifacts` step in implement, review,
review-fix and ci-repair passes the secret+PII pass over every
engine-emitted artifact (scc audit log, manifest, verdict, prompt files)
before the ledger append + upload — hashed evidence matches the uploaded
artifacts. Output magnitude (`checkProportion`): implement additionally
gates the push on a disproportion check — a staged diff over 100 files /
5000 lines, or >1000 lines against a ≤200-char trigger, escalates to
`needs-human` uncommitted instead of pushing.

## Run ledger

Tamper-evident per-stage run evidence (issue #196, epic #148). Every stage
appends a hash-chained record and seals the file at run completion:

- **Entry** (`append`): `{v, seq, stage, ts, repo, issue, pr, engine, run,
  data, data_sha256, prev, hash}` — `engine` is the engine adapter id
  (`engine.name` resolved from the contract, validated against
  `config/engine_adapters.json` — issue #230; `null` when the stage ran no
  resolvable engine); `data_sha256` is the SHA-256 of the
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

## Run-evidence layout

The canonical artifact layout is normative in
[spec/run-evidence.md](../spec/run-evidence.md) (issue #141) and enforced
by `tools/check-run-evidence.py` in `tools/validate-repo.sh`:

- Working evidence lives under `${{ runner.temp }}/ai-sdlc-<stage>/` —
  one directory per stage — and uploads as
  `ai-sdlc-<stage>-<subject>[-attempt-<n>]`. Sealed ledger segments live
  under `${{ runner.temp }}/ai-sdlc-ledger/ledger-<stage>.jsonl`, contract
  resolution under `${{ runner.temp }}/ai-sdlc-contract/`.
- Workspace-root uploads are a closed, enumerated class: the implement
  stage's engine session files (the engine CLI and `git` operate on the
  checkout — the set is `git add`-excluded and root-`.gitignore`d; its
  migration to `${{ runner.temp }}` is the tracked remainder of #141),
  the sweep's `ready-queue.*`, metrics `slo-*`, drift `drift-*`, and the
  release `dist/` bundle. New workspace writes outside the declared set
  fail the repo check.

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

### Audit query — "why did the agent do X"

`scripts/audit-query.js` (issue #174) turns a downloaded ledger bundle into
answers: it stitches the per-stage files into the causal chain and extracts
the decision each stage recorded (intake DoR verdict, engine model/exit,
review verdict, automerge action, escalation) — citing the entry hash so
every claim is verifiable back to the sealed evidence.

```bash
gh run download <RUN_ID> -D artifacts/ -p 'ai-sdlc-ledger-*'   # any bundle dir works

# full stage-by-stage trail for an issue (issue↔pr linkage pulls in the
# PR-scoped stages the issue dispatched) — or --pr N / --run <id>
node scripts/audit-query.js trace --dir artifacts/ --issue 174

# structured answers: escalated|skipped|labeled|merged|held|failed|dispatched
node scripts/audit-query.js why escalated --dir artifacts/ --pr 42
node scripts/audit-query.js labels --dir artifacts/ --issue 174
node scripts/audit-query.js trace --dir artifacts/ --issue 174 --json \
  --pubkey-file ai-sdlc-ledger.pub.pem --strict
```

Entries that fail the hash/signature recompute stay in the trail marked
`TAMPER` with the exact problem — a miss ("no ledger entries explain X")
and malformed/partial ledgers are reported, never fatal (`--strict` exits 1
on any integrity problem). Label names in `labels` output are the canonical
defaults — the ledger records the decision; the resolved `labels.*` /
`escalation.label` names live in the `contract.json` artifact.

### Signing key

```bash
node .github/actions/run-ledger/run-ledger.js keygen --out-dir /tmp/ledger-key
# private PEM → GitHub secret AI_SDLC_LEDGER_KEY (org secret scoped to
#   consumer repos — never committed)
# public  PEM → commit/publish; auditors pass it to verify --pubkey-file
```

The key is a dedicated secret (key separation from the App's
`AI_SDLC_APP_PRIVATE_KEY`); callers map it explicitly as
`AI_SDLC_LEDGER_KEY: ${{ secrets.AI_SDLC_LEDGER_KEY }}` on the stages that
declare it.

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
session files live in the workspace root during a run — the declared
workspace-session class of the run-evidence layout
([spec/run-evidence.md](../spec/run-evidence.md)) — but are excluded
from the implement stage's `git add` (`:!scc-*.json*`, `:!ai-sdlc-run.json`,
`:!issue.json`, `:!task-prompt.md`) — and root-ignored in this repo's
`.gitignore` — so a run's commit only ever carries real source changes
(issue #68).

## Cost accounting (issue→merge)

`scripts/cost-accounting.js` records prompt/completion token usage for every
LLM-bearing stage into a JSONL usage ledger (`ai-sdlc-usage.jsonl`) and
prices each record against the declared table in `config/cost_models.json`
(issue #195; epic #148). Cost is always computed from the table — the
engine's own `estimated_cost_usd` is preserved on the record as
`engine_cost_usd` for auditing, never trusted for rollup math. Everything
is bounded (`AI_SDLC_COST_MAX_LEDGER_BYTES`, `AI_SDLC_COST_MAX_ENTRIES`)
and malformed ledger lines are skipped — accounting must never break the
pipeline.

```bash
# implement stage — after the engine run, one record per manifest
node scripts/cost-accounting.js record --ledger ai-sdlc-usage.jsonl \
  --stage implement --issue "$ISSUE" --manifest scc-manifest.json \
  --outcome "$(jq -r '.exit_reason // .resolution // empty' scc-manifest.json)" \
  --github-output
# …and again with scc-manifest.primary.json when the fallback fired
# (stage stays `implement`; the manifest carries its own model).

# review / ci-repair stages — same op, their own stage + manifest
node scripts/cost-accounting.js record --ledger ai-sdlc-usage.jsonl \
  --stage review --issue "$ISSUE" --pr "$PR" --manifest scc-manifest.json

# issue→merge rollup, queryable at any point in the lifecycle
node scripts/cost-accounting.js rollup --ledger ai-sdlc-usage.jsonl --issue "$ISSUE"
node scripts/cost-accounting.js rollup --ledger ai-sdlc-usage.jsonl --markdown

# budget thresholds — advisory by default; --strict exits 1 on `exceeded`
node scripts/cost-accounting.js check --ledger ai-sdlc-usage.jsonl
node scripts/cost-accounting.js check --ledger ai-sdlc-usage.jsonl --strict
```

**Wiring points** (record ops run under `if: always()` +
`continue-on-error: true`, same posture as other evidence steps):

- `ai-sdlc-implement.yml` — after the *Run scc headless* step: `record`
  with `--manifest scc-manifest.json` (and `scc-manifest.primary.json`
  when `fallback_used=true`), `--stage implement`, `--issue`, and
  `--outcome` from the manifest; add `ai-sdlc-usage.jsonl` to the
  `ai-sdlc-run-<issue>` artifact paths.
- `ai-sdlc-review.yml` — `record --stage review --pr` after each reviewer
  engine invocation; include the ledger in the review artifact upload.
- `ai-sdlc-ci-repair.yml` — `record --stage ci-repair --pr` per repair
  attempt, so repair cost accrues to the issue.
- `ai-sdlc-intake.yml` / `ai-sdlc-automerge.yml` / `ai-sdlc-verify.yml` —
  `record` with the matching stage when the stage makes LLM calls;
  `automerge` records mark the rollup lifecycle `merged`.
- Post-merge economics — a workflow (or the sweep) downloads the stage
  artifacts and runs `rollup --issue` / `check`; the per-issue total is
  the acceptance artifact for "cost per issue→merge run".

**Run-ledger forward-compat** (issue #196): each record is a JSON object
with the same envelope fields the signed run ledger digests
(`v`/`stage`/`ts`/`repo`/`issue`/`pr`/`run`), so `record` output can be
passed verbatim as the `--data` payload of a ledger `append` — and
`normalizeRecord` unwraps usage records embedded in `data` on the way
back, so rollups keep working once stage ledgers become the transport.

**Pricing**: `config/cost_models.json` keys are provider-qualified model
ids (`openai/gpt-4o`, `nvidia/nemotron-3-super-120b-a12b`); lookup is
exact → case-insensitive → provider-suffix → `default` (never silent-zero
for unknowns). Update prices there when the provider or tier changes;
NIM free-tier endpoints are declared at `0.0` explicitly. Budget caps
(`per_issue_usd`, `per_issue_tokens`, `daily/weekly/monthly_usd`) and
`alert_thresholds` live in the same file; `AI_SDLC_COST_CONFIG` points at
an override.

## Umbrella coordination (`scripts/umbrella-coordinator.js`)

Contract-spanning work (an API bump, a shared protocol field, a dependency
couple) is coordinated from ONE `umbrella`-labeled hub issue: the
coordinator files a scoped child issue in each participating repo and keeps
a single upserted status comment on the umbrella as the live hub checklist
(issue #199, epic #147; ported from the Hermes control plane).

- **Declaration.** The umbrella body declares participating repos in a
  fenced ```` ```ai-sdlc:umbrella ```` block (JSON `items`/`repos`, or
  `o/r — title` lines; the legacy `hermes:umbrella` fence is accepted) or
  as plain bullets: `- o/r — feat(api): …`, `- o/r` (inherits the umbrella
  title), `- o/r#N` (adopt an existing issue). `--repos o/a,o/b` or
  `--spec spec.json` override body declarations outright.
- **Children.** One child per repo, filed as `scc:child` with a DoR-shaped
  body, a link back to the umbrella, and an
  `<!-- ai-sdlc-umbrella-child key:"…" umbrella:"o/r#N" -->` marker. No
  dispatch trigger is applied — a human arms `ready-to-implement` per repo,
  same contract as epic-decomposer children.
- **Hub state.** A single `<!-- ai-sdlc-umbrella-status -->` comment on the
  umbrella renders every repo row — pending → open → closed (closed
  children auto-check). Re-runs update that comment in place; it never
  stacks.
- **Idempotent.** Every write is keyed on a marker the next pass reads
  back: a marker-carrying child is adopted (never re-filed) and the status
  comment is upserted, so a repeated run converges to zero mutations.
- **Bounded.** `--max-repos` (default `UMBRELLA_MAX_REPOS=8`) caps
  participating repos; child scans are ≤100 issues per state per repo and
  status-comment lookup ≤300 comments.

```bash
node scripts/umbrella-coordinator.js --repo OWNER/NAME --issue N \
  [--repos o/a,o/b | --spec spec.json] [--dry-run] [--json] [--github-output]
```

## A/B experiment promotion (eval evidence)

`scripts/ab-experiment.js` is the decision layer for experiment lifecycle
(issue #192; epic #148). Experiments are declared in
`config/experiments.json` — variants (with `weight`-based traffic split and
the subject under test: `model`, `promptRef`, …), the control arm, the eval
`metric`, `alpha` (default 0.05), `minSamplesPerArm`, and the bounded
evidence `window` / `windowDays`. The service reads run/eval result
artifacts (JSON array or JSONL records tagged `experiment` + `variant`),
compares every challenger arm against control — two-proportion z-test for
rate metrics (`pass_rate`/`success_rate`/`win_rate`), Welch's t-test for
continuous metrics (`score`, `latency_ms`, `cost_usd`, …) — and emits a
deterministic decision: `promote` (a challenger beats control at
p <= alpha over a powered window), `rollback` (the promoted winner now
significantly loses to control), `continue`, or `insufficient_data`.

The decision is a pure function of config + evidence + state, so replays
with `--now` are byte-identical. Applied transitions persist in a state
file (`--state` / `AI_SDLC_AB_STATE`, never the committed config) and every
decision lands in a sibling `*.audit.jsonl` trail with the reasons and the
comparisons that produced it.

```bash
# evaluate the latest eval results for an experiment
node scripts/ab-experiment.js decide --experiment primary-model-quality \
  --evidence eval-results.jsonl --state ab-state.json

# persist the decision (promotion/rollback is marked in the state file)
node scripts/ab-experiment.js decide --experiment primary-model-quality \
  --evidence eval-results.jsonl --state ab-state.json --apply

# the variant subsequent runs should use — promoted winner, unless overridden
node scripts/ab-experiment.js serving --experiment primary-model-quality --state ab-state.json
# → candidate   (or 'control' while nothing is promoted)

# operator override: AI_SDLC_AB_OVERRIDE=<variant> or --override-variant;
# AI_SDLC_AB_DISABLE=1 is the kill switch — serving always resolves control
```

Manual `promote`/`rollback` ops exist for the human override path and are
recorded as `decidedBy: "manual"`. Undated evidence is excluded when
`windowDays` bounds the window (recency is unverifiable — fail-closed), and
records without a reading for the experiment's metric never count toward an
arm's sample size.

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
## Kill-switch (org autonomy revocation — issue #172)

Normative contract: **spec/kill-switch.md**. Implementation:
`scripts/kill-switch.js` + the "Autonomy gate — org kill-switch" step in
every stage workflow + `ai-sdlc-kill.yml` (engage/release dispatch).

`AI_SDLC_OFF` (and `AI_SDLC_KILL`) is the emergency stop, carried as a
**repository variable** (Settings → Secrets and variables → Actions →
*Variables* tab — not a secret); an **org-level variable** visible to
enrolled repos engages the switch fleet-wide with zero per-repo config.
A second channel is the `ai-sdlc:off` control issue: open it in the
control repo (`vars.AI_SDLC_CONTROL_REPO`, default the repo itself) and
every enrolled stage's gate denies on its next check — one open issue
halts the fleet.

Every stage checks at the top of the run and exits before any mutation
(review soft-skips: it also runs on human PRs, so it must not red-mark
them). The check is **per run** — no runner teardown, no workflow
disable; in-flight engine runs halt at the next gate. The verdict is
**fail-closed**: a channel that cannot be read denies (`unreadable`) —
autonomy defaults OFF. Unset the signal and the next event resumes the
loop. Prefer it over disabling workflows during an incident — disabling
also silences the sweep and the merge gate, leaving pipeline PRs
unresolved.

Canonical audited operation (control repo):

```bash
gh workflow run ai-sdlc-kill.yml -f action=engage  -f reason="incident-42"
gh workflow run ai-sdlc-kill.yml -f action=release -f reason="resolved"
```

Engagement/release is recorded with actor + ISO-8601 timestamp on the
control issue and in a `stage: kill-switch` run-ledger entry.

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
