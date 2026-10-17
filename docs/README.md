# docs/ — Runtime reference & decision records

Operational documentation for the AI-SDLC runtime: the `workflow_call`
stage reference and the architecture decision records (ADRs) that bind it.

## Contents

| Artifact | Purpose | Issue |
|---|---|---|
| [`onboarding.md`](onboarding.md) | One-command install (`gh ai-sdlc init` bootstrap PR) and the shadow-start trust ramp — new repos start at `shadow` and climb on evidence | #207 |
| [`runtime.md`](runtime.md) | Reusable `workflow_call` stage reference — workflows, contract resolution, inputs/secrets, provider resolution, repair loop | #7 |
| [`regression-harness.md`](regression-harness.md) | Golden-fixture replay suite for pipeline decisions (DoR verdicts, merge gate, risk tiers, adapter resolution, contract resolution) — bless/update workflow and case format | #193 |
| [`spec/interop-posture.md`](../spec/interop-posture.md) | Interop posture — ai-sdlc as the normative governance layer over pluggable engines; the seam (normalized events + headless engine contract), the conformance path, and the non-goals | #216 |
| [`architecture/adr/`](architecture/adr/) | Architecture Decision Records — binding decisions; currently ADR-0001 (homedir-ai-sdlc sunset and cutover) | #30 |
