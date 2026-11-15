# ai-sdlc

`ai-sdlc` is the Kubernetes/OpenShift runtime and portable contract for
bounded, evidence-driven AI-assisted software delivery.

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
digest in Git. Reapply the same command to reconcile desired state.

This first release installs **maintenance mode**, with delivery actions
disabled. It verifies infrastructure installation, not autonomous
issue-to-production delivery. No GitOps controller is installed; reconciliation
is explicit through Helm. See [installation and configuration](docs/installation.md)
for credentials, supported scope, limitations and recovery guidance.

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

The [Homedir enterprise pilot](docs/enterprise-pilot.md) uses a separate,
declaratively configured installation and real product validation. Start with
`./install.sh plan --config examples/homedir.local-install.json`; the example
grants shadow intake only. Its roadmap includes independent execution,
Konflux provenance gates and a measured Fullsend comparison.

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

For a quick check with Go installed, run `go build ./...`, `go vet ./...` and
`go test ./...` from `runtime/k8s`. Without `AI_SDLC_TEST_DATABASE_URL`, persistence
tests skip; that result is not full database validation. Build local images with
`make containers` from `runtime/k8s`. Runtime details are in
[runtime/k8s/README.md](runtime/k8s/README.md).

## Scope

This repository does not ship a GitHub Actions runtime, a `gh` bootstrap
extension, or a Node-based workflow engine. Consumers integrate with the
Kubernetes/OpenShift control plane using the portable contract and normalized
event schemas under `spec/`.

## License

Apache-2.0
