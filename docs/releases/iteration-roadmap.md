# Iteration roadmap: how AI-SDLC improves, and what comes next

This document records how the project's goals and decisions have evolved. It
derives a projection of the next decisions and improvements from that history.
It complements the [release plan](release-plan.md), which owns the stage exit
criteria, and the [strategic principles](../architecture/strategic-principles.md),
which every change is judged against. Work runs in 24-hour iterations; each
iteration ends with a summary and the plan for the next one.

## How the goals evolved

Each step widened the goal without dropping the previous one:

| Step | Goal as stated by the maintainer |
|---|---|
| 1 | Make the autonomous flow work end to end on a local cluster |
| 2 | Show end to end that AI-SDLC is a market standard for agentic software development, secure and of high quality |
| 3 | Build a definitive version that others can share, adopt, and use with results in real scenarios |
| 4 | An open-source project that receives issues and runs the SDLC autonomously, with quality and security best practices, compatible with Kubernetes and OpenShift |
| 5 | Mark an alpha; grow the evaluation batteries and reach acceptable success rates; validate on OpenShift |
| 6 | Certify autonomous operation in repeated measure-and-fix cycles |
| 7 | Work with the only model available: one served locally, on the maintainer's hardware |
| 8 | Audit the architecture against five strategic principles (P1–P5); add the Helm chart and Operator path |
| 9 | Develop the product through every release stage: Alpha → Beta → Release Candidate → General Availability |
| 10 | Get the most from the local hardware (integrated GPU, memory, CPU) and keep iterating on the principles |
| 11 | Continue autonomously in 24-hour iterations, guided by this projection |

## The improvement pattern

The same loop produced every result so far:

1. **Measure with real issues, not synthetic checks.** Evaluation batteries run
   the full flow on a sample repository, with the trigger label as the only
   input.
2. **Name every stop.** A stop that reads `unknown` is a defect in itself.
   Bounded, allowlisted diagnostic names turned each opaque failure into a
   cause (for example `publication-helper-exit-N`).
3. **Capture, replay, then fix.** For non-obvious failures, a bounded
   evaluation capture of the real request plus a replay against a reference
   found the cause:
   - the GPU backend's intermittent corruption;
   - the generator's miscounted diff hunks;
   - a hidden 300-second client default.
4. **Fix deterministically and fail closed.** Generative steps get repaired
   outputs or narrowed choices, never relaxed gates. Examples: hunk-count
   repair, tool-list narrowing for review, and degenerate-output detection.
5. **Audit independently before merge.** Every change passes green CI, has no
   unresolved review threads, and is audited by a reviewer that is given the
   code, not the conclusion. Audits repeatedly found real defects before
   merge.
6. **Record the lesson.** Evidence documents in the repository, plus sanitized
   patterns in the operator's knowledge base.

## Decisions taken so far

| Decision | Reason |
|---|---|
| A local model behind an OpenAI-compatible gateway | The only model path available; it also proves the system works without a vendor API |
| ROCm/HIP with a 1,024-token micro-batch, not Vulkan, on the reference iGPU | Vulkan corrupted output intermittently; HIP with this setting stayed clean in repeated replays, and a restart clears a corrupted process ([batteries 19–24](../evidence/2026-11-25-local-model-batteries-19-24.md)) |
| The generator gets the manifest's own toolchain (B1) | Refused candidates mostly failed their own tests; running them before export is the cheapest fix |
| Configurable generation budget and answer bound (B2) | Slow local models need more time and longer answers; defaults stay byte-identical |
| The sandbox `RuntimeClass` is a checked value (B3) | Portable sandboxing across kind, OpenShift (Kata) and managed clouds |
| Keyless signatures verified before install (B5) | Adopters must be able to trust the images they run |
| Helm chart first, then an Operator rendering the same chart | One source of truth, with OLM lifecycle on top |

## Where we are

- **Stage:** late Alpha. Sandbox and signing items are in progress; the separate
  review authority (B4), signature verification at install and Kata validation
  remain. The progress table is in the [release plan](release-plan.md#progress).
- **Functional:** on the local model, three of ten issues went from label to a
  merged pull request with no human step in battery 24 (30% [10.8%, 60.3%], a
  first measurement, not yet gate evidence;
  [batteries 19–24](../evidence/2026-11-25-local-model-batteries-19-24.md)). Remaining
  attempts mostly fail on model capability: candidates whose tests fail, or
  steps running out. The independent reviewer's final answer sometimes misses
  the strict verdict format.
- **Not yet measured:** a delivery rate with a 95% interval per cell
  (repository × model path), which is the Beta functional gate.

## Projection: the next iterations

Ordered by impact on the Beta gate. Each item follows the improvement pattern
above.

### Iteration 1: measure and raise the local-model delivery rate

1. Finish battery 24. Capture any reviewer final answer that is refused, and
   fix the cause deterministically (guidance or a bounded format repair that
   never changes the verdict). Done: review tool narrowing and turn budget.
2. Run repeated batteries until each cell has enough issues for an interval.
   Record rates per issue and per stop class.
3. Close the issue from the merged pull request for manifest repositories (B9,
   #715). Done.
4. Keep the evaluation evidence current ([batteries 19–24](../evidence/2026-11-25-local-model-batteries-19-24.md)).
5. Find the cause of each dominant stop by reproducing one stopped attempt,
   not from its stop name alone. On 2026-11-25, applying one refused candidate
   locally showed a whole-file rewrite of a large test file. The fix is a
   gateway guard (#797). Single failed API reads on a loaded cluster are now
   retried within bounds (#799, #800), and repeated source reads no longer
   exhaust the forge's anonymous quota (#802).

**Learned:** guidance alone did not change the model's behaviour (the rewrite
rule was already in the prompt). Mechanical limits at the tool-call boundary
did. Each change got an independent audit, and every audit found a real
defect before merge.

### Iteration 2: the second cell and the evaluation harness

1. A second sample repository in another language, with its manifest and
   toolchain (the Beta gate needs two repositories).
2. Evaluation harness v1 (B7): pass@k per cell with 95% intervals, a stop-class
   breakdown, and a reviewer confusion matrix.
3. Minimal metrics and a scheduled database backup (B10).
4. The component table checked in CI (B9, truthful docs).
5. A second model path for the second cell: the agent-CLI service, or a
   hosted model over HTTPS when an API key is available (B6b).

### Iteration 3: installable by others

1. A self-contained, signed, published Helm chart (B6): an OCI artifact, a
   documented Secret contract, and a generic Kubernetes path.
2. External PostgreSQL with TLS.
3. A quickstart verified on a clean kind cluster without the repository's
   local tooling, and an install on OpenShift with Kata when a full OpenShift
   cluster is available (an open environment condition otherwise).

### Iteration 4: separation of authority and quality gates

1. An independent review identity; merge requires a co-signature (B4).
2. A candidate quality and security baseline in the merge gate (B8): static
   analysis, dependency and secret scanning.
3. The rest of B5: image scanning, attestation verification, and signature
   verification when installing from a committed lock and at admission.

### Then: the Beta release

When the exit criteria hold, prepare `v0.2.0-beta.1` notes. Each criterion
cites its evidence, and any open conditions are stated, for example Kata
availability or a hosted model path. Release tags need maintainer approval.

### Beyond Beta (from the release plan)

- **Release Candidate:**
  - Operator `v1beta1` and OLM;
  - upgrade, rollback, backup and restore;
  - high availability;
  - traces and alerts;
  - admission policies and verified provenance;
  - a split release manager;
  - predictive signals in shadow mode;
  - a reviewed threat model.
- **General Availability:**
  - stable Operator channel;
  - support matrix with managed clouds;
  - air-gapped and multi-architecture;
  - risk-tiered human approval;
  - predictive admission.

## Rules for every iteration

- Changes reach `main` only with green CI, no unresolved review threads, and an
  independent audit.
- Defaults stay byte-identical unless a change is the point; upgrades must not
  silently reset configuration.
- No change relaxes a gate to improve a rate.
- Evaluation infrastructure (capture proxies, guards) stays outside the
  product, and the evidence says so.
