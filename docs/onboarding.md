# Onboarding — one-command install and the shadow-start trust ramp

One command — `gh ai-sdlc init` — opens a single bootstrap PR on the
target repo. Merging it wires the contract, the caller workflow, and the
label set; the repo enters the trust ladder at `shadow` and earns every
level above it on run evidence, not promises.

Tracking: [#207](https://github.com/os-santiago/ai-sdlc/issues/207) ·
epic [#153](https://github.com/os-santiago/ai-sdlc/issues/153) ·
installer [#185](https://github.com/os-santiago/ai-sdlc/issues/185) ·
self-accrediting ramp [#186](https://github.com/os-santiago/ai-sdlc/issues/186).

## `gh ai-sdlc init`, step by step

Three entrypoints produce the same bootstrap PR — all over the shared
installer engine `tools/ai-sdlc-init.js` shipped by this repo
(issue #185):

- **the installer script** — from an ai-sdlc checkout, point it at the
  target (or run it inside the target's checkout by path):

  ```bash
  node tools/ai-sdlc-init.js /path/to/target        # writes .ai-sdlc.yaml +
                                                    # caller + sweep
  node tools/ai-sdlc-init.js /path/to/target --pr   # also branches, commits,
                                                    # pushes, opens the
                                                    # bootstrap PR (needs gh)
  node tools/ai-sdlc-init.js . --dry-run            # plan + schema
                                                    # validation, no writes
  ```

  Idempotent: identical files report `unchanged`, divergent files are kept
  unless `--force`. The generated contract is validated against
  `spec/ai-sdlc.schema.json` (tools/validate-ai-sdlc-config.sh) before
  anything is written.
- **the `gh` extension** —
  [`os-santiago/gh-ai-sdlc`](https://github.com/os-santiago/gh-ai-sdlc);
  install once, run in any repo.
- **the bootstrap action** —
  [`os-santiago/ai-sdlc`](https://github.com/os-santiago/ai-sdlc) ships a
  dispatchable installer workflow that opens the identical PR — the
  entrypoint for orgs onboarding a fleet without a local `gh`.

> The [manual path](#manual-onboarding) produces the identical result —
> the bootstrap PR is automation over that path, not a separate
> mechanism.

1. **Prerequisites** — the [`gh` CLI](https://cli.github.com),
   authenticated (`gh auth login`), with admin rights on the target
   repo.
2. **Install the extension:**

   ```bash
   gh extension install os-santiago/gh-ai-sdlc
   ```

3. **Run init** inside the repo checkout — or name the target
   explicitly:

   ```bash
   gh ai-sdlc init                        # current repo
   gh ai-sdlc init --repo owner/name      # explicit target
   ```

   The command opens **one** bootstrap PR on the target
   (`chore(ai-sdlc): onboard owner/repo`) and prints its URL. It never
   writes to the repo directly — every change lands through the PR diff,
   so onboarding is reviewable and revertible like any other change.
4. **Review the PR** — the diff carries the starter contract, the caller
   workflow, and the label bootstrap; the body carries the secrets
   checklist below.
5. **Install the `ai-sdlc` GitHub App** on the repo (or org) and set the
   secrets/variables the checklist enumerates.
6. **Merge the PR** — merging is the onboarding act. Then label a
   well-scoped issue `ready-to-implement`: at `shadow` the pipeline
   reports what it *would* do — intake, plan, dry-run — and mutates
   nothing.

## What the bootstrap PR carries

| Artifact | Purpose |
|---|---|
| `.ai-sdlc.yaml` | Starter contract pinned to `autonomy.level: shadow` with the `promotion:`/`demotion:` evidence gates pre-declared — see [the trust ramp](#the-trust-ramp--start-at-shadow-climb-on-evidence) |
| `.github/workflows/ai-sdlc.yml` | Thin caller dispatching the reusable stages (`intake` on `issues.labeled`, `implement` on dispatch; `review` / `automerge` / `ci-repair` / `sweep` / `verify` wire the same way — [runtime.md](runtime.md)) |
| Label bootstrap | The canonical set — `ready-to-implement`, `scc:*` run states, `pr:risk-*` tiers, `needs-human`, skip labels ([spec/labels.md](../spec/labels.md)) — created by the PR itself; no direct API label writes before merge |
| PR body | Secrets checklist + the post-merge dispatch instructions above |

## Secrets the PR body walks through

Nothing secret ever lands in the repo — contract validation rejects
credential-shaped values ([runtime.md](runtime.md)). The checklist is:

| Secret / variable | Required | Purpose |
|---|---|---|
| `AI_SDLC_APP_ID` + `AI_SDLC_APP_PRIVATE_KEY` | yes | every mutation stage mints a per-run App installation token — fail-closed, no PAT/`GITHUB_TOKEN` fallback |
| `MODEL_API_KEY` | yes, for inference | API key for the resolved provider endpoint (GitHub Models retired 2026-07-30) |
| `AI_SDLC_PROVIDER_URL` (repo **variable**) | yes | OpenAI-compatible inference endpoint the engine resolves |
| `AI_SDLC_OFF` / `AI_SDLC_KILL` (repo or org variable) | optional | truthy halts dispatch per run — the emergency stop (org-scoped = fleet-wide; spec/kill-switch.md) |
| `AI_SDLC_CONTROL_REPO` (repo variable) | optional | control repo holding the `ai-sdlc:off` kill issue — set on enrolled repos for the fleet channel (default: the repo itself) |
| `AI_SDLC_FREEZE` | optional | contract-named freeze env; pins the effective level to `shadow` while set |

The end-state the installer targets (#185): a shared public App whose
tokens are OIDC-minted per run — the org installs the App and manages
**zero** secrets; self-managed per-org secrets remain the path for
regulated or air-gapped environments.

## The trust ramp — start at `shadow`, climb on evidence

The starter contract pins the lowest rung of the
[trust ladder](../spec/trust-ladder.md) **and pre-declares the evidence
thresholds it climbs against** — onboarding wires a level *and* the
gates, not just a level:

```yaml
# .ai-sdlc.yaml — as written by gh ai-sdlc init
spec: "1.0.0"
autonomy:
  level: shadow   # observe-only: reports, never mutates (also the
                  # resolved default when autonomy.level is omitted)
promotion:
  clean_merges_threshold: 3   # clean merges in the window → promotion PR
  window: 30                  # rolling evidence window, completed runs
demotion:
  escalation_threshold: 1     # escalations in the window → demotion PR
  window: 30
```

`shadow` is a ceiling, not a dead end — and not just a convention:
`shadow` is the schema's resolved default, so a contract that omits
`autonomy.level` still starts observe-only (silence never grants
privilege). From the first labeled issue the
repo accumulates the exact evidence the ladder measures — merge success
rate, revert rate, escalation rate, repair rounds per PR — over a
rolling window of completed runs (default N = 20; at least 10 before any
promotion is considered). Two properties make the ramp
*self-accrediting* (issue #186):

- **Evidence accrues automatically.** Clean pipeline runs count toward
  the promotion gate from day one — no separate audit, no manual score
  keeping. The bootstrap contract pre-declares the thresholds it climbs
  against.
- **Promotion arrives as a PR.** When the repo's evidence passes the
  gate — merge success ≥ 90%, revert rate ≤ 5%, escalation rate ≤ 10%,
  mean repair rounds ≤ 1.5 — the promotion proposal lands as a normal,
  reviewed contract PR raising `autonomy.level`. The pipeline accredits
  itself with data; a maintainer still merges the promotion.

The rest of the ladder applies unchanged: the configured level is a
ceiling (the pipeline may run below it — freezes, incidents — never
above), demotion triggers drop one level automatically,
`AI_SDLC_FREEZE` pins the effective level to `shadow`, and an org
baseline can clamp or pin the ceiling from above
([spec/org-policy.md](../spec/org-policy.md)). Maintainers may always
raise the level by contract PR — beyond measured evidence the promotion
gate must pass first.

| Level | What it earns |
|---|---|
| `shadow` (start) | Reports what it *would* do; zero mutations |
| `suggest` | Proposes diffs/plans as issue comments; humans implement |
| `auto-pr` | Opens real PRs; humans merge |
| `auto-merge-low` → `auto-deploy` | Progressive merge/deploy autonomy per [spec/trust-ladder.md](../spec/trust-ladder.md) |

## Manual onboarding

No extension required — the bootstrap PR only automates what the
[README](../README.md#adopt-this-runtime) lists by hand: a root
`.ai-sdlc.yaml`, a caller workflow under `.github/workflows/`, and the
App + secrets setup. Validate the contract against
`spec/ai-sdlc.schema.json` with
[tools/validate-ai-sdlc-config.sh](../tools/validate-ai-sdlc-config.sh);
the archetypes under [examples/](../examples/) are the reference
starting points — [onboarding.ai-sdlc.yaml](../examples/onboarding.ai-sdlc.yaml)
mirrors the bootstrap starter above. To mirror the guided path,
set `autonomy.level: shadow` (or omit `autonomy` — `shadow` is the
resolved default), pre-declare the `promotion:`/`demotion:` thresholds,
and let evidence drive promotion.

## See also

- [runtime.md](runtime.md) — per-stage `workflow_call` reference
- [../spec/trust-ladder.md](../spec/trust-ladder.md) — levels, promotion
  gates, demotion triggers, freeze
- [../spec/labels.md](../spec/labels.md) — the label set the PR
  bootstraps
- [positioning.md](positioning.md) — why the contract, not the engine,
  is the product surface
