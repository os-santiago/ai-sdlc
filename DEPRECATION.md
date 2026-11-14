# GitHub Actions runtime deprecation

## Status

The GitHub Actions implementation is deprecated and removed from the active
branch. This includes reusable workflows, composite actions, the GitHub
Actions bootstrap path, workflow-specific scripts, and their test fixtures.

The final preserved implementation is the annotated Git tag
[`deprecated-github-actions-runtime-2026-11-14`](https://github.com/os-santiago/ai-sdlc/tree/deprecated-github-actions-runtime-2026-11-14).

## Replacement

`runtime/k8s/` is the sole supported execution runtime. It uses the portable
contracts under `spec/` and is deployed through its Kustomize or Helm
surfaces.

## Migration boundary

Existing GitHub Actions consumers must pin or fork the deprecation tag before
updating. New work must not reference `.github/workflows/`, composite actions,
`gh ai-sdlc`, or the removed bootstrap installer. Migrate execution and
operational controls to the Kubernetes/OpenShift runtime through a reviewed,
environment-specific rollout.

This repository does not provide automatic migration of GitHub secrets,
repository variables, labels, or workflow permissions. Those are environment
state and must be reviewed by the owning operator.

## Merge prerequisite

The protected main branch still requires `contract`, `gitleaks`, `sbom`,
`codeql (javascript)`, and `CodeQL` checks from their configured GitHub Apps.
Removing workflow files does not migrate this external configuration. Before
merging, maintainers must provision replacement validators and migrate the
required-check policy to those verified checks. Do not fabricate passing
statuses or disable protections to land this change.
