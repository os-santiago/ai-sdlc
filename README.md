# ai-sdlc

`ai-sdlc` is the Kubernetes/OpenShift runtime and portable contract for
bounded, evidence-driven AI-assisted software delivery.
See the [innerloop/outerloop architecture](docs/innerloop-outerloop.md) for component
responsibilities, authority boundaries and the remaining E2E graduation gates.

For the current local demonstration, see [Saferus E2E readiness](docs/operations/saferus-e2e-readiness.md): verified preparation and remaining autonomous-delivery gates.

The [browser verifier component](docs/operations/browser-verifier.md) adds real
desktop/mobile conformance tests. Its integration into autonomous deployment
health remains a separate gate.

## Install locally with kind and Helm

On Linux amd64 with Python 3.11+ and Docker or Podman:

```bash
git clone https://github.com/os-santiago/ai-sdlc.git
cd ai-sdlc
./install.sh plan
./install.sh local
./install.sh status
```

If the preflight reports low Linux inotify limits, run
`sudo ./install.sh host --configure-host`, then retry as your normal user.
The explicit adjustment affects this boot only; see the
[host prerequisites](docs/installation.md) for persistent host management.

One entrypoint installs verified Kubernetes clients, creates kind and applies
the versioned Helm chart. Each of the six runtime services has its own
container and Deployment; PostgreSQL uses persistent storage. A short-lived
migrator Job owns schema setup, while services receive separate database
credentials and signing identities. Images come
from [Quay](https://quay.io/repository/os-santiago/ai-sdlc) and are pinned by
digest in Git. Reapply the same command to reconcile desired state. To refresh
pins from a reviewed publication, use the [image upgrade procedure](docs/operations/image-lock-upgrades.md).
`./install.sh upgrade-plan` compares explicit published versions before mutation;
its [preflight report](docs/operations/image-lock-upgrades.md#plan-a-transition-before-changing-an-installation)
does not authorize upgrading an active installation. For an original pre-candidate
runtime, `runtime-upgrade pause` preserves history while stopping producers;
`runtime-upgrade backup` verifies a private copy by restoring it into an isolated
database. The bounded `runtime-upgrade diagnostic` transition applies the
reviewed diagnostic migration and worker image selection, then opens intake last.
For an installation on that diagnostic bridge, `runtime-upgrade tool-budget`
performs the reviewed 16-to-24 tool-step adjustment, including pause, a new
restore-verified backup and reactivation. Provider quotas remain unchanged.
After that update completes, `runtime-upgrade reapprove` can install separately
reviewed [finite approval windows](docs/operations/approval-window.md) for the
same test environment without renewing workflow requests or failed attempts.
After an expired, contained review failure, `runtime-upgrade reviewer-image`
can [replace only the reviewer image](docs/operations/reviewer-image-upgrade.md)
while retaining the candidate, verification and failed-review history.
See the same procedure for scope, prerequisites and recovery limits.

For an existing installer-owned Konflux cluster, use the
[frozen runtime capability](docs/operations/frozen-konflux-runtime.md) and
`examples/saferus-konflux-frozen.local-install.json`. It preserves the platform identity and
keeps Saferus delivery disabled while integration is validated.

An explicit [closed-build capability](docs/operations/build-runtime-install.md)
connects signed merged-source dispatch, relay and the build controller on that
platform. Prepare its [persistent foundation](docs/operations/build-foundation.md)
through `install.sh build-foundation` before selecting the approved bundle. It requires a separately approved current profile and original storage
slots; installation does not authorize release or demonstrate the full E2E.

This first release installs **maintenance mode**, with delivery actions
disabled. It verifies infrastructure installation, not autonomous
issue-to-production delivery. No GitOps controller is installed; reconciliation
is explicit through Helm. See [installation and configuration](docs/installation.md)
for credentials, supported scope, limitations and recovery guidance.

Before installing the remaining delivery capabilities on an owned, frozen
Saferus Konflux environment, `./install.sh prepare --cluster <name> --images
<reviewed-images.lock.json>` verifies an empty current database and pauses all
delivery services through Helm. It preserves PostgreSQL and frozen authority;
ordinary `local` reapplication cannot silently resume it. See
[initial preparation](docs/operations/build-runtime-install.md#initial-empty-runtime-preparation)
for prerequisites and the distinction from upgrading a runtime with history.

For local GitHub client access, run `./install.sh credentials --github-auth gh-cli`
after installation and `gh auth login`. See the
[credential setup](docs/installation.md#optional-local-github-client) for its scope
and explicit refresh command.

The reviewed `shadow` profile adds real GitHub issue polling, signed admission
and durable blocked plans. It does not execute generated code or publish PRs:

```bash
./install.sh local --profile shadow --github-auth gh-cli
```

Only issues carrying `ai-sdlc:shadow`, labeled by the configured local account,
can enter that profile. See [shadow intake](docs/installation.md#shadow-intake)
for scope, verification and returning to maintenance.

The opt-in [candidate profile](docs/operations/candidate-runtime.md) adds signed
issue-to-candidate execution with an isolated SCC executor, a containerized Codex
gateway and terminal crash recovery. It requires reviewed matching images and
stops at an unvalidated candidate; it does not authorize PR publication or release.

The explicit [verification profile](docs/operations/independent-verification.md#install-the-independent-verifier)
adds a separate verifier service and four offline validation Pods. It requires
a matching published verifier image and stores independent behavior evidence;
publication, merge and promotion remain disabled.

With reviewed compatible images, verification can also enable an
[independent read-only SCC review](docs/operations/independent-review-installation.md)
using `"independentReview": true` in the installation JSON. The reviewer has a
separate offline namespace, model conversation and durable evidence journal.
Publication and deployment remain disabled.

The Homedir verification pilot can additionally opt into
[verified PR publication](docs/operations/publication-installation.md) with
`"publication": true`. Its release manager has a separate GitHub credential and
requires signed behavior/review evidence. Publication creates a bounded PR;
merge, release and production promotion remain separate gates.

With compatible images and verified native protection, the separate
[low-risk merge capability](docs/operations/merge-installation.md) can be enabled
with `"merge": true`. This does not enable release or production promotion.

The [Saferus reference adapter](docs/saferus-reference-project.md) provides a
separate, finite token-lifetime UI recipe with repository-bound publication and
merge. Its isolated validation is not an autonomous issue-to-production result.

The [Homedir enterprise pilot](docs/enterprise-pilot.md) uses a separate,
declaratively configured installation and real product validation. Start with
`./install.sh plan --config examples/homedir.local-install.json`; the example
grants shadow intake only. Its roadmap includes independent execution,
Konflux provenance gates and a measured Fullsend comparison.

For a separate local Konflux environment, use the same installer:

```bash
./install.sh supply-chain plan
./install.sh supply-chain local
./install.sh supply-chain status
```

This optional installation needs at least 8 GiB available memory and creates
its own kind cluster. It installs pinned upstream dependencies and a Helm-managed
Konflux operator, with no default users or UI host ports. See
[local Konflux setup](docs/operations/konflux-local.md) for prerequisites,
reapplication, evidence and the remaining build/provenance integration work.

Local installation also verifies NetworkPolicy enforcement with temporary
connectivity probes against the network plugin included in the pinned kind image;
see the [installation boundaries](docs/installation.md#installation-readiness-versus-autonomous-delivery).

## GitHub Actions deprecation

The GitHub Actions AI-SDLC runtime, reusable stages, bootstrap tooling, and its
runtime-specific tests were removed from this branch. The final historical
implementation is preserved at the annotated tag
[`deprecated-github-actions-runtime-2026-11-14`](https://github.com/os-santiago/ai-sdlc/tree/deprecated-github-actions-runtime-2026-11-14).

The Kubernetes/OpenShift runtime is now the sole execution path. See
[DEPRECATION.md](DEPRECATION.md) for the migration boundary.

## What remains

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

## Scope

This repository does not ship a GitHub Actions runtime, a `gh` bootstrap
extension, or a Node-based workflow engine. Consumers integrate with the
Kubernetes/OpenShift control plane using the portable contract and normalized
event schemas under `spec/`.

## License

Apache-2.0

For supply-chain component validation, see the [isolated Homedir/Konflux fixture](docs/operations/konflux-homedir-fixture.md). It records actual builds and signature checks without advancing live delivery.

For a new Saferus demonstration cluster, the [cold Helm registry profile](docs/operations/build-runtime-install.md#cold-installation-with-a-helm-owned-registry) adds native read-only provenance credentials and retained local registry storage. It preserves existing installations and does not activate autonomous delivery by itself.
