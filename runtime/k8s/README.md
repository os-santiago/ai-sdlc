# runtime/k8s — Kubernetes/OpenShift control plane

The canonical Kubernetes/OpenShift control plane for `ai-sdlc`. It consumes
the portable `.ai-sdlc.yaml` contract (`spec/ai-sdlc.schema.json`) and the
`spec/normalized-event.schema.json` intake payload. The removed GitHub
Actions runtime is preserved only by the repository deprecation tag.

For installation, start at [the Helm/kind guide](../../docs/installation.md).
The canonical installer is `./install.sh` at the repository root. Helm chart
0.4 deploys one container/Deployment per component and uses internal Services.
The original Kustomize topology remains a legacy manual deployment surface;
it is not the new installer's desired state.

The installer requires positive/negative network conformance probes to pass
against the plugin included in the pinned kind image. Existing remote clusters
must provide their own NetworkPolicy enforcement.

## Components

| Component | Stage (ADR-0003) | Package |
|---|---|---|
| `intake` | Bounded GitHub issue polling | `internal/intake` |
| `admission-controller` | Admission | `components/admission-controller/internal/admission` |
| `orchestrator` | Plan | `components/orchestrator/internal/planning` |
| `worker` | Bounded implementation coordination; execution disabled during integration | `components/worker/internal/cycle` |
| `release-manager` | Publish/review, Release | `components/release-manager/internal/gates` |
| `relay` | Signed durable transport; admission and planning only in shadow | `internal/delivery` |
| `migrator` | Installation-only schema and role provisioning | `components/migrator/cmd` |
| `model-mailbox` | Hidden offline model-data transport; no live workload integration | `internal/modelmailbox` |

The six service binaries load mounted component identities, connect with
separate database LOGIN credentials and check migrations before starting.
The short-lived `migrator` Job alone receives the bootstrap database credential
and reconciles schema and role grants. Readiness rechecks those dependencies;
there is no stateless or stub-success fallback for a missing installation.

Only observation endpoints are registered in maintenance. Unsigned delivery
routes and synchronous engine execution are no longer installed by service
entry points. Pure evaluators and legacy handlers remain isolated test assets;
The opt-in shadow profile registers only signed admission and planning routes
through `internal/delivery`. It polls real issues and creates blocked plans;
it never invokes the model or publishes artifacts. Generated execution must
use isolated Jobs in a later integration stage.

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
localhost-only OpenAI-compatible endpoint backed by pinned `codex-cli 0.169.0`
and `gpt-6.1-sol`. Its reviewed completion policy advertises no executable tools;
Codex proposes structured calls that SCC validates and executes in its workspace.
Both JSON and buffered SSE responses are supported. Set `CODEX_WRAPPER_TOKEN`
to a random value of at least 32 characters and use that value as the SCC model
API key. The server binds only to `127.0.0.1`. When SCC runs in a container,
use host networking only for this supervised local test.

Each completion has a 24-second subprocess deadline, a 128 KiB accepted output
limit and a 3-second absolute HTTP request-read deadline. The fixed backend and
policy digests are verified before listening. Existing host authentication is
used with a restricted child environment. User configuration is ignored, but
system and managed host configuration remain trusted dependencies. Read the
[completion adapter boundary](../../docs/security/codex-completion-adapter.md)
for setup, conformance checks, limits and the host trust assumptions. This host
adapter is not the containerized production model gateway.

This integration supplies profile context and verifies its integrity; it does
not implement every A-Dev acceptance or release policy. The isolated HTTP and
tool-mapping tests are not evidence of a complete autonomous release cycle.

## Local delivery validation boundary

The earlier direct-endpoint probe is retired. It manually supplied admissions
and release evidence, so it could not prove an issue-to-production autonomous
cycle. Its CLI now exits before building images, starting processes or writing
workflow state. The [historical implementation](https://github.com/os-santiago/ai-sdlc/tree/228a7128f69f183e6518804e9a0ac6fa4bc81ff4/runtime/k8s/tools)
remains available for dated evidence; its deterministic helper tests are retained.

Use `./install.sh local` and `tools/verify_local_install.py` for installation
and reapplication checks. These are infrastructure checks, not workflow intake.
A functional E2E must enter through a GitHub issue and let authenticated runtime
components own generation, verification, PR, merge, release and production-test
verification. Only issue intake, admission and blocked planning are enabled by
the opt-in shadow profile in this stage.

Historical SCC probes on 2026-11-14 produced both an exact documentation change
and budget exhaustion. Malformed/no-op patches could be reported as applied by
SCC; independent Git checks rejected false success. Those observations remain
an open convergence issue (#419), not evidence of a reliable release pipeline.

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

From the repository root, run `python3 tools/validate_runtime.py` for a complete
build, vet and serial test run using disposable PostgreSQL. The
[validation guide](../../docs/development/local-validation.md) explains logs,
cleanup and optional integration fixtures.

With Go installed, fast local checks remain available:

```bash
cd runtime/k8s
go build ./...
go vet ./...
go test ./...
```

Persistence-path tests skip when `AI_SDLC_TEST_DATABASE_URL` is unset. Manual
full-suite runs need a fresh PostgreSQL instance with permission to create the
isolated component roles, the complete repository fixtures, and Git. Set that
variable only to a disposable test database, then use
`go test -count=1 -p 1 ./...`; multiple packages must not reconcile the same
fixture roles concurrently. The single validation command supplies these
prerequisites automatically and does not use a host database port.

## Signed delivery integration boundary

`internal/delivery` is the hidden replacement boundary for unsigned delivery;
live delivery remains disabled while isolated executors and release gates are integrated.
It implements:

- Exact-byte, producer-signed records in `authorized_outbox`. Producers can
  append their own facts; the relay can update transport status only.
- A transactional `authorized_inbox` and expiring `authorized_nonces` table.
  Concurrent/restarted receivers return the committed response for the same
  receipt; conflicting content under an existing event ID is rejected.
- Receiver-signed acknowledgements. Lost responses can be retried without
  reapplying state, while a plain HTTP 200 cannot complete delivery.
- Attempt/lease reservation committed before HTTP, a ten-second HTTP limit,
  exponential retry spacing and exhaustion after eight reservations, including
  crashes. Expired leases allow recovery; exhausted facts remain stored.

`store.ProvisionRoles` is reserved for the dedicated migration Job in
an installation-owned database. It requires eight distinct component credentials
from external Secrets and refuses to adopt unregistered PostgreSQL roles.
It reconciles table and column grants, revokes PUBLIC table access and schema
creation, rejects role membership and removes privileged role attributes.
Operational transactions reject bootstrap credentials and elevated identities.
Helm mounts only each service's own database Secret and signing identity. The
migrator Job receives the original bootstrap Secret and the separate credential
map. Services wait for its schema and role provisioning before becoming ready.

Effects passed to `Inbox.Accept` must validate payload semantics, repository
ownership and stage gates, then write state/outgoing commands using its SQL
transaction. They must not run a model, call a forge or deploy synchronously.
Those operations require durable command leases and their own idempotency keys.
`Inbox.Handler` resolves policy from trusted configuration on every request.
Policy and signing identities must never come from a generated workspace.

Migration `0002_authorized_delivery.sql` only adds new tables/policies. It does
not promote legacy unsigned rows into signed facts or activate delivery.
Rollback can retain these unused tables while reverting component images;
do not delete delivery evidence as an automatic rollback step.

Validate with `go build ./...`, `go vet ./...` and `go test -count=1 -p 1 ./...` using a
disposable PostgreSQL 17 database via `AI_SDLC_TEST_DATABASE_URL`. The integration
tests provision actual restricted LOGIN roles and use HTTP receivers, rather
than treating superuser-only SQL tests as proof of isolation. See
[component authority](../../spec/component-authority.md) for the trust contract.

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
above. `make containers` builds all thirteen project images locally with Podman. The
`model-mailbox` image is a hidden prerequisite, tested in disposable Pods; the
installer does not activate it. See the [transport boundary](../../docs/security/offline-model-mailbox.md).

The [browser verifier](../../docs/operations/browser-verifier.md) is a separate
Firefox image with a fixed Saferus recipe and isolated gVisor conformance tests.
Its deployer-owned integration remains pending; component tests do not establish
autonomous deployment health.

### Engine source pin (image builds)

The worker image builds `scc` from `os-santiago/sc-agent-cli` at
image-build time, pinned to an immutable ref via the `SCC_ENGINE_PIN`
build arg (`components/worker/Containerfile`) — a 40-char commit SHA or
semver tag; anything else fails the build closed. The reviewed default is
`b86701a80a82a8ae99655b727ff148d499f970dd`, including bounded diff application,
MCP input-pipe failure isolation, patched development tooling, bounded
reviewer verdict finalization after the inspection-tool limit, and acceptance
of a final summary after completed work instead of a self-heal livelock.
The source revision is baked into the worker; changing a contract field cannot
modify a published image. For a contract declaring a different `engine.pin`,
pass its resolved value explicitly when building:

```bash
podman build -f components/worker/Containerfile \
  --build-arg SCC_ENGINE_PIN=<resolved engine.pin> \
  -t localhost/ai-sdlc-worker:dev .
# or, for all components:
make containers SCC_ENGINE_PIN=<resolved engine.pin>
```

Every service requires its dedicated `DATABASE_URL`, signing Secret and
trusted authority policy before becoming ready. The default
Helm local profile installs a PostgreSQL StatefulSet; the generic default
uses a [CloudNativePG](https://cloudnative-pg.io/) `Cluster`, rendered
from `deploy/kustomize/base/postgres-cluster.yaml` / Helm's
`postgres.cloudNativePG` values — see
[deploy/kustomize/base/README.md](deploy/kustomize/base/README.md)
"Durable state" for the one-time operator install.

See [deploy/kustomize/base/README.md](deploy/kustomize/base/README.md) for
the GitOps promotion model (immutable image digests, no runtime mutation)
and the security posture (Pod Security `restricted`, default-deny
`NetworkPolicy`, dropped capabilities, read-only root filesystem,
non-root images). Those legacy Kustomize assets do not provision the new
component identities; they are rendering references, not the supported
installation path. OpenShift deployment is not validated by rendering alone. `worker` is the one component image that is not
distroless (it needs a Node runtime for `scc` and a `git` binary for
`internal/checkout` — see `components/worker/Containerfile`); the other
six images are distroless/static.

## Non-goals

- **Providing a GitHub Actions runtime.** That implementation was removed and
  is preserved only by `deprecated-github-actions-runtime-2026-11-14`.
- **A GitHub webhook adapter.** `admission-controller`'s Route (OpenShift
  overlay) exists, but translating a GitHub webhook payload into
  `spec/normalized-event.schema.json` is separate, reviewable follow-up.
- **A dashboard.** The runtime exposes component and deployment surfaces but
  does not ship a dashboard.

The signed transport foundation uses PostgreSQL and a transactional outbox,
without a mandatory message broker. Live relay integration remains disabled
until issue-only intake, isolated workers and release evidence are connected.
