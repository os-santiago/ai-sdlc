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

The base co-locates four containers in one pod. When concurrency requires
horizontal scaling, the worker moves to a dedicated Deployment without
changing contracts.

## Security posture

Every container runs non-root (distroless `nonroot` base image, fixed
UID 65532), with `allowPrivilegeEscalation: false`,
`capabilities: {drop: [ALL]}` and `readOnlyRootFilesystem: true`. The
namespace enforces the Pod Security `restricted` profile. A default-deny
`NetworkPolicy` only opens DNS, HTTPS, the event bus, the durable store and
the OTel collector ports for control-plane pods.

This satisfies OpenShift's default `restricted-v2` SCC without a custom SCC
object or an explicit `runAsUser` — see
[../../README.md](../../README.md) and
[ADR-0004](../../../../docs/architecture/adr/0004-cloud-native-k8s-openshift-runtime.md).
