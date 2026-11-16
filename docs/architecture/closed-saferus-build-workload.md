# Closed Saferus build workload (inactive)

The compiler accepts a signed build command, installed profile and one journal-owned storage slot. It emits exact build and publication PipelineRuns and hashes their bytes into the persisted plan. It accepts no caller-provided shell, image, arbitrary task or manifest. This package does not install a controller or authorize deployment.

## Execution boundary

The source writer has no registry credential or service-account token. A separate publisher receives the original slot read-only and uses a fixed registry/recipe. Both begin with a custody guard. The compiled guard checks the signed expiry against a complete phase budget, so delayed scheduling cannot start work with stale authority. The inspection-only guard remains usable without this optional deadline; the compiler and mandatory admission always supply it.

The slot is a separately provisioned 4 GiB tmpfs with 32,768 inodes. Native Kubernetes mutation makes Tekton helper volumes memory-backed: 96 MiB for its 65,903,252-byte entrypoint, 64 MiB for temporary files, 16 MiB for other helpers. Container memory limits also apply. These helper volumes do not have the slot's explicit inode limit. A namespace LimitRange supplies helper CPU/memory/storage budgets.

A mandatory ValidatingAdmissionPolicy checks the final Pod after mutation: fixed images, arguments, scripts, mount paths, resource limits, role/slot/node correspondence, no credentials in source execution, no sidecars or ephemeral containers, no host namespaces and restricted runtime selection. Build and publication deadlines are 600 and 300 seconds; termination grace is at most 30 seconds. CREATE cannot preassign a node; UPDATE preserves the original assigned slot node so Tekton can publish its READY annotation.

## Trust and installation prerequisites

This local profile requires Kubernetes 1.36+ with native MutatingAdmissionPolicy v1. The existing runtime cluster at 1.35.8 has not been upgraded or adopted by this change. The separate 1.36.4 supply-chain fixture is the validation target.

The trusted computing base includes cluster administrators, the API server, kubelet, canonical Tekton controllers and every applicable mutator. Before any create, a future transport must re-observe installed UID/spec dependencies and conservatively inventory all mutating webhooks and native mutating policies. Unknown applicability denies execution. Inventory is point-in-time; installer/freeze policy must prohibit concurrent changes. This is not protection against malicious administrators or a continuously attested CNI. Dynamic source hashes are exact compiler inputs checked by the trusted transport, not authenticated by a static Pod policy alone.

The runtime must prove actual writer termination before publication, retain original UIDs across recovery, quarantine ambiguous creates and never reuse consumed physical storage. These are journal/transport obligations. A successful build result alone is not release authorization or cryptographically verified provenance.

## Validation scope

`tools/verify_build_admission.py` uses an exclusive owned fixture namespace and synthetic PipelineRuns. Its node is proven absent and its PVC is never provisioned. It inspects actual Tekton Pods, performs server-side positive/negative admission requests, and tests READY updates after binding only to the absent node. No source workload executes. A temporary namespaced RBAC grant limited to the two original fixture Pods lets the ephemeral-container negative control reach the VAP; production controller RBAC already denies that operation. Cleanup uses original object UIDs, records evidence before cleanup and retries a failed finalizer CAS only after re-reading ownership and state. Host-namespace and missing-runtime-class negative controls can be rejected by native admission before the custom policy; the report identifies those cases separately.

The pinned entrypoint initializer also ran successfully under Podman with UID 1000, no network, no capabilities, read-only root, 256 MiB memory and a 96 MiB memory-backed executable volume. This proves helper initialization, not a complete build or E2E.

Operational transport, installation reconciliation, independent provenance verification and promotion/deployment remain separate work. No eligible Saferus feature issue has been submitted by this compiler change.

The final evidence is [recorded here](../evidence/2026-11-16-closed-saferus-build-compiler.json): 1,364 Go/PostgreSQL tests in 42 packages; four positive admission cases and 34 negatives, including four native pre-policy denials. Policy type checking reports no warnings. A fresh network conformance check passed using the installed kindnetd controller; it is a point-in-time result.
