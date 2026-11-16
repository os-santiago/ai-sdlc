# Saferus E2E readiness

Observed on **2026-11-16** in the owned local `ai-sdlc-konflux-reviewed` cluster
(Kubernetes v1.36.4). **The autonomous issue-to-test-production flow has not run.**
The runtime remains frozen and Saferus issue
[#4](https://github.com/os-santiago/saferus/issues/4) has no ingestion trigger label.

| Boundary | Observed state | What the evidence establishes |
|---|---|---|
| Saferus baseline in `ai-sdlc-production` | Installed and reapplied | Actual TLS/authentication/authorization/encryption/audit/network controls; Firefox desktop/mobile with normal X.509 checks; bounded pressure and recovery. |
| AI-SDLC component publication | Eleven immutable image identities resolved | One successful reviewed-main publication; exact component, source revision and archive identity checks. This is not application artifact provenance. |
| Persistent build foundation | Prepared, sixteen resources | Original namespace/storage/node identities, compiled native policies and separately reviewed trust; real reapply preserves UIDs/mount/bundle. |
| Build installation preflight | Passed | The published normal migrator observed a profile exactly matching the approved bundle through the original node. It creates no workflow command. |
| Signed build controller | Implemented and published; not activated here | Real isolated journal/transport/containment tests and reviewed installation support. Preparation does not imply a running delivery cycle. |
| Independent application provenance | Pending | Needs retained registry attestations, independent current trust and a durable verifier result. |
| Promotion, deployment and acceptance | Pending | Need authenticated component-owned transitions, digest-preserving rollout, bounded recovery and real browser/API acceptance. |

The [sanitized installation evidence](../evidence/2026-11-16-persistent-build-foundation.json)
records publication revision `bd967118eb16aaeea607eecab07b552ed351559b`, the exact
image digests, original identities, trust review scope and reapply/preflight
results. Saferus [PR #6](https://github.com/os-santiago/saferus/pull/6) records the
explicit Konflux environment selection and its cold-install evidence. These
are administrative setup and component validations, not substitute workflow
receipts or proof of autonomous delivery.

## Reproduce preparation

Use the [persistent foundation procedure](build-foundation.md) against an owned,
frozen Konflux installation. Resolve images from one successful reviewed-main
publication, prepare original resources, review the trust candidate separately,
then supply that exact approved public input. Reapply must preserve the original
resource and physical workspace identities. Do not adopt or erase a resource to
make a failed check pass.

Saferus can select the same installed Konflux cluster explicitly:

```sh
./saferus test-environment --provider podman \
  --ai-sdlc-cluster ai-sdlc-konflux-reviewed
./saferus verify-environment --provider podman \
  --ai-sdlc-cluster ai-sdlc-konflux-reviewed
```

Run these in the Saferus checkout containing its reviewed environment-selection
change. Runtime state and application custody use separate derived directories;
original owner/capability/namespace identities are checked before application
namespace creation. The older `ai-sdlc-saferus` test installation remains a
separate environment; a directory alone cannot authorize a target switch.

## Remaining acceptance gates

1. Implement [operational provenance verification #574](https://github.com/os-santiago/ai-sdlc/issues/574).
   Select authenticated historical build facts and fetch image/DSSE evidence by
   exact registry digest, with independently approved current trust. Preserve
   enough immutable compiler context to reconstruct source/tree/definition and
   recipe bindings. Do not depend on Tekton objects already removed by cleanup.
2. Implement current-policy-gated promotion and namespace-scoped deployment with
   a durable original identity, loss/restart handling and bounded rollback.
   Verification alone must not grant deployment permission.
3. Validate the complete installation and a containment-preserving upgrade or
   recovery path before activation. Preserve cleanup privileges and storage;
   the initial installer intentionally refuses automatic enabled-build upgrades.
4. Admit the low-risk Saferus issue only after the required chain is installed.
   Its implementation, independent review, forge gates, merge, artifact build,
   verification, deployment and acceptance must be performed by AI-SDLC components.
   A supervising agent must not call internal stages or write workflow rows.

The demo has a single node, synthetic co-located vault custody and a single-use
build slot backed by tmpfs. Its observed timings and cryptographic checks are
not an enterprise SLO, certification, SLSA level, HA proof or production security
assessment. The approved platform trust is a point-in-time administrative
snapshot; normal pre-execution controls remain required.
