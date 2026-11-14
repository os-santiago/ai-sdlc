# ai-sdlc agent guidance

`ai-sdlc` is a Kubernetes/OpenShift runtime with portable contracts under
`spec/`. GitHub Actions runtime code is removed; historical consumers are
preserved only by the `deprecated-github-actions-runtime-2026-11-14` tag.

## Scope

- `runtime/k8s/` is the only executable runtime in this repository.
- `spec/` defines portable policy and event contracts.
- Do not add GitHub Actions workflows, composite actions, or workflow-specific
  bootstrap code.
- Keep credentials out of source control and deployment manifests.

## Working rules

- Use a focused branch and one reviewable pull request per change.
- Preserve Kubernetes/OpenShift security defaults: non-root containers,
  read-only filesystems where supported, least privilege, and explicit network
  policy.
- Keep component behavior bounded by timeouts, retry caps, and explicit
  escalation paths.
- Validate the changed Go and deployment surface before requesting review.

## Validation

```bash
cd runtime/k8s
go build ./...
go vet ./...
go test ./...
```

Use Kustomize or Helm rendering checks for deployment-manifest changes. Run
container builds only when the change affects an image or build contract.
