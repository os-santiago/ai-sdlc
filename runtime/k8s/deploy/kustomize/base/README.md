# Kustomize GitOps deployment

`base` defines topology, security and component identities. Overlays define
environment configuration and image references.

```bash
kustomize build ../overlays/development
kustomize build ../overlays/production
kustomize build ../overlays/openshift
```

CI builds, tests, scans and publishes each component image. Promotion updates
immutable image digests in an environment overlay through a pull request.
Runtime mutation is prohibited; rollback is a Git revert or reviewed digest
change.

No literal credentials belong in Git. Production must use External Secrets,
SOPS or Sealed Secrets. The `.ai-sdlc.yaml` contract mounted at
`/etc/ai-sdlc/contract` (see `contract-configmap.yaml`) is config, not a
secret — but it should still come from the target repo via a reviewed
GitOps PR, not a manual `kubectl edit`.

The base co-locates five containers (admission-controller, orchestrator,
worker, release-manager, relay) in one pod. When concurrency requires
horizontal scaling, the worker moves to a dedicated Deployment without
changing contracts; `relay` would need `AI_SDLC_RELAY_ROUTES` repointed at
Services instead of `localhost` if it is ever split out (see
`internal/outbox.DefaultRoutes`).

## Durable state (ADR-0005)

`postgres-cluster.yaml` is a [CloudNativePG](https://cloudnative-pg.io/)
`Cluster` custom resource. Install the CloudNativePG operator cluster-wide
first — one-time, outside this Kustomize tree:

```bash
# OpenShift: subscribe to "CloudNativePG" from OperatorHub (web console or
# an OLM Subscription object), or:
kubectl apply --server-side -f \
  https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/release-1.24/releases/cnpg-1.24.0.yaml
```

CloudNativePG generates a Secret (`ai-sdlc-postgres-app`, key `uri`) that
every component's `DATABASE_URL` env var reads a ready-to-use
`postgresql://` connection string from. If your cluster already runs a
different Postgres (another operator, a managed instance), delete
`postgres-cluster.yaml` from the overlay's `kustomization.yaml` and point
that Secret (or `deployment.yaml`'s `secretKeyRef`s) at your own instance —
no application code is CloudNativePG-specific.

## Security posture

Every container runs non-root (distroless `nonroot` base image, fixed
UID 65532), with `allowPrivilegeEscalation: false`,
`capabilities: {drop: [ALL]}` and `readOnlyRootFilesystem: true`. The
namespace enforces the Pod Security `restricted` profile. A default-deny
`NetworkPolicy` only opens DNS, HTTPS, Postgres and the OTel collector
ports for control-plane pods — `relay`'s traffic to the other four
components never crosses this boundary, since all five are containers in
one pod (loopback, not subject to `NetworkPolicy`).

This satisfies OpenShift's default `restricted-v2` SCC without a custom SCC
object or an explicit `runAsUser` — see
[../../README.md](../../README.md) and
[ADR-0004](../../../../docs/architecture/adr/0004-cloud-native-k8s-openshift-runtime.md).
