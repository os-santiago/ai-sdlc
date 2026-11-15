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

## Contributor provenance

Contributors must sign off commits with `git commit -s` under the Developer
Certificate of Origin. Automated project-agent commits retain the existing
exemption. A maintainer must vouch for a first-time contributor through a PR
comment or approval. Runtime retirement does not remove these requirements.

## Validation commands

For runtime changes, run the complete suite with an isolated database:

```bash
python3 tools/validate_runtime.py
```

This requires Python 3, Git, and Docker or Podman; Go and PostgreSQL run in
pinned containers. Include the report's outcome and any skipped optional
integration fixtures in the PR. See [local validation](docs/development/local-validation.md).

For deployment changes, also render the relevant Kustomize or Helm surface and
run its integration fixture. Local Go/database success does not validate
Kubernetes containment, model-provider behavior, or an autonomous delivery cycle.
