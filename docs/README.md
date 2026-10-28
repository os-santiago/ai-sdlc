# docs/ — Runtime reference & decision records

Operational documentation for the AI-SDLC runtime: the `workflow_call`
stage reference and the architecture decision records (ADRs) that bind it.

## Contents

| Artifact | Purpose | Issue |
|---|---|---|
| [`onboarding.md`](onboarding.md) | One-command install (`gh ai-sdlc init` bootstrap PR) and the shadow-start trust ramp — new repos start at `shadow` and climb on evidence | #207 |
| [`runtime.md`](runtime.md) | Reusable `workflow_call` stage reference — workflows, contract resolution, inputs/secrets, provider resolution, repair loop | #7 |
| [`operations.md`](operations.md) | Operations runbook — `scc:*` label state machine, failure modes, artifacts, kill-switch, recovery paths | #143 |
| [`regression-harness.md`](regression-harness.md) | Golden-fixture replay suite for pipeline decisions (DoR verdicts, merge gate, risk tiers, adapter resolution, contract resolution) — bless/update workflow and case format | #193 |
| [`self-healing-parity.md`](self-healing-parity.md) | Self-healing parity map — Hermes control-plane feature → GHA-native equivalent → forge-agnostic contract, plus the `scripts/self-heal.js` orchestrator (`plan`/`classify`/`postmortem`/`sentinel`) | #187 |
| [`runbooks/failure_modes.md`](runbooks/failure_modes.md) | Machine-readable failure-mode runbook — `scripts/failure-runbook.js` injects recorded fixes into ci-repair briefs (epic #146) | #146 |
| [`engine_contract.md`](engine_contract.md) | Headless engine contract + certification process for third-party engines (`--prompt-file`, `--audit-log`, `--summary-file`, `--no-commit`) | — |
| [`positioning.md`](positioning.md) | Moved — superseded by [`spec/interop-posture.md`](../spec/interop-posture.md) | #250 |
| [`problems/ai-contributions.md`](problems/ai-contributions.md) | Problem analysis — managing AI-generated contributions in open-source projects (DCO, vouch, review trade-offs) | — |
| [`metrics/dora.md`](metrics/dora.md) | DORA metrics snapshot (generated) — deploy/lead-time definitions (epic #88) and the per-window numbers | #88 |
| [`fleet-dashboard.md`](fleet-dashboard.md) | Generated fleet dashboard — per-repo autonomy/run-health window from [spec/run-evidence.md](../spec/run-evidence.md) artifacts | #141 |
| [`spec/interop-posture.md`](../spec/interop-posture.md) | Interop posture — ai-sdlc as the normative governance layer over pluggable engines; the seam (normalized events + headless engine contract), the conformance path, and the non-goals | #216 |
| [`security-log.md`](security-log.md) | Tamper-evident `SECURITY_LOG.md` — hash-chained security event log, `verify`/seal/cosign attestation, auditor verification steps | #106 |
| [`security-scanning.md`](security-scanning.md) | Repo self-scanning posture — Dependabot + CodeQL + gitleaks secret scan, and the admin-scope repo-settings toggles that cannot ship in YAML | #105, #125 |
| [`security/baseline-scan.md`](security/baseline-scan.md) | Gitleaks full-history baseline — findings register, triage statuses (`false-positive` / `historical-revoked`), `.gitleaksignore` procedure | #125 |
| [`security/gate-calibration.md`](security/gate-calibration.md) | Security-gate calibration — which finding classes and `pr:risk-*` tiers block vs report the `ai-sdlc-security` check, and the org-floor semantics per `security.*` field | #90, #93 |
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
