# Observe a closed build installation

The production migrator provides three read-only operations. They accept bounded
private JSON on stdin with explicit Kubernetes `client` credentials and an array
of original `slots`. Credentials are never printed; outputs contain public
configuration and fingerprints. These commands do not sign requests, select an
issue or source, create workloads, write a journal, or authorize releases.

1. `observe-build-admission` additionally accepts `publisherImage`. It checks
   original cluster/namespace/PVC/PV/node/boot custody and obtains actual API and
   registry network targets. Pass the returned `admission` and original `slots`
   to `build-admission` to render the finite installation policies.
2. After installing the policies and explicit bundle ConfigMaps,
   `observe-build-trust` returns a **candidate** inventory of trusted controller,
   network implementation, configuration and all mutating admission resources.
   An observation is not approval. The administrator must compare this candidate
   with the reviewed platform installation and approve it separately. Runtime
   drift must never trigger automatic relearning or replacement of this trust.
3. Store the approved inventory as the immutable `ai-sdlc-builds/build-trust`
   ConfigMap, with one `trust.json` data field. `observe-build-profile` requires
   `admission`, `slots`, and the separately selected `approvedTrust`. It rejects
   a different installed trust value, missing/changed custody, unready platform
   controllers, changed inventory, or native policies differing from the exact
   compiler definitions. It rechecks custody before returning the public profile.

A profile is configuration, not a capability. The signed build journal and
runtime policy/freshness/physical-slot checks still determine whether any work
may execute. The API server, node and platform administrators remain trusted;
these point-in-time observations do not prove continuous isolation.

The isolated integration harness supports `--installer-binary` together with
`--preflight-only` (mutually exclusive with `--test-binary`) and requires an
explicit `--installer-image sha256:<image-config-id>`. It extracts `/component`
from an unstarted container of that exact local image, compares its binary
SHA256, and checks the copy on the original kind node before execution. Reports
bind image and executable identities; a substituted binary is rejected. It runs the normal
migrator binary through initialization, admission, trust and profile observation,
then checks rejection of a changed separately supplied trust value. This fixture
approves its own snapshot only for that disposable test namespace; it never
installs operational authority. Reports distinguish it from synthetic signed
workload tests and record original-UID cleanup.
