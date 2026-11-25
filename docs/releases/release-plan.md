# Release plan: Alpha → Beta → Release Candidate → General Availability

This plan orders the work from the
[principles audit](../architecture/principles-audit-2026-11-24.md) by its
impact on a **functional and stable** AI-SDLC. It also sets measurable exit
criteria for each release stage. A stage is reached when its criteria are met
with recorded evidence, not by calendar.

## Where we are: late Alpha

| Area | State | Evidence |
|---|---|---|
| Release | `v0.1.0-alpha.1` pre-release | [notes](v0.1.0-alpha.1.md) |
| Inner loop (issue label → merged pull request) | Autonomous end to end. 70–90% of issues delivered within two attempts in batteries 8, 9, 11 and 13 (fresh installations), on one sample Go repository with an agent-CLI model service. Batteries 10 and 12 degraded from an intake freshness decay, fixed in #753. Every stop names its cause. | [batteries 8–11](../evidence/2026-11-23-evaluation-batteries-8-11.md), [12–13](../evidence/2026-11-23-evaluation-batteries-12-13.md) |
| Remaining failure causes | On the local model: generated candidates whose own tests fail, often because the model rewrote a large existing file with a whole-file write (a gateway guard now stops that and retries once, #797), the generator running out of steps, and calls over the ceiling. Transport stops after one failed API read on a loaded cluster are retried within bounds (#799, #800). The forge's anonymous quota was exhausted by repeated source reads in the verifier; it now shares one source cache (#802), and an optional read-only credential removes the anonymous limit (#796). | [batteries 19–24](../evidence/2026-11-25-local-model-batteries-19-24.md); batteries 27–28 (evidence to follow) |
| Local model | Generation, verification and independent review on a local model behind the gateway: hunk-count repair, degenerate-output guard, long-call keepalive, review tool narrowing, step-budget guidance, and a guard that refuses whole rewrites of existing files and retries once. Battery 24 on the local model: 3/10 delivered within two attempts, 30% [10.8%, 60.3%]. On a fresh corpus before the rewrite guard, battery 27 delivered 0 of 6 issues. | #777, #779, #781, #782, #784, #797 |
| Outer loop (build → release → deploy) | Project-specific: build, provenance and deploy work only for the reference project (M3) | [capabilities](../capabilities.md) |
| Platforms | Local kind supported. OpenShift Local delivered once without a kernel sandbox. | [OpenShift](../operations/openshift.md) |
| Principle maturity | P1 3, P2 2, P3 3, P4 2, P5 2 | [audit](../architecture/principles-audit-2026-11-24.md) |

## Progress

Updated 2026-11-25. The [iteration roadmap](iteration-roadmap.md) projects the next steps.

| Item | State | Pull requests |
|---|---|---|
| B1 | Stages 1–4 merged: the generator copies the manifest toolchain from the pinned verification image into its sandbox and runs the test command before export. Observed under gVisor on kind ([batteries 19–24](../evidence/2026-11-25-local-model-batteries-19-24.md)). #745 stays open for the remaining stages. | #763, #773 |
| B2 | Done: staggered gateway benchmarks; generation budget (600–900 s, 24–32 steps) and per-answer token bound (4096–8192) configurable at install, defaults unchanged; more than 24 steps or 4096 tokens only with `--model-upstream` | #762, #770, #778 |
| B3 | Done in code: the sandbox `RuntimeClass` is a value checked in preflight, and autonomous work stops without it. Not yet validated on a live cluster with a non-default class (Kata). Follow-ups in #771. | #769 |
| B5 | Mostly done: govulncheck gate, base images pinned by digest, keyless signing of every main image digest with an SBOM attestation, and signature verification before an image lock is written and in the upgrade tools. The first signed main publication was verified by the resolver ([batteries 19–24](../evidence/2026-11-25-local-model-batteries-19-24.md), finding 7). Remaining: image scanning, attestation verification, verification when installing from a committed lock and at cluster admission. | #764, #783 |
| Local model | Generation, verification and independent review on a local model behind the gateway: hunk-count repair, degenerate-output guard, long-call keepalive, review tool narrowing and step-budget guidance. Battery 24 on the local model: 3/10 delivered within two attempts, 30% [10.8%, 60.3%]. | #777, #779, #781, #782, #784 |
| B8 | First part done: an always-on, offline secret scan of the lines a candidate adds (seven high-confidence rules, refused before any pod or review, only the rule ID and path class logged, version bound into the signed recipe revision that publication and merge check), and optional manifest-declared static analysis run on the baseline and candidate in the pinned toolchain image, where only new failures refuse. Defaults unchanged for manifests that do not opt in. Remaining: dependency scanning (pinned vulnerability database image, designed), a managed SAST rule pack and structured findings ([design](../architecture/candidate-quality-baseline.md)) | #795 |
| B10 | Done in code. Metrics: bounded Prometheus metrics for queue depth, safe stops by diagnostic stage, model gateway calls (count and duration) and delivery attempts, served on an opt-in metrics port with a NetworkPolicy and an optional PodMonitor ([metrics](../operations/metrics.md)). Backup: an opt-in CronJob runs `pg_dump --format=custom` as the bootstrap owner (Secret reference only) to a volume under a volume lock, checks each archive with a full read, writes it atomically with a checksum, keeps the newest N files and runs restricted with an egress-only NetworkPolicy; restore is documented and runs from the same CronJob ([backup and restore](../operations/backup-and-restore.md)). The script was run against the bundled PostgreSQL image outside a cluster. Not yet observed on a live cluster: a metrics scrape, a scheduled backup and a restore. | #794, #798 |
| B9 | Done: the merged pull request closes its issue (`Closes #N`). Truthful component docs and the dead-contract decision remain. | #715 |
| Source reads | An optional read-only, single-repository credential for verification and review, never falling back to anonymous reads; the verifier shares one source cache that backs off until the forge's reset. | #796, #802 |
| Others | Not started (B4, B6 rest, B6b) | |

## Prioritization

Items are ranked by impact on a functional, stable product.
- **Functional:** issues get delivered.
- **Safe:** autonomy cannot be abused.
- **Installable:** someone else can run it.
- **Stable:** it keeps working over time and across upgrades.

The outer loop stays on the existing v1.1 track (M3), together with
produced-artifact trust (audit gap 4.6: #455, #574, #439, #525). GA covers
the inner loop with production-grade installation and operation.

### Beta: functional, safe and installable by early adopters

Ordered by impact.

| # | Item | Why it matters | Audit gaps / issues |
|---|---|---|---|
| B1 | The generator runs the manifest's tests in its sandbox | The top cause of refused candidates | #745 |
| B2 | Configurable generation budget; staggered gateway benchmarks | Budget, timeout and contention stops, worse on local models | #750, #758 |
| B3 | Sandbox: consume the platform `RuntimeClass` as a value, verified in preflight; refuse autonomy without it | Safe execution of generated code on any cluster | 5.1 |
| B4 | Independent review identity; merge needs a co-signature | Autonomy cannot approve or merge its own work | 1.1, 1.2 |
| B5 | Signed runtime images verified by the installer; pinned bases; vulnerability gate (govulncheck, image scan) | Adopters can trust what they install | 4.1, 4.4, 4.5, 3.7 (part) |
| B6 | Self-contained, published Helm chart; generic Kubernetes path; external Postgres with TLS | Installable without the repository's local tooling | 5.2, 5.3, 5.5; #430, #431, #432, #434 |
| B6b | Model upstream by FQDN over HTTPS with a credential, so a hosted model can be configured when an API key is available | A second model path that adopters can choose | 5.7 (part) |
| B7 | Evaluation harness v1 (several repositories, pass@k with intervals, reviewer confusion matrix) | Rates that predict adoption; the gate for model changes | 2.1 |
| B8 | Candidate quality and security baseline in the merge gate | Merged changes meet a minimum standard | 3.3 |
| B9 | Close the issue from the merged pull request; truthful component docs; decide the dead contracts | A complete, honestly documented inner loop | #715, 1.4, 1.6, 4.7 |
| B10 | Minimal metrics (queue depth, stops by class, model calls); scheduled database backup | Operators can see and recover the system | 3.1, 3.2 (part) |

### Release Candidate: stable, operable and portable

| # | Item | Audit gaps / issues |
|---|---|---|
| R1 | Operator `v1beta1` (`AISDLCInstallation`) rendering the chart; OLM `candidate` channel | 5.2, 5.4 |
| R2 | Generic upgrade and rollback; uninstall that keeps data; tested backup and restore; doctor | 5.4, 5.6, #435, #441, #442, #443 |
| R3 | Database high availability by default; leader election with standbys and PDBs | 3.1, 3.5 |
| R4 | Traces and alerts with correlation IDs | 3.2, 3.7 |
| R5 | Admission policies as chart templates; required image signature policy for autonomous profiles; signed SLSA provenance verified at install | 3.4, 4.2, 4.3 |
| R6 | Split the release manager into publisher, merge gate and release coordinator | 1.3 |
| R7 | Predictive in shadow: budgets, timeouts and retry value from recorded calls and outcomes | 2.3, 2.4 |
| R8 | Remove legacy handlers; split the toolkit image; predictive role in the A-Dev profile | 1.5, 1.7, 2.7 |
| R9 | Relax the remaining local-topology assumptions (Ingress or Gateway API, configurable namespaces) | 5.7 |
| R10 | Threat model for the runtime and its autonomy boundaries, reviewed | P3, P4 |

### General Availability: supported

| # | Item | Audit gaps / issues |
|---|---|---|
| G1 | Operator `stable` channel; signed bundle on OperatorHub; certification on OpenShift | 5.4, 5.6 |
| G2 | Support matrix with recorded end-to-end runs: kind, OpenShift (Kata), and at least two managed clouds | #440, #444 |
| G3 | Air-gapped bundle; multi-arch images | 5.6, 5.7 |
| G4 | Human approval tiers by risk; structured verdicts and test reports for autonomy | 3.6, 2.6 |
| G5 | Predictive admission (tighten only) and anomaly detection | 2.2, 2.5 |

## Exit criteria

### Alpha → Beta

**Functional:**
- The evaluation harness reports **≥ 80%** of issues delivered within two
  attempts, with a 95% interval lower bound of **≥ 70%**.
- The rate is measured **per cell**, each cell being one repository and one
  model path. Each cell needs at least 60 issues. A pooled rate does not
  count.
- It holds on **two repositories in different languages**, with **two model
  paths**: a model served on local hardware (OpenAI-compatible server), and a
  second path, either the agent-CLI service or a hosted API when an API key
  is available (B6b).

**Safe:**
- Generated code always runs in a kernel sandbox, and preflight refuses
  autonomy without one.
- Review and merge are separate authorities.
- Runtime images are signed, and verified at install.
- No known open security or data-loss defect.

**Installable:** the published chart installs on kind and on OpenShift with
Kata, following the documentation, without the repository's local tooling.

**Environment dependencies:**
- Kata needs a full OpenShift cluster; OpenShift Local cannot install it.
- A hosted model path needs an API key.

If either is unavailable at the gate, the Beta notes record it as an open
condition. It is not silently waived.

**Truthful:** the docs match the code (component table checked in CI). The
release notes state every known limit.

### Beta → Release Candidate

**Functional:**
- **≥ 85%** of issues delivered within two attempts, with a lower bound of
  **≥ 75%**, on three repositories.
- No open release-blocking defect.

**Stable:**
- A **7-day soak** under continuous delivery with no manual intervention.
  Rates stay within the Beta interval, and median model-call latency stays
  within 20% of the first day.
- Upgrade from the previous beta, and rollback, both tested.

**Operable:** the operator installs and upgrades through OLM. Metrics,
traces and alerts are in place, and backup and restore are tested.

**Portable:** end-to-end runs recorded on OpenShift and on at least one
managed cloud.

**Secure:**
- Signed SLSA provenance for runtime images is verified at install, and the
  admission signature policy is enforced.
- A threat model has been reviewed.

**Frozen scope:** only fixes after RC1.

### Release Candidate → General Availability

**Stable:** **4 weeks** on the RC with no release-blocking defect, and
rates held within the RC interval.

**Supported:**
- The support matrix includes at least two managed clouds and OpenShift.
- A documented support and upgrade policy (N-1 upgrades).
- The operator `stable` channel.

**Documented:** installation, operation, upgrade, troubleshooting and
security guides, verified by a person following them on a clean cluster.

## Versioning

| Stage | Versions | Compatibility |
|---|---|---|
| Alpha | `v0.1.0-alpha.N` | Breaking changes allowed |
| Beta | `v0.2.0-beta.N` | Configuration and data upgrades supported from the previous beta |
| Release Candidate | `v1.0.0-rc.N` | Feature-frozen; upgrades from the last beta |
| General Availability | `v1.0.0` | Semantic versioning; N-1 upgrades supported |

Each stage transition is published as a release whose notes cite the
evidence for every exit criterion. The release is approved by the maintainer
before it is tagged.
