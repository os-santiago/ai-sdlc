# docs/ — Runtime reference & decision records

Operational documentation for the AI-SDLC runtime: the `workflow_call`
stage reference and the architecture decision records (ADRs) that bind it.

## Contents

| Artifact | Purpose | Issue |
|---|---|---|
| [`runtime.md`](runtime.md) | Reusable `workflow_call` stage reference — workflows, contract resolution, inputs/secrets, provider resolution, repair loop | #7 |
| [`positioning.md`](positioning.md) | Interop posture — ai-sdlc as the normative governance layer over pluggable engines; the seam (normalized events + headless engine contract), the conformance path, and the non-goals | #216 |
| [`architecture/adr/`](architecture/adr/) | Architecture Decision Records — binding decisions; currently ADR-0001 (homedir-ai-sdlc sunset and cutover) | #30 |
| [`problems/`](problems/) | Problem docs — options/trade-offs analyses with open questions that feed ADRs; the design-rigor surface | #215 |
