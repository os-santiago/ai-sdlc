# ai-sdlc

**ai-sdlc is a Kubernetes runtime for autonomous, evidence-driven software delivery.**
It turns a reviewed GitHub issue into a verified, independently reviewed and
merged change, and (for supported projects) into a signed, provenance-checked
deployment. Every stage runs as a separate component under its own authority.
Models are consumed only as a service, and every stop fails closed with
retained evidence.

It is built for platform and security teams who want AI-assisted delivery they
can **audit and bound**: an issue is the only external input, there are no
hidden human steps, and an independent check gates each consequential action.

> **Status: pre-1.0.** The core loop has run end to end on a reference project,
> but onboarding your own repository still requires code. See the
> [capability matrix](docs/capabilities.md) for what is generic today, what is
> project-specific, and what is not yet proven, and the [roadmap](docs/roadmap.md)
> for the path to an adoptable v1 on Kubernetes and OpenShift (developed on kind
> first, then validated on OpenShift).

## How it works

```
issue ──► admission ──► plan ──► generation ──► behaviour ──► independent ──► PR ──► merge
          (reviewable    (work     (model as a    verification   review          (gated)
           criteria)      items)    service)                     (read-only)
                                                                    │
                     production ◄── deployment ◄── provenance ◄── signed build
                     record          + health      verification   (outer loop)
```

- **Inner loop:** admission checks that an issue is reviewable (or proposes a
  revision), planning creates bounded work items, generation runs a coding agent
  against a model service, behaviour verification runs reviewed checks, and an
  independent read-only reviewer approves or requests changes.
- **Outer loop:** publication and a gated merge, then a signed build, provenance
  verification, promotion, deployment with health evidence and a production
  record.
- **Model as a service:** generation and review reach models only through
  gateway adapters (OpenAI-compatible, Anthropic Messages, or an agent CLI
  wrapper), with a startup benchmark that sets per-call timeouts. See
  [model gateway adapters](docs/operations/model-gateway-adapters.md).

`ai-sdlc` is the Kubernetes/OpenShift runtime and portable contract for
bounded, evidence-driven AI-assisted software delivery. See the
[innerloop/outerloop architecture](docs/innerloop-outerloop.md) for component
responsibilities, authority boundaries and the remaining E2E graduation gates.
For the current local demonstration on the reference project (Saferus), see
[Saferus E2E readiness](docs/operations/saferus-e2e-readiness.md): verified
preparation and remaining autonomous-delivery gates.

Independent review can opt into [one bounded repair](docs/operations/review-repair.md),
using the original issue authority and remaining model budget. It is disabled by default.
The [browser verifier component](docs/operations/browser-verifier.md) adds real
desktop/mobile conformance tests. Deployment health requires its bounded evidence
chain as well as HTTP/TLS checks; a component test is not autonomous E2E proof.

## Evidence

On 2026-11-21 a repository that is not a built-in reference went from one
authorized issue to a merged pull request using only a reviewed project
manifest. Its own test suite verified the change offline, and an independent
review approved it ([generic inner loop E2E](docs/evidence/2026-11-21-generic-inner-loop-e2e.md)).

The earlier issue-only run (2026-11-20) went from a single authorized issue
event to a merged pull request without human intervention. Review, publication
and merge were performed by the runtime, and the signed build ran. It then
stopped fail-closed at provenance and at build capacity, exposing two
installation defects that are now fixed or designed. See the
[issue-only E2E record](docs/evidence/2026-11-20-issue-only-e2e.md).

## Quickstart (local, kind)

On Linux amd64 with Python 3.11+ and Docker or Podman:

```bash
git clone https://github.com/os-santiago/ai-sdlc.git
cd ai-sdlc
./install.sh plan
./install.sh local
./install.sh status
```

This installs the runtime in maintenance mode (no delivery). If the preflight
reports low Linux inotify limits, run `sudo ./install.sh host --configure-host`,
then retry as your normal user. The explicit adjustment affects this boot only;
see the [host prerequisites](docs/installation.md) for persistent host
management and other limits.
Enabling delivery, the upgrade transitions and the supply-chain profiles are
described in [runtime upgrades and installation details](docs/operations/runtime-upgrades.md).

## Local development

Run the complete Go build, vet and test suite, including real PostgreSQL paths,
with Python 3 and Docker or Podman:

```bash
python3 tools/validate_runtime.py
```

The command uses pinned Linux/amd64 tool images, creates its own temporary
database, checks that required persistence tests actually passed, and removes
only its own containers. It prints a report path with stage timings, skipped
tests and cleanup results. See [local validation](docs/development/local-validation.md)
for deadlines, prerequisites and troubleshooting.

For an opt-in test of real Codex completions, SCC, gVisor and independent
candidate checks, see the [Homedir component benchmark](docs/security/real-model-homedir-benchmark.md).
It uses disposable fixture authority and does not advance live issues or prove
autonomous delivery.

For a quick check with Go installed, run `go build ./...`, `go vet ./...` and
`go test ./...` from `runtime/k8s`. Without `AI_SDLC_TEST_DATABASE_URL`, persistence
tests skip; that result is not full database validation. Build local images with
`make containers` from `runtime/k8s`. Runtime details are in
[runtime/k8s/README.md](runtime/k8s/README.md).

For the opt-in Saferus outerloop, see [paused deployer installation](docs/operations/deployer-installation.md). The [target preparation command](docs/operations/release-target-preparation.md) binds the original application and pull credential through the same installer. The deployer adds a dedicated container and scoped Kubernetes permissions; [finite initial activation](docs/operations/initial-runtime-activation.md) enables intake only after downstream readiness. Recovery and autonomous E2E validation remain separate gates.

## Repository layout

- `spec/` — portable contracts for admission, risk, trust, policy, labels,
  injection defense, and normalized events.
- `runtime/k8s/` — the canonical Go control plane, deployment manifests, and
  component tests.
- `examples/` — starter `.ai-sdlc.yaml` contracts.
- `.github/workflows/` — repository quality/security validation and component
  image construction/publication; no AI-SDLC orchestration;
  see [quality checks](docs/quality-checks.md) for CI and local commands.
- `.github/ISSUE_TEMPLATE/` and `.github/CODEOWNERS` — repository governance,
  not runtime execution.

## Scope

This repository does not ship a GitHub Actions runtime, a `gh` bootstrap
extension, or a Node-based workflow engine. Consumers integrate with the
Kubernetes/OpenShift control plane using the portable contract and normalized
event schemas under `spec/`.

## GitHub Actions deprecation

The GitHub Actions AI-SDLC runtime, reusable stages, bootstrap tooling, and its
runtime-specific tests were removed from this branch. The final historical
implementation is preserved at the annotated tag
[`deprecated-github-actions-runtime-2026-11-14`](https://github.com/os-santiago/ai-sdlc/tree/deprecated-github-actions-runtime-2026-11-14).

The Kubernetes/OpenShift runtime is now the sole execution path. See
[DEPRECATION.md](DEPRECATION.md) for the migration boundary.

## Contributing, security and license

- Contributions: [CONTRIBUTING.md](CONTRIBUTING.md).
- Security reports: [SECURITY.md](SECURITY.md).
- License: Apache-2.0.
