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

The runtime requires Go and optionally Podman, Kustomize, Helm, and Postgres
depending on the surface being exercised.

```bash
cd runtime/k8s
go build ./...
go vet ./...
go test ./...
```

`go test ./...` runs the pure component tests without external services.
Set `AI_SDLC_TEST_DATABASE_URL` to a fresh disposable database and run
`go test -count=1 -p 1 ./...` to include persistence-path tests. Build local
images with `make containers` from `runtime/k8s` when Podman is available. Full instructions are
in [runtime/k8s/README.md](runtime/k8s/README.md).

## Scope

This repository does not ship a GitHub Actions runtime, a `gh` bootstrap
extension, or a Node-based workflow engine. Consumers integrate with the
Kubernetes/OpenShift control plane using the portable contract and normalized
event schemas under `spec/`.

## License

Apache-2.0
