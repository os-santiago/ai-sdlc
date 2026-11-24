# Principles audit and development plan, 2026-11-24

**Scope:** `main` at `b2f36d1`, audited against the
[strategic principles](strategic-principles.md). Five independent read-only
audits, one per principle, read the code, charts, workflows and docs, and
cited evidence by file and line. Their findings are consolidated here.
Evaluation evidence comes from
[batteries 8 to 13](../evidence/2026-11-23-evaluation-batteries-8-11.md).

**Maturity scale:**

| Level | Meaning |
|---|---|
| 1 | Absent |
| 2 | Partial or designed only |
| 3 | Defined and partly enforced |
| 4 | Enforced and measured |
| 5 | Optimized |

## Summary

| Principle | Maturity | Strongest point | Largest gap |
|---|---|---|---|
| P1. Independent, aligned components | **3** | Every handoff is a finite, signed, transactional contract with per-component database roles | The verifier and the release manager are hubs. Review shares the verifier's identity, and merge is self-granted |
| P2. Generative and predictive AI | **2** | Generative AI is confined to implementation, review and repair, behind deterministic gates | No predictive component, and no model evaluation harness |
| P3. Embedded policy and resilience | **3** | Integrity enforcement: signed authority, forced RLS, fenced leases, idempotent delivery, sandboxed exact-shape workloads | No metrics or traces, no HA, no database backup, no quality and security baseline on candidates |
| P4. Trusted supply chain | **2** | Rigorous design for produced artifacts (bound build records, closed schemas, short-lived trust) | The runtime's own images are unsigned. The installer trusts API metadata, not signatures. No vulnerability gate. The produced-artifact gate is not active |
| P5. Cloud native, standard install and upgrade | **2** | A real Helm chart, digest pins, careful upgrade journals, an OpenShift Local run | The supported target is local kind. The sandbox works only on kind. The chart is not self-contained. Lifecycle issues #430–#444 are open |

## P1. Independent, aligned components (3/5)

**Strengths:**
- **Signed handoffs.** Every handoff is a producer-signed outbox record, delivered by the relay to an inbox. The inbox checks a fixed operation→(source, target) table (`internal/authority/policy.go:57-74`), an autonomy floor, nonces and receiver acknowledgements.
- **Fixed routes.** The relay refuses any route set that is not exactly the reviewed one (`components/relay/cmd/main.go:66-100`).
- **Clear ownership.** Every stage maps to a named component.

**Gaps:**

| # | Severity | Gap | Recommendation |
|---|---|---|---|
| 1.1 | High | **Review is not an independent authority.** The verifier runs verification, review (holding the review model token), the repair handoff and the publication handoff in one loop. The provenance verifier is the same binary with the same signing identity (`components/verifier/cmd/main.go:116-147`, `deploy/helm/.../provenance.yaml:116`). | Build a separate reviewer role and identity, and a distinct identity for provenance verification. |
| 1.2 | High | **Merge is self-granted.** `merge-publication` is a release-manager→release-manager grant (`policy.go:66`). | Require a separate gate role, or a fresh reviewer or verifier co-signature, to merge. |
| 1.3 | Medium | **The release manager runs seven loops:** publication, merge, build dispatch, promotion, deploy dispatch, terminal write and production dispatch (`components/release-manager/cmd/main.go:177-262`). | Split it into an inner-loop publisher/merger and an outer-loop release coordinator, with separate roles and grants. |
| 1.4 | Medium | **Dead contracts.** `record-attempt` and `record-release` are declared with no producer or consumer, so the orchestrator is blind after planning until production. | Implement them, so the orchestrator owns per-issue state, or remove them. |
| 1.5 | Medium | **The model-mailbox image is really a shared toolkit.** It ships five binaries, including the verification test runner (`components/model-mailbox/tests-cmd/main_linux.go:29`). | Rename it or split it by owner. |
| 1.6 | Medium | **Stale responsibility docs.** `docs/innerloop-outerloop.md` and the `runtime/k8s/README.md` components table disagree with the code. There is no inner-loop architecture document. | Add a stage-ownership table generated from `policy.go` and service metadata, checked in CI. |
| 1.7 | Low | **Legacy code.** Unsigned legacy handlers sit next to live code (`internal/outbox`, `components/*/internal/{cycle,gates}`). | Delete them or move them under test support. |

Session evidence: the autonomous inner loop closes the issue only through the draft #715.

## P2. Generative and predictive AI (2/5)

**Strengths:**
- **Confinement.** Generative AI runs only in implementation (SCC in a sandboxed executor), independent review (read-only tools, a strict verdict, a bound digest) and one bounded repair.
- **Deterministic gates.** Every output passes deterministic gates: budgets, mailbox digests, repository-test decisions, and fail-closed benchmarks.
- **Enforced split.** The A-Dev profile states the split between deterministic and generative work (`deploy/helm/.../files/adev-profile.yaml:12-13`).

**Gaps:**

| # | Severity | Gap | Recommendation |
|---|---|---|---|
| 2.1 | High | **No model evaluation harness.** Batteries cover about 10 issues, one repository and one model. The startup benchmark measures speed only. | Build a versioned evaluation suite with a seeded corpus and ground truth across repositories, including injection and should-reject cases. Report pass@1 and pass@2 with intervals, and a reviewer confusion matrix. Make it a required gate for a model or adapter change. |
| 2.2 | High | **Admission risk is regex only** (`admission/evaluator.go:145-215`). | Add a predictive admission score that can only tighten a decision: decompose, or escalate to a human. Run it in shadow against battery outcomes first. |
| 2.3 | Medium | **Static budgets and one-shot timeouts.** Budget and timeout stops were about a quarter of recent failures (#750, exit codes 22 and 124). The gateway timeout comes from one startup sample (#758). | Predict per-issue budgets within the reviewed ceilings. Recalibrate timeouts from `model-call` percentiles (#757). |
| 2.4 | Medium | **Unconditional retry and repair, and no flake history** (`repositorytests/decide.go`). | Estimate retry value from stop class and history. Keep per-test pass/fail history and flake scores. |
| 2.5 | Medium | **Degradation is found by hand** (batteries 10 and 12). | Detect anomalies over stop-reason and latency rates. |
| 2.6 | Low | **The reviewer judges weak evidence** (no JUnit report), **and the verdict is free text.** | Require structured test reports for autonomy, and use a structured verdict. |
| 2.7 | Low | **The A-Dev profile has no predictive role.** | Add the role, with "tighten or advise only". |

Session evidence:
- The generator cannot run tests (#745). That is the top cause of refused candidates.
- A local model needs operational care: a long-context numerical instability on Vulkan required an f16 KV cache, a 32k context and one slot, measured with captured agent requests.

## P3. Embedded policy and resilience (3/5)

**Strengths:**
- **Signed authority:** Ed25519-signed, revisioned authority with a freeze switch (`internal/authority`).
- **Database roles:** 61 forced row-level security statements and 228 role-scoped policies across 38 migrations. Components refuse privileged database roles (`store/roles.go:28-40`).
- **Delivery:** fenced claims and leases, an idempotent outbox and inbox with replay protection, and bounded delivery attempts.
- **Workloads:** PSA restricted, deny-all NetworkPolicies, ResourceQuotas, a sandbox RuntimeClass, and exact pod-shape checks.
- **Recovery:** strong crash recovery through cleanup journals and recovery loops.
- **CI:** SHA-pinned actions, CodeQL, gitleaks, Dependabot.
- **Diagnostics:** every fail-closed stop now names its bounded cause (#742–#756).

**Gaps:**

| # | Severity | Gap | Recommendation |
|---|---|---|---|
| 3.1 | High | **The database is a single point of failure.** One instance, no scheduled backup, no point-in-time recovery (`values.yaml:60`, `postgres-cluster.yaml`). | Default CNPG to three instances, with object-store backup, WAL archiving and a tested restore. |
| 3.2 | High | **No metrics or traces** (`roadmap.md` M2). | Add Prometheus metrics (queue depth, lease expiries, gate denials, budgets) and OpenTelemetry traces carried through outbox headers, with alert rules. |
| 3.3 | High | **No baseline on candidate code.** Candidates are checked only by the target repository's tests and review. | Add a sandboxed baseline recipe (SAST, dependency and secret scan of the diff), bound into evidence and required by the merge gate. |
| 3.4 | Medium | **Policy lives in Go.** The one ValidatingAdmissionPolicy is generated in Go (`buildworkload/admission.go`). Executor and verifier pods have no admission control. | Render admission policies as chart templates with tests, and add pod-shape policies for executors and verifiers. |
| 3.5 | Medium | **No HA.** Every component is single-replica, with a `Recreate` strategy and no PDB. | Keep single-writer semantics with Lease-based leader election, warm standbys and PDBs. |
| 3.6 | Medium | **No human approval tiers.** The merge policy requires zero human approvals. | Require approval above a risk tier. |
| 3.7 | Low | **CI lacks** govulncheck, a Go linter and an image scan. Logs are mixed and carry no correlation IDs. | Add them. |

Session evidence: hours of operation exposed an intake freshness decay (fixed in #753). Resilience needs observation over time, not only unit tests.

## P4. Trusted supply chain (2/5)

**Strengths:**
- **Runtime image hygiene:** SHA-pinned actions, a digest-pinned Go builder, BuildKit provenance and SBOM generation.
- **Installer identity checks:** the installer accepts only exact-revision images from main-push publications (`tools/install_images.py`).
- **Produced artifacts:**
  - bound build records;
  - Tekton Chains signatures verified against a pinned key (`internal/provenance`);
  - verification that never authorizes on its own.

**Gaps:**

| # | Severity | Gap | Recommendation |
|---|---|---|---|
| 4.1 | High | **Runtime images are unsigned.** The installer trusts GitHub API metadata, not a signature (`containers.yml:9-10`, `install_images.py:39-107`). | Sign keyless with cosign and Rekor on main. Verify signer identity and issuer in the installer before writing pins. |
| 4.2 | High | **No signed SLSA provenance.** The BuildKit provenance is unsigned and unverified (an earlier attestation path regressed). | Attest build provenance per digest, and verify it at install. |
| 4.3 | High | **No admission-time signature policy** for runtime or produced images. | Ship an optional sigstore ClusterImagePolicy or Kyverno `verifyImages` policy, and the OpenShift image policy. |
| 4.4 | High | **No vulnerability gate** (epic #87). | Run govulncheck in CI. Scan each pushed digest, block fixable high and critical findings, and attest the result. |
| 4.5 | Medium | **Unbound SBOM, unpinned bases.** The SBOM is not attested to the image. Runtime bases use mutable tags, and digests are optional for the main components in the chart. | Attest the SBOM per digest. Pin every base image and make digests mandatory. |
| 4.6 | Medium | **Produced-artifact trust is fixture grade.** It uses a local key, no transparency log, SLSA v0.2, Conforma with an empty policy, and a hard-coded scope. The promotion gate is inactive (#455, #574, #439). | Move to keyless or KMS signing, SLSA v1, real Enterprise Contract policies, and an active promotion gate with a tamper test. |
| 4.7 | Low | **Docs overclaim** provenance verification before promotion (`docs/security-overview.md:45`). | Correct the docs. |

**SLSA levels today:**

| Artifact class | Level |
|---|---|
| Runtime images | Build L1 |
| Installer | L0 |
| Produced artifacts | About L2 in mechanism, L0 operationally |

## P5. Cloud native, standard install and upgrade (2/5)

**Strengths:**
- A Helm chart with a values schema, digest pins and re-appliable installs.
- Cluster-identity fencing.
- Journaled, identity-checked runtime upgrades.
- An OpenShift Local delivery with admission normalization.

**Gaps:**

| # | Severity | Gap | Recommendation |
|---|---|---|---|
| 5.1 | Critical | **No working sandbox outside kind.** gVisor fails under CRI-O, Kata is not evaluated, and OpenShift ran on `crun`. The RuntimeClass name is hard-coded (`executor/workload.go:77`). | Add a sandbox abstraction with a configurable RuntimeClass, mapped to GKE Sandbox, OpenShift sandboxed containers, and AKS or EKS Kata options. Preflight refuses autonomy without a sandbox. |
| 5.2 | High | **The chart is not self-contained.** The tag defaults to `unpublished`, Secrets are created by the installer, and templates fail without installer JSON (#430). | Go Helm first: a published OCI chart and images, and a documented Secret contract (ExternalSecrets or pre-created). The installer becomes a thin wrapper. |
| 5.3 | High | **The installer accepts only kind or OpenShift** and needs a local container engine (#434). | Add a generic Kubernetes path with capability preflight. |
| 5.4 | High | **Runtime upgrades are one-off scripts** hard-coded to kind contexts (#441). | Build one generic, ordered upgrade command reusing the journals. |
| 5.5 | High | **No external or managed database path.** The bundled URI uses `sslmode=disable`. | Support an external Postgres with TLS required, and document role provisioning. |
| 5.6 | Medium | **No uninstall, no backup or restore after cluster loss, no air-gapped install** (#442, #443). | Add `uninstall --keep-data`, scheduled backups, and a release bundle with mirror configuration. |
| 5.7 | Medium | **Local-topology assumptions.** The model endpoint must be a literal private IP. There is only an OpenShift Route, amd64-only images, fixed namespaces, and a stale Kustomize base. | Relax each assumption. Generate Kustomize from Helm or delete it. |

Operator/OLM: once the chart is self-contained, wrap it in a Helm-based
operator for an OLM channel. Move to a reconciling Go operator only when the
generic upgrade command exists.

Session evidence: a host reboot showed that host prerequisites and the local
model proxy are not persistent. The local evaluation topology is not a
deployment model.

## Development plan

The plan sequences work so that trust and truthfulness come first, then
operability and portability, then the enterprise outer loop. Each item names
the principle and gaps it closes, and the existing issues it continues.

### Phase 1: Trust and truthful structure (about 6 weeks)

| Item | Closes | Issues |
|---|---|---|
| Keyless signing of runtime images; signature verification in the installer; signed SLSA provenance and SBOM attestations per digest | P4 4.1, 4.2, 4.5 | #430 |
| Vulnerability gate: govulncheck plus image scan, blocking fixable high and critical | P4 4.4, P3 3.7 | #87 |
| Pin every base image by digest; make chart digests mandatory | P4 4.5 | new |
| A separate reviewer identity and role; provenance verifier identity; merge needs a co-signature | P1 1.1, 1.2 | new |
| Stage-ownership table generated from policy and service metadata, checked in CI; fix stale docs and overclaims | P1 1.6, P4 4.7 | new |
| Decide the dead contracts: implement `record-attempt` and `record-release`, or remove them | P1 1.4 | new |
| Candidate quality and security baseline (SAST, dependency and secret scan of the diff) required by the merge gate | P3 3.3 | new |
| Evaluation harness v1: seeded corpus, pass@k with intervals, reviewer confusion matrix, required for model changes | P2 2.1 | extends M4 |
| Generator toolchain in the sandbox | P2 (session evidence) | #745 |

### Phase 2: Operability and portability (about 6–8 weeks)

| Item | Closes | Issues |
|---|---|---|
| Sandbox abstraction with configurable RuntimeClass and provider mapping; preflight refuses autonomy without one; evaluate Kata on OpenShift | P5 5.1 | new |
| Self-contained, published Helm chart (OCI); Secret contract; thin installer | P5 5.2 | #430, #431, #432 |
| Generic Kubernetes install path; external Postgres with TLS | P5 5.3, 5.5 | #434 |
| Generic upgrade and rollback command; uninstall that keeps data; scheduled backup and tested restore; doctor | P5 5.4, 5.6, P3 3.1 | #441, #442, #443, #435 |
| CNPG high availability by default; leader election with standbys and PDBs | P3 3.1, 3.5 | new |
| Metrics, traces and alerts, with correlation IDs | P3 3.2, 3.7 | M2 |
| Admission policies as chart templates; executor and verifier pod-shape policies | P3 3.4 | new |
| Split the release manager into inner-loop publisher/merger and outer-loop coordinator | P1 1.3 | new |
| Predictive in shadow: budget and timeout calibration from `model-call` events; retry gating; flake history | P2 2.3, 2.4 | #750, #758 |

### Phase 3: Enterprise outer loop and distribution (12 weeks and beyond)

| Item | Closes | Issues |
|---|---|---|
| Produced artifacts: keyless or KMS Chains signing, SLSA v1, real Enterprise Contract policies, active promotion gate with tamper test | P4 4.6 | #455, #574, #439, #525 |
| Admission-time signature policy for runtime and produced images (sigstore policy controller, OpenShift image policy) | P4 4.3 | new |
| Helm-based Operator and OLM channel; air-gapped bundle; multi-arch images | P5 | #433, #444 |
| Support matrix with recorded end-to-end runs on EKS, GKE, AKS, ROSA and ARO | P5 | #440 |
| Predictive admission (tighten only) and anomaly detection on stop and latency rates | P2 2.2, 2.5 | new |
| Human approval tiers by risk; structured verdicts and test reports for autonomy | P3 3.6, P2 2.6 | new |
| Pluggable, multi-project outer loop | P1 | M3 |

### How progress is measured

Each phase ends with this audit repeated against the same principles:
- every gap closed must cite its evidence;
- every principle must state its maturity;
- autonomy rates are reported with intervals from the evaluation harness, not
  from single batteries.
