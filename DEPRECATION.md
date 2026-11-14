# GitHub Actions runtime deprecation

## Status

The GitHub Actions AI-SDLC implementation is deprecated and removed from the active
branch. This includes reusable orchestration workflows, composite actions, the GitHub
Actions bootstrap path, workflow-specific scripts, and their test fixtures.

The final preserved implementation is the annotated Git tag
[`deprecated-github-actions-runtime-2026-11-14`](https://github.com/os-santiago/ai-sdlc/tree/deprecated-github-actions-runtime-2026-11-14).

## Replacement

`runtime/k8s/` is the sole supported execution runtime. It uses the portable
contracts under `spec/` and is deployed through its Kustomize or Helm
surfaces.

## Migration boundary

Existing GitHub Actions consumers must pin or fork the deprecation tag before
updating. New runtime work must not reference retired `ai-sdlc*.yml` workflows, composite actions,
`gh ai-sdlc`, or the removed bootstrap installer. Migrate execution and
operational controls to the Kubernetes/OpenShift runtime through a reviewed,
environment-specific rollout.

This repository does not provide automatic migration of GitHub secrets,
repository variables, labels, or workflow permissions. Those are environment
state and must be reviewed by the owning operator.

## Quality validation remains active

The protected main branch still requires `contract`, `gitleaks`, `sbom`,
`codeql (javascript)`, and `CodeQL` checks from their configured GitHub Apps.
These checks are retained as independent repository validators. `contract`
validates portable examples and tests the Go runtime against PostgreSQL.
CodeQL scans Go and the retained JavaScript security-log utility; its existing
JavaScript check name remains accurate. The SBOM covers the current Go module.
Actionlint and security-log integrity/signing remain repository assurance,
not AI-SDLC execution. See [quality checks](docs/quality-checks.md).

No issue intake, generative implementation, AI review/fix, automated merge,
deployment, or AI-SDLC release stage runs in these workflows. Branch
protections remain in force and require actual successful validation.
