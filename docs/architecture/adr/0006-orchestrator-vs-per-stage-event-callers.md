# ADR-0006: Orchestrator vs per-stage event callers — runtime caller topology

- **Status**: Accepted
- **Date**: 2026-10-19
- **Deciders**: @Axel-DaMage (sole active maintainer; delegated authority)
- **Tracking**: [#110](https://github.com/os-santiago/ai-sdlc/issues/110)

## Context

The runtime ships ~20 workflows under `.github/workflows/`: nine
`workflow_call`-only stage units (`ai-sdlc-intake`, `-implement`,
`-classify`, `-review`, `-review-run`, `-review-fix`, `-automerge`,
`-ci-repair`, `-verify`), self-scheduled operational stages (`-sweep`,
`-metrics`, `-dora`, `-drift`, `-dep-update`, `-vuln-feed`, `-canary`),
plus the self-dogfooding caller `ai-sdlc.yml`. The maintainer question
(issue #110): should the per-stage workflows collapse into **one
orchestrator workflow that advances step-wise, calling each component**?
Concern raised: many workflow files become a maintenance burden.

The governing constraint is that stages are separated in **time**, not
just sequence. Intake fires on `issues`/`issue_comment` events; implement
runs seconds-to-minutes later; classify and review wait on `pull_request`
events; the merge gate waits on `check_run`/`pull_request` events that can
arrive hours after the PR opens; verify runs post-merge on `push`. A
single in-run orchestrator would either idle-wait through those gaps —
burning runner minutes and colliding with job execution limits — or chain
stages via `workflow_run`/scheduled sweeps, which recreates the event
model inside one file with strictly worse attribution and observability.

Two facts already on the ground:

- **The consolidation the question asks for shipped** with the installer
  (`tools/ai-sdlc-init.js`, issue #185): consumer repos get **two** entry
  workflows — one `ai-sdlc.yml` multiplexing `issues`, `pull_request`,
  `check_run`, `push` and `workflow_dispatch` into per-job `if:` + `uses:`
  dispatches, and one vendored scheduled `ai-sdlc-sweep.yml`. That is
  already under the issue's ≤3-entry-workflows ceiling; this repo's
  self-caller (`ai-sdlc (self)`) dogfoods the same shape.
- **`ai-sdlc-canary.yml`** (issue #132) is the end-to-end smoke the issue
  calls "selftest": it files a synthetic issue, arms it with the real
  trigger label, and observes `scc:queued` → `scc:pr-opened` through the
  real event path — stronger evidence than an in-run `uses:` chain
  because it tests the exact plumbing adopters deploy.

| | In-run orchestrator | Consolidated event callers (ratified) |
|---|---|---|
| Runner minutes | Idle-waits across event gaps (minutes→hours), or needs `workflow_run` chains that reintroduce events anyway | Neutral — each stage runs only on its trigger event |
| Ops surface | One file, one `on:` — but orchestration state machines get re-implemented as YAML glue | Two generated files; event routing stays declarative `on:` + per-job `if:` |
| Retry granularity | A crashed stage forces replay/skip gymnastics inside the orchestrator run | Per-stage isolation: a crashed `implement` never replays `intake`; the merge gate re-fires on the next event |
| Contract clarity | A `uses:` chain proves sequence, not the event model adopters actually run | The `workflow_call` surface **is** the product; consumer callers show exactly what fires when |
| Observability | One run to read, but a stalled stage buries the pipeline inside one long-lived run | One run per stage-event — maps 1:1 onto the `scc:*` label state machine (spec/labels.md, docs/operations.md) |

## Decision

1. **Stage workflows remain `workflow_call` reusable units.** That
   contract is the product surface — each stage is independently
   versioned, pin-able and testable. Stages are never collapsed into a
   single workflow file.
2. **Consumer entry surface: consolidated callers, ≤3 files.** One event
   caller (`.github/workflows/ai-sdlc.yml`) multiplexes every event
   trigger — `issues`, `issue_comment`, `pull_request`, `check_run`,
   `push`, `workflow_dispatch` — into per-job `if:` + `uses:` dispatches;
   one vendored scheduled caller (`ai-sdlc-sweep.yml`) covers the cron
   domain, since a schedule cannot live inside a `workflow_call`-only
   stage. A third entry file is permitted only for a trigger domain that
   genuinely cannot ride the main caller — none exists today. This
   ratifies the topology `tools/ai-sdlc-init.js` already generates.
3. **The single in-run orchestrator is rejected.** It cannot span the
   event gaps without idle-burning minutes or re-deriving the event model
   it was meant to simplify.
4. **`ai-sdlc-canary.yml` is the chain's proof**, not a parallel
   orchestrator-shaped selftest — it drives the loop through real events
   on the real callers, which is the surface adopters actually run.

## Consequences

### For consumer repos

- **Two workflow files to read and pin** (`ai-sdlc.yml` +
  `ai-sdlc-sweep.yml`); consumer-side Dependabot keeps the SHA-pinned
  `uses:` refs fresh — the maintenance-burden concern is answered at the
  caller layer, where consolidation already happened.
- **Per-stage retry semantics preserved**: every stage remains its own
  run — re-run a single failed stage (or let the next event re-fire it)
  without replaying upstream work. The merge gate is additionally
  re-driven by the scheduled sweep, so a missed event self-heals.

### For this repo

- The workflow-file count stays high by design: stage files are versioned
  product surface (like `spec/` documents), not incidental complexity.
  The ops load lives in the two caller files adopters copy.
- The single event caller concentrates `on:` triggers and per-job `if:`
  routing in one file — a deliberate trade: generation (`ai-sdlc-init`)
  owns it, and `tools/check-doc-snippets.py` keeps the documented caller
  blocks honest against the `workflow_call` contracts.
- The doc note for consumers lives in `docs/runtime.md` (§Caller
  topology) — which model applies and why.

### Rejected alternatives

- **Single orchestrator calling stages sequentially via `uses:`** —
  infeasible across event gaps without idle-waiting; `workflow_run`
  chaining re-derives the event model with worse attribution.
- **One caller file per stage** (the shape the `ai-sdlc-repair.yml`
  doc example showed pre-consolidation) — superseded: N entry files
  duplicate the secrets map and permissions union per file.
- **`workflow_run` chaining between stage workflows** — adds a second
  dispatch layer for events the forge already emits; reserved for a
  future need that the direct event model demonstrably cannot serve.

## Rollback

Docs-only decision — it ratifies the topology already shipped by the
installer and dogfooded by `ai-sdlc (self)`. Reverting means editing this
record and the `docs/runtime.md` note; no generated artifact or live
workflow changes.
