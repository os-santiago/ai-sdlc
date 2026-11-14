# Contributing

`ai-sdlc` accepts contributions to its portable contracts and Kubernetes/OpenShift
runtime. GitHub Actions runtime contributions are out of scope; see
[DEPRECATION.md](DEPRECATION.md).

## Contribution path

1. Open or reference a scoped issue.
2. Create a focused branch and keep commits atomic.
3. Keep code, commits, pull requests, and documentation in English.
4. Do not commit credentials, private keys, internal endpoints, or sensitive
   environment output.
5. Explain the observable change, validation, intentional exclusions, and
   follow-up work in the pull request.

## Validate

For runtime changes:

```bash
cd runtime/k8s
go build ./...
go vet ./...
go test ./...
```

For deployment changes, render the relevant Kustomize or Helm surface. For
changes that need Postgres persistence coverage, configure
`AI_SDLC_TEST_DATABASE_URL` as documented in `runtime/k8s/README.md`.
