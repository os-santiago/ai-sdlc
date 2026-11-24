# AI-SDLC strategic principles

These five principles are the architectural commitments of AI-SDLC. Every
component, change and roadmap item is judged against them. A design that
conflicts with a principle needs an explicit, reviewed exception.

The current state against each principle is measured in the
[principles audit](principles-audit-2026-11-24.md), together with the
development plan that closes the gaps.

## P1. Independent, aligned components from issue to release

Each stage of the flow, from processing an issue to releasing a deployable
artifact, is owned by exactly one component with one responsibility:

- intake, admission, planning, generation, verification, independent review,
  repair, publication, merge, build, provenance, promotion, deployment and
  completion.

Components hand work to each other only through explicit, versioned, signed
contracts. A component never performs another stage's decision, and no
component authorizes its own output. Independence is real, not nominal:

- a reviewer does not share an identity with what it reviews;
- a merge is never self-granted.

**Evidence of compliance:** a stage-ownership table generated from the
authority policy and the components' service metadata, checked in CI against
the code.

## P2. Generative and predictive AI, each where it fits best

Generative AI does the work that needs language and code understanding:
- implementation;
- independent review;
- bounded repair.

It always runs behind deterministic gates that decide acceptance:
- budgets;
- bound evidence;
- repository tests;
- strict verdicts.

Predictive AI estimates what the deterministic flow would otherwise guess:
- admission risk and effort;
- budgets and timeouts;
- whether a retry or repair is worth attempting;
- flaky tests;
- anomalies in stop rates and latency.

A predictive signal may only tighten a deterministic decision or advise a
human. It never relaxes a gate.

Model capability is measured, not assumed. A model or adapter change passes
a versioned evaluation before it is used.

**Evidence of compliance:**
- an evaluation report per model and adapter;
- predictions recorded next to outcomes;
- the A-Dev profile names generative, predictive and deterministic roles.

## P3. Embedded automation and policy enforce the expected behaviour

Expected behaviour is enforced by the process itself, not by convention:
- signed, revisioned authority;
- per-component database roles with forced row-level security;
- fenced claims and leases;
- idempotent delivery;
- fail-closed stops with bounded diagnostics;
- sandboxed workloads with exact shape checks;
- quality gates on every change.

Policies are declarative and reviewable as code wherever the platform allows.

Every candidate change meets a standard quality and security baseline (tests,
static analysis, dependency and secret scanning) before it can merge. The
system is resilient as a distributed system: it recovers from crashes and is
observable through metrics and traces. It runs without a single point of
failure for its state, and its data can be backed up and restored.

**Evidence of compliance:**
- policy manifests and their tests;
- the candidate baseline in the merge gate;
- metrics, traces and alerts;
- high-availability defaults;
- a tested restore.

## P4. A trusted security supply chain

Every artifact AI-SDLC runs or produces is traceable and verifiable:

- **Its own images and installer:** built from pinned inputs, signed keyless
  with a transparency log, with signed SLSA provenance, an attested SBOM and a
  vulnerability gate. They are verified by signature before installation and
  at cluster admission.
- **The artifacts it builds for target projects:** built in isolated builds,
  signed, with attested provenance, verified against a declared policy
  (Enterprise Contract / Conforma) before promotion.

Verification produces evidence. It never grants authority by itself.

**Evidence of compliance:**
- a SLSA build level stated and met per artifact class;
- signature verification at install and admission;
- a promotion that refuses an unsigned or tampered artifact in an
  end-to-end test.

## P5. Cloud native portability with standard installation and upgrade

AI-SDLC runs on conformant Kubernetes and OpenShift, on premises and in any
cloud, without depending on a local development topology. Platform
differences are confined to declared capabilities:
- the sandbox runtime class;
- storage;
- DNS;
- ingress;
- the image signature policy.

Installation and upgrade use standard, supportable mechanisms:
- a published, self-contained Helm chart;
- an Operator/OLM path once the chart is complete;
- versioned, reversible upgrades;
- an uninstall that preserves data;
- scheduled backup and restore;
- air-gapped support;
- a read-only doctor.

**Evidence of compliance:**
- a support matrix, with an end-to-end run recorded per target platform;
- published, signed releases;
- tested upgrade and rollback paths.
