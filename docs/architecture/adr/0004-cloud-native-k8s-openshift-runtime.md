# ADR-0004: Cloud-native (Kubernetes/OpenShift) alternate control plane

- **Status**: Proposed
- **Date**: 2026-10-16
- **Deciders**: see repo CODEOWNERS
- **Tracking**: new feature; no issue filed yet

## Context

ADR-0001 sunsets `os-santiago/homedir-ai-sdlc`'s bash/VPS worker in favor of
this repo's GHA-native runtime for the `homedir` consumer repo. That
decision stands: GHA remains the default, zero-infrastructure control
plane for any repo that can use GitHub Actions.

Not every adopter can. Self-hosted GitHub/GitLab instances, air-gapped
environments, and orgs with an existing Kubernetes/OpenShift platform team
need a control plane that runs as cluster workloads instead of GitHub-hosted
or self-hosted Actions runners. `homedir-ai-sdlc` already contains an
unmerged, unreleased answer to this: `future-go/` — four small, independently
testable Go microservices (`admission-controller`, `orchestrator`, `worker`,
`release-manager`) with zero third-party dependencies, 100% unit-tested pure
domain logic, distroless non-root containers, and a Kustomize GitOps layout
(default-deny `NetworkPolicy`, Pod Security `restricted`, dropped
capabilities, read-only root filesystems). It was built against ADR-0003's
bounded-delivery stage contracts (admission → plan → bounded edit →
validate → publish/review → release) but never wired to a real event bus,
never deployed, and — per ADR-0001 — dies with the VPS worker it was meant
to eventually replace in that repo.

That code is good; the repo it lives in is being archived. This ADR ports it
here, as a new, explicitly optional control plane alongside (not replacing)
the GHA-native one.

## Decision

1. **Add `runtime/k8s/`**: a self-contained Go module (`go.mod` at
   `runtime/k8s/`) with four components ported from
   `homedir-ai-sdlc/future-go/components/*`, each independently
   containerized and independently deployable.
2. **Drop `implementation`** (the fifth future-go component, which wraps
   `sc-agent-cli` directly). Engine invocation is a single integration
   point this repo already owns via the headless contract
   (`--prompt-file --audit-log --summary-file --no-commit`, documented in
   the root README and `.ai-sdlc.yaml`'s `engine` block). A second,
   k8s-specific engine client would duplicate that contract and let it
   drift. The ported `worker` component evaluates bounded attempts
   (`cycle.Evaluate`); it is expected to shell out to the same `scc`
   invocation the GHA `ai-sdlc-implement.yml` stage uses, not reimplement
   it.
3. **Spec-first, not policy-duplicated**: `homedir-ai-sdlc`'s
   `future-go/policies/defaults/v1/admission.yaml` is **not** ported as a
   second policy source. Instead, `internal/contract` loads a repo's actual
   `.ai-sdlc.yaml` (validated against this repo's own
   `spec/ai-sdlc.schema.json`) and each component derives its policy
   defaults from it (`admission.PolicyFromContract`,
   `cycle.PolicyFromContract`, `gates.ApplyContractDefaults`). One contract
   drives both control planes.
4. **Intake via the existing normalized event, not a bespoke `Issue`
   struct**: `internal/normalizedevent` decodes `spec/normalized-event.schema.json`
   payloads (the schema this repo's GHA intake stage already produces) and
   projects them into the admission component's `Issue` type, including
   acceptance-criteria extraction per `spec/definition-of-ready.md`
   rules R2/R3. The trigger surface does not change based on which control
   plane executes the pipeline.
5. **Three deployment surfaces**, all rendering from the same base:
   - `deploy/kustomize/base` + `overlays/{development,production}` —
     ported from `future-go/deploy/gitops`, renamed off the `homedir-ai-sdlc`
     namespace.
   - `deploy/kustomize/overlays/openshift` — **new**. Adds a `Route`
     (admission-controller's intake endpoint is the only externally exposed
     surface), `ImageStream`/`BuildConfig` pairs per component for
     in-cluster source-to-image builds, and swaps image references to the
     internal registry. No custom `SecurityContextConstraints` object is
     needed: every container already runs as a fixed non-root UID (image
     `USER 65532:65532`) with `allowPrivilegeEscalation: false`,
     `capabilities: {drop: [ALL]}` and `readOnlyRootFilesystem: true`, which
     satisfies OpenShift's default `restricted-v2` SCC without an explicit
     binding.
   - `deploy/helm/ai-sdlc-k8s-runtime` — **new**. A Helm-chart wrapping of
     the same topology for teams that standardize on Helm over Kustomize;
     the OpenShift `Route` template guards on
     `.Capabilities.APIVersions.Has "route.openshift.io/v1"`, so the same
     chart is safe on vanilla Kubernetes.
6. **This is additive, not a replacement.** The GHA-native runtime stays
   the default path in the root README's onboarding flow. `runtime/k8s/` is
   documented as an alternative for self-hosted/air-gapped adopters —
   analogous to how Hermes is an optional control-plane surface per the
   root README's layering diagram.

## Schema gaps closed

Porting `admission-controller` and `worker` originally surfaced two fields
with no `spec/ai-sdlc.schema.json` equivalent. Both are now closed, as an
explicitly **additive, opt-in** schema change — no existing `.ai-sdlc.yaml`
(including this repo's own, and every file under `examples/`) required any
edit; all four re-validated against the updated schema unchanged.

- **`triggers.min_body_length`** (default **120**) and
  **`triggers.acceptance_criteria.{min,max}`** (defaults **1** / **null**).
  `spec/definition-of-ready.md` documents R4 ("body length ≥ 120 chars")
  and the atomicity ceiling ("1-8 criteria... >8 probably not atomic") as
  prose/CI rules; they now have a schema home. `min_body_length` and
  `acceptance_criteria.min` default to the exact numbers
  `.github/workflows/ai-sdlc-intake.yml` already hardcodes (120, 1) — zero
  behavior change for the live pipeline if it is ever wired to read them.
  `acceptance_criteria.max` defaults to **null (unbounded)**, because the
  live intake workflow does not enforce a ceiling today; the field is
  opt-in so adding it cannot retroactively fail an issue no repo asked to
  bound.
- **`review.max_changed_files`** (default **null/unbounded**), the
  bounded-edit "atomic file limit". No runtime stage enforces this today,
  so it is opt-in for the same reason.
- `minimumTitleLength` stays local-only (`admission.DefaultPolicy`, no
  schema field): DoR R1 is a title-prefix regex, not a numeric floor, so
  there is no real spec number to promote.

`runtime/k8s` is fully wired to the new fields: `admission.PolicyFromContract`
reads `min_body_length` and the acceptance-criteria ceiling (0 = unbounded
sentinel, guarded in `Evaluate` so it is never misread as "zero allowed");
`cycle.PolicyFromContract` reads `max_changed_files` the same way.
`admission.DefaultPolicy()`'s standalone body-length floor was also
corrected from future-go's arbitrary 30 to the real DoR number (120), since
nothing depended on the old value beyond this repo's own tests.

**Deliberately not touched**: `.github/workflows/ai-sdlc-intake.yml` and
`.github/actions/load-config/` (`resolve.py`/`resolve.sh`). Those drive the
actual, currently-running GHA pipeline for this repo (and any adopter) —
wiring their hardcoded R3/R4 checks to the new fields is real, valuable
follow-up work, but it touches the composite action every stage calls
first and was explicitly out of scope for "close the schema gap without
disrupting the current implementation." The schema now has a stable home
for these numbers; migrating the GHA checks to read them is a separate,
reviewable PR.

Two fields that *did* map cleanly in the original port, and are
contract-driven instead of hardcoded (future-go had them as constants):

- `release-manager`'s `MaximumRemediation` now defaults from
  `merge.max_repairs` (schema default 3) instead of future-go's hardcoded 5.
- `worker`'s `MaximumAttempts` now defaults from `review.max_fix_iterations`
  (schema default 3) instead of future-go's hardcoded 5.

## Consequences

### Ported essentially unchanged
- `internal/platform/service` (shared HTTP runtime: health/ready/metadata,
  bounded timeouts, structured logging, strict JSON).
- `orchestrator`'s dependency-ordered plan reconciliation
  (`internal/planning`) — pure, no contract coupling needed.
- `contracts/events/v1/issue-admission-decided.schema.json` and
  `contracts/commands/v1/work-item-ready.schema.json`.
- The Kustomize base topology, security posture and GitOps promotion model
  (`deploy/kustomize/*/README.md`).

### New in this port
- `internal/contract` (loads `.ai-sdlc.yaml`), `internal/normalizedevent`
  (decodes `spec/normalized-event.schema.json`).
- `PolicyFromContract` / `ApplyContractDefaults` on three of the four
  components.
- Two new event contracts (`implementation-attempt-decided`,
  `release-gate-decided`) completing the four-stage event trail; future-go
  only had the first two stages' contracts.
- The OpenShift Kustomize overlay and the Helm chart.

### Explicitly out of scope (this PR)
- Wiring to a real event bus (NATS, per the ConfigMap's
  `AI_SDLC_EVENT_BUS_URL` convention) or durable state store — components
  currently expose synchronous HTTP evaluation endpoints only, exactly as
  future-go left them. Making this an actually-running pipeline (not just a
  buildable/testable scaffold) is follow-up work and should get its own ADR
  once there's a concrete adopter.
- Engine invocation from the `worker` component (see Decision #2).
- Reviving the Quarkus dashboard — ADR-0001 already decided GitHub-native
  observability suffices for a single consumer; nothing here changes that.

## Rollback

Delete `runtime/k8s/`. Nothing outside that directory (and this ADR) is
touched — the GHA-native stages, `spec/`, and `.ai-sdlc.yaml` schema are
unmodified.
