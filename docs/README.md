# Documentation

Start here:

- [Strategic principles](architecture/strategic-principles.md): the five
  architectural commitments every component and change is judged against, and
  the [principles audit and development plan](architecture/principles-audit-2026-11-24.md).
- [Capability matrix](capabilities.md): what is generic, project-specific or not
  yet proven.
- [Roadmap](roadmap.md): the path to an adoptable v1.
- [Release plan](releases/release-plan.md): where AI-SDLC stands and the exit
  criteria for Beta, Release Candidate and General Availability.
- [Latest end-to-end evidence](evidence/2026-11-20-issue-only-e2e.md).
- [Evaluation harness](operations/evaluation-harness.md): delivery rates per
  repository and model path, with 95% intervals and the Beta gate check.
- [Runtime upgrades and installation details](operations/runtime-upgrades.md).

Current operational documentation lives with the Kubernetes/OpenShift runtime:

- [Installation](installation.md) — one entrypoint, local kind, Helm desired
  state, Quay images and the maintenance-mode boundary.
- [Enterprise pilot](enterprise-pilot.md) — Homedir validation, iteration gates,
  Konflux integration and Fullsend comparison.

- [runtime/k8s/README.md](../runtime/k8s/README.md) — components, tests,
  images, and deployment surfaces.
- [DEPRECATION.md](../DEPRECATION.md) — GitHub Actions retirement and the
  preserved historical tag.

The former GitHub Actions orchestration documentation was removed with that
implementation. Repository [quality checks](quality-checks.md) and [security log](security-log.md)
remain supported through GitHub Actions.
