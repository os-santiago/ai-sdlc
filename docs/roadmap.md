# Roadmap to an adoptable v1

Goal: a release that another team can install, point at **their own
repository**, and evaluate with reproducible results. v1 prioritizes the inner
loop (issue to merged, verified and reviewed pull request). A generic outer
loop (build, sign, deploy) follows as v1.1. Status of each capability:
[capability matrix](capabilities.md).

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

## v1.1, M3: Pluggable outer loop

- Builds from a referenced Tekton/Konflux pipeline, with a reusable slot pool
  ([design](architecture/build-slot-provisioning.md)).
- Deployment targets described by configuration (workload, service, secrets,
  health probes), with a GitOps or Konflux release deployer.
- Provenance policy bound to the project, not to a named compiler.
