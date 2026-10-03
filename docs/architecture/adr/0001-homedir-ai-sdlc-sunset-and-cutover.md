# ADR-0001: homedir-ai-sdlc sunset and canonical-runtime cutover

- **Status**: Accepted (direction); cutover execution is gated on evidence — see gate below
- **Date**: 2026-10-02
- **Deciders**: @Axel-DaMage (sole active maintainer; delegated authority)
- **Tracking**: epic [#30](https://github.com/os-santiago/ai-sdlc/issues/30)

## Context

Three independent implementations of the same loop (issue intake → implement →
review → merge) exist today:

| Implementation | Form | State |
|---|---|---|
| `os-santiago/ai-sdlc` (this repo) | `workflow_call` GHA workflows + normative spec | MVP scaffold; dogfoods its own `.ai-sdlc.yaml` |
| `Axel-DaMage/hermes` | Node.js polling loop inside an always-on container | Production; far ahead in features (review-fix loop, runbook, refiner, umbrella coordination, merge sentinel) |
| `os-santiago/homedir-ai-sdlc` | ~3.2k-line bash worker on a VPS (Podman) + Quarkus dashboard + events service | Operational for `os-santiago/homedir`; heavy maintenance and doc debt |

`homedir-ai-sdlc` exists because the pipeline originally required always-on
infrastructure: a polling worker, persistent state, and a dashboard need a host.
The GHA-native direction (epic [#7](https://github.com/os-santiago/ai-sdlc/issues/7);
Hermes-side vision in `Axel-DaMage/hermes#34`) removes that requirement: compute
becomes ephemeral and event-driven, and for public repos effectively free.

Maintainer bandwidth is effectively a single operator; every concurrent
implementation multiplies filing/label drift and maintenance surface
(precedent: `Axel-DaMage/hermes#381` — duplicated `scc:child` filings across
repos). The end state already declared by both roadmaps: this repo is the
canonical runtime; Hermes transitions from runtime to **control plane**
(Discord UX, OmniRoute routing, memory, cross-repo orchestration);
`homedir-ai-sdlc` is the legacy instance.

## Decision

1. **`os-santiago/homedir` migrates to this runtime.** The consumer repo carries
   only `.ai-sdlc.yaml` + a thin caller workflow. Pilot:
   [os-santiago/homedir#1573](https://github.com/os-santiago/homedir/issues/1573).
2. **`homedir-ai-sdlc` enters feature-freeze now** and is archived (read-only,
   never deleted — history is evidence for the adev practice) after the
   evidence gate passes. Tracking:
   [homedir-ai-sdlc#117](https://github.com/os-santiago/homedir-ai-sdlc/issues/117).
3. **No repository fusion.** The spec/runtime is a versioned product consumed by
   `@ref`; the instance is stateful infrastructure. Merging them violates the
   layering this ADR ratifies.
4. **Identity is a GitHub App** (`ai-sdlc`), not a PAT or bot user — App
   installation tokens fix the `GITHUB_TOKEN` no-downstream-trigger limitation
   with least privilege. [#26](https://github.com/os-santiago/ai-sdlc/issues/26).
5. **Model wiring in GHA uses direct providers via secrets; default provider is
   GitHub Models** (`models.github.ai`, `GITHUB_TOKEN` + `models: read`) as the
   zero-secret baseline. OmniRoute remains a Hermes control-plane concern and is
   deliberately NOT exposed to runners.
   [#23](https://github.com/os-santiago/ai-sdlc/issues/23),
   [#25](https://github.com/os-santiago/ai-sdlc/issues/25).
6. **Repository rename** of `homedir-ai-sdlc` (candidate: `ergaster`) is deferred
   to maintainer discussion —
   [homedir-ai-sdlc#116](https://github.com/os-santiago/homedir-ai-sdlc/issues/116).
   Cosmetic; does not block the cutover.

## Evidence gate

Cutover execution (worker flag-off, VPS decommission, repo archive) is blocked
until ALL of the following hold. The gate is evidence, not backlog completion:

- [ ] ~5 real homedir issues reach merged PRs through the GHA loop with zero
      human clicks
- [ ] At least one failing-CI PR is auto-repaired by `ci-repair`
      ([#27](https://github.com/os-santiago/ai-sdlc/issues/27)) — the non-happy
      path is what proves autonomy
- [ ] 1–2 week soak with GHA as the only active loop on homedir; the VPS worker
      stays paused-but-available as rollback (`podman start`)
- [ ] No silent stalls: green-idle and red-stall PRs resolve or escalate
      (sweep [#29](https://github.com/os-santiago/ai-sdlc/issues/29), or manual
      detection acceptable during pilot if documented)
- [ ] Extraction checklist in
      [homedir-ai-sdlc#117](https://github.com/os-santiago/homedir-ai-sdlc/issues/117)
      complete (runbooks ported, host secrets scrubbed, teardown steps listed)

## Bootstrap paradox

The pipeline cannot fix its own runtime yet: the implement stage cannot reach an
inference provider ([#23](https://github.com/os-santiago/ai-sdlc/issues/23)) and
no workflow consumes `.ai-sdlc.yaml`
([#22](https://github.com/os-santiago/ai-sdlc/issues/22)). The first runtime
fixes land **by hand**; the loop self-hosts only after those land.

## Consequences

### Dies with the cutover

- VPS bash worker (`platform/scripts/homedir-sdlc-worker.sh`) and the Podman pod
- VPS-hosted observability (Quarkus dashboard, events-service) — GitHub-native
  observability (run logs, artifacts, PR comments) suffices for a single
  consumer; extraction is reconsidered only if a second consumer appears
- `future-go/` spike — archived with the repo, not ported

### Survives (ported or referenced)

- Operational runbook knowledge in `homedir-ai-sdlc/CLAUDE.md` (config.json as
  sole permissions source, timeout calibration, env-file format pitfalls,
  partial-deployment pattern) → this repo's ops documentation
- ADRs 0001–0003 in homedir-ai-sdlc remain as historical record
- Open feature issues in homedir-ai-sdlc that still matter are re-filed against
  this repo or upstream `sc-agent-cli`, not ported wholesale

### New obligations for this repo

- Contract resolution ([#22](https://github.com/os-santiago/ai-sdlc/issues/22)):
  the runtime must actually consume `.ai-sdlc.yaml`, or the spec is decoration
- Headless provider wiring ([#23](https://github.com/os-santiago/ai-sdlc/issues/23))
  and the upstream env surface
  ([sc-agent-cli#455](https://github.com/os-santiago/sc-agent-cli/issues/455))
- Model fallback ([#24](https://github.com/os-santiago/ai-sdlc/issues/24)),
  review stage ([#28](https://github.com/os-santiago/ai-sdlc/issues/28)),
  stall-sweep ([#29](https://github.com/os-santiago/ai-sdlc/issues/29))

### Rejected alternatives

- **Expose OmniRoute to runners**: an unauthenticated model proxy on the public
  internet is a credential-exfiltration and quota-abuse vector.
- **Merge homedir-ai-sdlc into this repo**: couples a versioned contract surface
  to one org's stateful deployment.
- **Bot user account** (the old `homedir-sdlc[bot]` pattern): credential
  maintenance and ToS gray area; GitHub Apps supersede it.
- **Waiting for backlog-zero before cutover**: the backlog is self-generating by
  design; it never reaches zero. Evidence, not emptiness.

## Rollback

- VPS worker pod is paused, not deleted, during the soak; rollback = restart the
  pod and un-label-route homedir issues away from GHA.
- Repo archive is reversible (unarchive) for 90+ days after cutover.
