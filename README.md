# ai-sdlc

`ai-sdlc` is the Kubernetes/OpenShift runtime and portable contract for
bounded, evidence-driven AI-assisted software delivery.

## GitHub Actions deprecation

The GitHub Actions runtime, reusable workflows, bootstrap tooling, and its
Node-based test surface were removed from this branch. The final supported
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
Set `AI_SDLC_TEST_DATABASE_URL` to include persistence-path tests. Build local
images with `make -C runtime/k8s containers` when Podman is available. Full instructions are
in [runtime/k8s/README.md](runtime/k8s/README.md).

## Scope

This repository does not ship a GitHub Actions runtime, a `gh` bootstrap
extension, or a Node-based workflow engine. Consumers integrate with the
Kubernetes/OpenShift control plane using the portable contract and normalized
event schemas under `spec/`.

## License

Apache-2.0
