# Roadmap to an adoptable v1

Goal: a release that another team can install, point at **their own
repository**, and evaluate with reproducible results. v1 prioritizes the inner
loop (issue to merged, verified and reviewed pull request) and runs on standard
Kubernetes and OpenShift. A generic outer loop (build, sign, deploy) follows as
v1.1. Development happens first on local kind; when the criteria of the v1
milestones that apply to kind pass there (for M5: the kit and installer run on
kind without kind-specific mechanisms), testing, adjustment and evolution move
to an OpenShift cluster, where the remaining M5 criteria are validated. Milestones keep their original
numbers: M3 (the outer loop) was moved after M4 when the inner loop was chosen
as the v1 core, and M5 (platform portability) was added after it. Status of each capability: [capability matrix](capabilities.md).

## M0: Truthful positioning

- Product README, capability matrix, this roadmap.
- A security overview mapping trust boundaries, credentials and the data sent to
  a model provider, and a real disclosure contact.
- A published evidence record of the latest end-to-end run.

**Accepted when** a newcomer can tell what ai-sdlc does, what it does not, and
how it is secured, in 10 minutes.

## M1: Generic inner loop (v1 core)

- **Project manifest.** A signed, installed declaration per repository:
  - repository;
  - verification image and test command;
  - allowed and denied path globs;
  - merge policy.

  It replaces the compiled two-repository registry and the fixed file
  allowlists.
- **Verification with the repository's own tests**, run in the existing sandbox
  with deterministic, bounded results. Reviewed per-issue oracles become an
  optional plug-in.
- **Publication and merge driven by the manifest** and policy instead of
  per-repository Go.

**Accepted when** a repository that is not a reference project, with no Go
changes, goes from an authorized issue to a merged pull request whose tests and
independent review passed.

**Status (2026-11-21): met once, released as `v0.1.0-alpha.1`**
([evidence](evidence/2026-11-21-generic-inner-loop-e2e.md)). Follow-ups:
running the repository's tests inside the generator's sandbox, strong
decisions from a JUnit report (`verification.result.junitPath` is supported;
the evaluated repository does not emit one yet). The generated pull request
now closes its issue for manifest repositories (`Closes #N`); the built-in
references keep `Refs #N` because their outer loop continues after the merge.

## M2: Repeatable operations

- One declarative installation bundle and a `doctor` preflight.
- A generic `upgrade --to <release>` replacing per-change transitions.
- Policy-based approval rotation, with alerts before expiry.
- Approved retries of a failed stage for an already-merged change.
- A per-issue stage view with reason codes, and metrics for slots, approvals and
  model calls.

**Accepted when** an installation runs a stream of issues for two weeks without
bespoke intervention.

## M4: Evaluation kit and evidence

- `install.sh eval` against any OpenAI-compatible model service, with a sample
  repository fork.
- A seeded corpus of 5 to 10 low-risk issues across 2 to 3 repositories,
  including a deliberate injection or failure case.
- An automated report: outcome and stop reason per issue, model calls and
  tokens, wall time, and the denominator.
- Tagged `v1.0` release with images, chart, lock and changelog.

**Accepted when** an external team can reproduce the published results in under
a day.

## M5: Platform portability (v1)

- Replace kind-specific mechanisms with cluster equivalents:
  - build slots on PVCs from a StorageClass, instead of node tmpfs mounts
    created through the container runtime;
  - a configurable external registry, instead of only the managed local one;
  - no installer step that needs access to the node or host.
- OpenShift compatibility: restricted SCCs, Routes, the operator-provided
  Pipelines (Tekton) and Chains.
- The evaluation kit (M4) and the installer run on any conformant cluster.

**Accepted when** the evaluation kit produces the same results on kind, on an
OpenShift cluster and on a managed Kubernetes cluster, with no
platform-specific manual steps beyond documented prerequisites. Kind is
completed first; OpenShift validation follows with access to a cluster.

## v1.1, M3: Pluggable outer loop

- Builds from a referenced Tekton/Konflux pipeline, with a reusable slot pool
  ([design](architecture/build-slot-provisioning.md)).
- Deployment targets described by configuration (workload, service, secrets,
  health probes), with a GitOps or Konflux release deployer.
- Provenance policy bound to the project, not to a named compiler.

**Accepted when** a repository that is not a reference project, using its own
declared pipeline and deployment target, goes from a merged pull request to a
signed, provenance-verified deployment with health evidence and a production
record, and repeats it for consecutive merges without manual slot or target
changes.
