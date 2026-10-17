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
| [`spec/interop-posture.md`](../spec/interop-posture.md) | Interop posture — ai-sdlc as the normative governance layer over pluggable engines; the seam (normalized events + headless engine contract), the conformance path, and the non-goals | #216 |
| [`security-log.md`](security-log.md) | Tamper-evident `SECURITY_LOG.md` — hash-chained security event log, `verify`/seal/cosign attestation, auditor verification steps | #106 |
| [`architecture/adr/`](architecture/adr/) | Architecture Decision Records — binding decisions; currently ADR-0001 (homedir-ai-sdlc sunset and cutover) | #30 |
