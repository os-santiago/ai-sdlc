# runtime/k8s — Kubernetes/OpenShift control plane

The canonical Kubernetes/OpenShift control plane for `ai-sdlc`. It consumes
the portable `.ai-sdlc.yaml` contract (`spec/ai-sdlc.schema.json`) and the
`spec/normalized-event.schema.json` intake payload. The removed GitHub
Actions runtime is preserved only by the repository deprecation tag.

## Components

| Component | Stage (ADR-0003) | Package |
|---|---|---|
| `admission-controller` | Admission | `components/admission-controller/internal/admission` |
| `orchestrator` | Plan | `components/orchestrator/internal/planning` |
| `worker` | Bounded edit (engine invocation via `internal/engine`; `StubEngine` by default, `SCCEngine` when `AI_SDLC_ENGINE=scc` — see `internal/engine/scc.go`) | `components/worker/internal/cycle` |
| `release-manager` | Publish/review, Release | `components/release-manager/internal/gates` |
| `relay` | — (infrastructure, not a pipeline stage) | `internal/outbox` |

Each of the first four is a standalone HTTP service
(`internal/platform/service`): one pure, synchronous `Evaluate(...)`
function per component, wrapped by a thin HTTP handler. `relay` is the
5th container: it polls the `outbox` table (`internal/store`,
`internal/outbox`) and delivers unpublished events/commands to the other
four over HTTP (localhost — all five are co-located in one pod, see
`deploy/kustomize/base/deployment.yaml`), closing the "wiring to a real
event bus" gap ADR-0004 left open, without a message broker
(ADR-0005: PostgreSQL + a transactional outbox, matching
`homedir-ai-sdlc`'s own prior ADR-0002).

Set `DATABASE_URL` on any of the first four components to enable
persistence + outbox emission around their existing `Evaluate` call; leave
it unset and they behave exactly as ADR-0004 shipped them (stateless,
dry-run only). `relay` requires `DATABASE_URL` — polling the outbox is its
only job.

## A-Dev policy bundle

When `AI_SDLC_ADEV_BUNDLE_PATH` is set, the SCC worker reads the mounted,
reviewed A-Dev AI-SDLC profile before each invocation and places it in the
trusted policy section of the SCC prompt. An unreadable, empty, or oversized
bundle stops the attempt before checkout or configuration writes. Set
`AI_SDLC_ADEV_BUNDLE_SHA256` to the reviewed file's SHA-256 (`sha256sum`);
the worker verifies integrity and the supported 1.0.0 profile structure,
including fail-closed policy. Mount a pinned copy of
`implementation/profiles/ai-sdlc.yaml` from A-Dev; do not source it from a
work item or target repository.

For supervised local development, `tools/codexcli_openai_wrapper.py` exposes a
localhost-only OpenAI-compatible endpoint backed by `codex exec`. It allows
SCC to use CodexCLI as a completion model. Codex runs read-only in a temporary
directory and proposes structured tool calls; SCC executes those tools in the
target workspace. Both JSON and buffered SSE responses are supported. Set
`CODEX_WRAPPER_TOKEN` to a random value of at least 32 characters and use that
same value as the SCC model API key. The server binds only to `127.0.0.1`.
When SCC runs in a container, use host networking for this supervised local
test; do not expose the endpoint externally. The completion has a 100-second
timeout. Codex uses existing host authentication, but ignores user config.

This integration supplies profile context and verifies its integrity; it does
not implement every A-Dev acceptance or release policy. The isolated HTTP and
tool-mapping tests are not evidence of a complete autonomous release cycle.

## Spec integration

- `internal/contract` loads `.ai-sdlc.yaml` from the path named by
  `AI_SDLC_CONTRACT_PATH` (default: schema-documented defaults). Each
  component derives its policy from it: `admission.PolicyFromContract`,
  `cycle.PolicyFromContract`, `gates.ApplyContractDefaults`.
- `internal/normalizedevent` decodes `spec/normalized-event.schema.json`
  work_item payloads and extracts acceptance criteria per
  `spec/definition-of-ready.md` R2/R3, so admission-controller's input
  shape matches the portable normalized-event contract.

## Build & test

```bash
cd runtime/k8s
go build ./...
go vet ./...
go test ./...
```

The pure domain packages (`admission`, `cycle`, `planning`, `gates`,
`engine`) and most of `internal/outbox` need no external services — those
tests are pure unit tests, no network. `internal/store` and a few
persistence-path tests in `admission`/`outbox` are the deliberate
exception: they talk to a real Postgres and skip cleanly when
`AI_SDLC_TEST_DATABASE_URL` is unset. To run them:

```bash
export AI_SDLC_TEST_DATABASE_URL=postgres://ai_sdlc:ai_sdlc@localhost:5432/ai_sdlc_test
go test ./... -v
```

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
above. `make containers` builds all five images locally with Podman.

### Engine source pin (image builds)

The worker image builds `scc` from `os-santiago/sc-agent-cli` at
image-build time, pinned to an immutable ref via the `SCC_ENGINE_PIN`
build arg (`components/worker/Containerfile`) — a 40-char commit SHA or
semver tag; anything else fails the build closed. The default mirrors
the contract `engine.pin` field, so a plain `make containers` or the OpenShift
`worker` BuildConfig already bakes in the pinned engine. To keep the
image on a contract that declares its own `engine.pin`, pass the
resolved value through:

```bash
podman build -f components/worker/Containerfile \
  --build-arg SCC_ENGINE_PIN=<resolved engine.pin> \
  -t localhost/ai-sdlc-worker:dev .
# or, for all components:
make containers SCC_ENGINE_PIN=<resolved engine.pin>
```

Every surface needs a Postgres reachable via `DATABASE_URL` before the
pipeline does anything beyond respond-and-discard (ADR-0005). The default
path is a [CloudNativePG](https://cloudnative-pg.io/) `Cluster`, rendered
from `deploy/kustomize/base/postgres-cluster.yaml` / Helm's
`postgres.cloudNativePG` values — see
[deploy/kustomize/base/README.md](deploy/kustomize/base/README.md)
"Durable state" for the one-time operator install.

See [deploy/kustomize/base/README.md](deploy/kustomize/base/README.md) for
the GitOps promotion model (immutable image digests, no runtime mutation)
and the security posture (Pod Security `restricted`, default-deny
`NetworkPolicy`, dropped capabilities, read-only root filesystem,
non-root images — satisfies OpenShift's default `restricted-v2` SCC with
no custom SCC object). `worker` is the one component image that is not
distroless (it needs a Node runtime for `scc` and a `git` binary for
`internal/checkout` — see `components/worker/Containerfile`); the other
four stay distroless/static.

## Non-goals

- **Providing a GitHub Actions runtime.** That implementation was removed and
  is preserved only by `deprecated-github-actions-runtime-2026-11-14`.
- **A GitHub webhook adapter.** `admission-controller`'s Route (OpenShift
  overlay) exists, but translating a GitHub webhook payload into
  `spec/normalized-event.schema.json` is separate, reviewable follow-up.
- **A dashboard.** The runtime exposes component and deployment surfaces but
  does not ship a dashboard.

~~Wiring a real event bus or durable store~~ — closed by ADR-0005:
PostgreSQL + a transactional outbox (`internal/store`, `internal/outbox`),
polled and delivered by the `relay` component. This avoids a mandatory
message broker for the initial control plane.
