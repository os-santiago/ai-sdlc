# Conditional local console deployment

The inactive console adapter changes only the installed Saferus console image.
It does not admit issues, create promotion/deployment authority, or write workflow
history. A deployment controller must supply its authenticated durable effect
journal before this adapter can be wired into the runtime.

## Destination and effects

The approved target binds original cluster/namespace/Deployment/Service and
credential UIDs, the entire API-defaulted Deployment spec with its sole image
normalized, and the entire Service spec. The adapter checks a non-root,
read-only, capability-free console security floor in addition to those hashes.
The pull credential must be immutable and explicitly referenced by the Pod.

An advance records the original UID, resourceVersion, generation, old image and
compiled request digest. After re-observing the destination, the journal gate
must authorize that exact intent immediately before one JSON Patch request.
The patch tests identity, resourceVersion, generation, container name and old
image; it replaces only the image. Any failed or lost acknowledgement after
sending is uncertain. There is no retry or adoption based on image equality.

Rollback requires the original acknowledged advance and the same still-owned
candidate/generation. It uses a freshly observed resourceVersion because status
updates may change that version. It cannot overwrite a later rollout. Its
separate containment gate may operate after an advance freeze, but it never
bypasses target expiry. Planning and effect admission reserve fourteen of the
maximum fifteen minutes for advance, rollback, health checks and bounded overhead.

## Rollout and health

Readiness requires the exact Deployment generation, original owner chain,
ReplicaSet template, admitted Pod spec and actual runtime image digest. The
original Service endpoints must refer only to that ready Pod. These observations
are repeated around the smoke test. Unknown admission changes, extra containers,
foreign endpoints or later rollouts fail closed.

TLS uses the existing approved `saferus-console` certificate SAN. The HTTP host
and origin are fixed to `saferus-console:8443`; the TLS server name excludes the
port. The connection goes only to the approved Service ClusterIP on port 8443.
There is no ambient proxy, redirect following, insecure TLS or response-selected
URL. Bounded requests check `/health/live`, the console page and the synthetic
`POST /api/demo` request `{}`. Its response requires exactly the expected keys,
algorithm, successful token revocation, agentless flag and integer TTL 1–60.
A busy response is not success. Evidence retains hashes, not response contents.

The initial local image import can have a different runtime descriptor: its
wrapper index points to the immutable manifest used by the Pod spec. The target
must separately approve `baselineRuntimeDigest` after verifying that exact OCI
relationship and content hashes. Only baseline rollback uses this exception.
New retained images require their exact manifest digest. `status.image` is
informational because Kubernetes may report the original imported tag; the
actual checks remain exact Pod spec and approved `status.imageID` digest.

## Actual Kubernetes fixture

```sh
python3 tools/verify_console_deployment.py --provider podman --output /private/new-fixture-directory
```

This command creates an owned disposable kind cluster and a tiny trusted TLS
server, applies an intentionally unpullable image through the real API, rejects
its readiness, rolls back conditionally, and checks the original Service route
and TLS contract. It verifies imported OCI content hashes and removes its own
node, runner containers and image. CI runs this fixture with Docker. Reports and
test logs are exported explicitly; private fixture keys and kubeconfig are not.

The tiny server returns a synthetic business response. The fixture proves API,
owner, rollback and TLS behavior; it does **not** prove vault cryptography,
a successful candidate rollout, runtime network isolation, or an autonomous
issue-to-production cycle. Tests with a local TLS API server separately cover
successful candidate ownership, ambiguous acknowledgements, rejected changes,
endpoint substitution and malformed health responses.

The trusted deployer remains part of the platform's security boundary. RBAC
limited to patching one named Deployment does not itself constrain the patch to
its image if that component is compromised. The current adapter enforces that
restriction; this increment makes no enterprise-isolation or SLSA-level claim.
