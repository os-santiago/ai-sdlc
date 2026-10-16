# runtime/k8s — Kubernetes/OpenShift control plane

A cloud-native **alternative** to this repo's GHA-native pipeline, for
adopters who cannot or do not want to run on GitHub Actions (self-hosted
forges, air-gapped clusters, orgs standardizing on an in-cluster platform).
Same spec, different compute: both control planes read the same
`.ai-sdlc.yaml` (`spec/ai-sdlc.schema.json`) and the same
`spec/normalized-event.schema.json` intake payload.

See [ADR-0004](../../docs/architecture/adr/0004-cloud-native-k8s-openshift-runtime.md)
for the full decision record, including provenance (ported from
`os-santiago/homedir-ai-sdlc`'s unreleased `future-go/` prototype) and the
schema gaps this port surfaced.

## Components

| Component | Stage (ADR-0003) | Package |
|---|---|---|
| `admission-controller` | Admission | `components/admission-controller/internal/admission` |
| `orchestrator` | Plan | `components/orchestrator/internal/planning` |
| `worker` | Bounded edit (attempt evaluation only — see Non-goals) | `components/worker/internal/cycle` |
| `release-manager` | Publish/review, Release | `components/release-manager/internal/gates` |

Each is a standalone HTTP service (`internal/platform/service`): one pure,
synchronous `Evaluate(...)` function per component, wrapped by a thin HTTP
handler and a `main.go` that just calls `service.Run`. No component talks to
another directly — they are meant to sit behind an event bus (not wired up
yet; see ADR-0004 "Explicitly out of scope"), the same way future-go left
them.

## Spec integration

- `internal/contract` loads `.ai-sdlc.yaml` from the path named by
  `AI_SDLC_CONTRACT_PATH` (default: schema-documented defaults). Each
  component derives its policy from it: `admission.PolicyFromContract`,
  `cycle.PolicyFromContract`, `gates.ApplyContractDefaults`.
- `internal/normalizedevent` decodes `spec/normalized-event.schema.json`
  work_item payloads and extracts acceptance criteria per
  `spec/definition-of-ready.md` R2/R3, so admission-controller's input
  shape matches the GHA intake stage's output shape.

## Build & test

```bash
cd runtime/k8s
go build ./...
go vet ./...
go test ./...
```

No external services required — every test is a pure unit test (see
`go test ./... -v`: 25 tests, 0 skips, no network).

## Deploy

Three equivalent surfaces, pick one:

```bash
# Generic Kubernetes (Kustomize)
kustomize build deploy/kustomize/overlays/development
kustomize build deploy/kustomize/overlays/production

# OpenShift (Kustomize) — adds Route + ImageStream/BuildConfig, swaps
# image refs to the internal registry
kustomize build deploy/kustomize/overlays/openshift

# Helm (either platform; OpenShift Route is capability-gated)
helm lint deploy/helm/ai-sdlc-k8s-runtime
helm template deploy/helm/ai-sdlc-k8s-runtime
helm template deploy/helm/ai-sdlc-k8s-runtime \
  --set openshift.route.enabled=true --api-versions route.openshift.io/v1
```

`make render-k8s`, `make render-openshift` and `make helm-lint` wrap the
above. `make containers` builds all four images locally with Podman.

See [deploy/kustomize/base/README.md](deploy/kustomize/base/README.md) for
the GitOps promotion model (immutable image digests, no runtime mutation)
and the security posture (Pod Security `restricted`, default-deny
`NetworkPolicy`, dropped capabilities, read-only root filesystem,
distroless non-root images — satisfies OpenShift's default `restricted-v2`
SCC with no custom SCC object).

## Non-goals

- **Reimplementing engine invocation.** `worker` evaluates bounded
  attempts; it does not call `sc-agent-cli` itself. The headless contract
  (`--prompt-file --audit-log --summary-file --no-commit`, see root
  README and `.ai-sdlc.yaml`'s `engine` block) is one integration point
  shared with the GHA runtime, not duplicated here.
- **Replacing the GHA-native runtime.** GHA stays the default,
  zero-infrastructure path for any repo that can use it (root README
  "Adopt this runtime"). This directory is an opt-in alternative.
- **Wiring a real event bus or durable store.** Components are
  independently buildable/testable/deployable today; making this an
  actually-running pipeline is tracked as follow-up work (ADR-0004).
- **A dashboard.** ADR-0001 already decided GitHub-native observability
  suffices for a single consumer.
