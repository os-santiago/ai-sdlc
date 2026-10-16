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
schema gaps this port surfaced, and
[ADR-0005](../../docs/architecture/adr/0005-runtime-k8s-event-bus-and-engine-invocation.md)
for the durable-state/event-bus/engine-invocation wiring that turns this
from a buildable scaffold into an actually-running pipeline.

## Components

| Component | Stage (ADR-0003) | Package |
|---|---|---|
| `admission-controller` | Admission | `components/admission-controller/internal/admission` |
| `orchestrator` | Plan | `components/orchestrator/internal/planning` |
| `worker` | Bounded edit (engine invocation via `internal/engine` — stub only, see Non-goals) | `components/worker/internal/cycle` |
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
distroless non-root images — satisfies OpenShift's default `restricted-v2`
SCC with no custom SCC object).

## Non-goals

- **A real engine subprocess.** `worker` runs the full bounded-edit loop
  (`internal/engine.Engine` interface, `/v1/attempts/dispatch`) but the
  only shipped `Engine` is `StubEngine` — deterministic, no subprocess, no
  network. Shelling out to `sc-agent-cli` with the headless contract
  (`--prompt-file --audit-log --summary-file --no-commit`, see root
  README and `.ai-sdlc.yaml`'s `engine` block) is tracked follow-up
  (ADR-0005 "Explicitly still out of scope") — it needs a target repo,
  model provider and credentials this environment does not have.
- **Replacing the GHA-native runtime.** GHA stays the default,
  zero-infrastructure path for any repo that can use it (root README
  "Adopt this runtime"). This directory is an opt-in alternative.
- **A GitHub webhook adapter.** `admission-controller`'s Route (OpenShift
  overlay) exists, but translating a GitHub webhook payload into
  `spec/normalized-event.schema.json` is separate, reviewable follow-up —
  mirroring the GHA intake stage's existing logic (ADR-0005).
- **A dashboard.** ADR-0001 already decided GitHub-native observability
  suffices for a single consumer.

~~Wiring a real event bus or durable store~~ — closed by ADR-0005:
PostgreSQL + a transactional outbox (`internal/store`, `internal/outbox`),
polled and delivered by the new `relay` component. See ADR-0005 for why
this isn't a message broker (NATS/Kafka) and why CloudNativePG.
