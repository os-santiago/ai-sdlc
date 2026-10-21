# AI-SDLC Documentation

The canonical reference for AI-SDLC: normative spec, runtime workflows, and operational guides.

## Table of Contents

| Document | Purpose | Related Issues |
|---|---|---|
| [`spec.md`](spec.md) | Normative specification of the `.ai-sdlc.yaml` contract — schema, stages, defaults, and invariants | #1, #87 |
| [`runtime.md`](runtime.md) | Runtime workflows and operational model — event-driven execution, stage callers, and deployment topology | #30, #110 |
| [`security-scanning.md`](security-scanning.md) | Repo self-scanning posture — Dependabot + CodeQL + gitleaks secret scan, and the admin-scope repo-settings toggles that cannot ship in YAML | #105, #125 |
| [`security/baseline-scan.md`](security/baseline-scan.md) | Gitleaks full-history baseline — findings register, triage statuses (`false-positive` / `historical-revoked`), `.gitleaksignore` procedure | #125 |
| [`security/gate-calibration.md`](security/gate-calibration.md) | Security-gate calibration — which finding classes and `pr:risk-*` tiers block vs report the `ai-sdlc-security` check, and the org-floor semantics per `security.*` field | #90, #93 |
| [`metrics/definitions.md`](metrics/definitions.md) | Canonical DORA metric definitions + GitHub event sources for the generated `metrics/dora.md` report — agentic-vs-human classifier, failure/recovery boundaries, external-benchmark caveat | #88, #94 |
| [`architecture/adr/`](architecture/adr/) | Architecture Decision Records — binding decisions and open proposals: ADR-0001 (homedir cutover), ADR-0004/0005 (k8s control plane, proposed), ADR-0006 (caller topology) | #30, #110 |

## Repo presentation (locked decisions, #111)

Repo-level settings that cannot ship in YAML, recorded here as the
canonical reference:

| Setting | Value |
|---|---|
| Description (`ai-sdlc`) | `Canonical AI-SDLC — normative spec + GitHub-Actions-native runtime` |
| Description (`sc-agent-cli`, sibling) | `scc — agent CLI engine for AI-SDLC` |
| Topics | `ai-sdlc`, `github-actions`, `agentic-pipeline`, `automation` |
| Social preview image | **Deferred** — no designed asset exists; revisit with the branding/packaging work (#91) rather than shipping a placeholder |
