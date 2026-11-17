# Configure the Saferus deployer while paused

The opt-in `release` capability installs the deterministic deployer and connects
verified provenance → promotion → signed deployment handoff. It applies only to
`os-santiago/saferus`, `ai-sdlc-production`, and the existing `saferus-console`
Deployment. The reviewed target binds original Kubernetes UIDs, the API-defaulted
Deployment and Service specifications, baseline image/runtime digest, TLS CA and
a finite approval window. The runtime changes only the console image through a
conditional JSON Patch and checks readiness and the HTTPS application contract.

This is an installation increment. It does not activate intake, demonstrate an
autonomous issue-to-production cycle, implement restart recovery, or create a
GitHub release. Keep the prepared installation paused until those gates pass.

## Inputs and installation

First prepare the baseline application, its original TLS material and its
immutable `ai-sdlc-release-reader` pull Secret. The Deployment must reference
exactly that pull Secret. Independently review the target document; the
installer cannot approve observations or create/adopt an application. Verify
any baseline OCI index/manifest relationship against the original hash-checked
content before approving `baselineRuntimeDigest`.

Select images from one successful publication of the exact merged `main` commit:

```bash
python3 tools/install_images.py --run "$PUBLICATION_RUN" --revision "$MAIN_SHA" \
  --include-build --include-release --output /path/to/new-publication
```

This emits the existing runtime lock, build pins and a separate
`release-images.json`. The deployer artifact belongs to the `release-image`
class; ordinary installers continue to select their existing image inventory.
A build-branch publication is not an accepted installer source.

Follow [prepared runtime configuration](prepared-runtime-configuration.md),
adding both inputs to the existing `install.sh local --configure-prepared`
invocation:

```text
--release-target /path/to/independently-approved-target.json
--release-images /path/to/new-publication/release-images.json
```

Keep the build bundle, provenance trust and all other reviewed inputs explicit.
The selected migrator compiles and validates the public target against observed
application objects before Helm; observation repeats after application. Secret
contents stay out of the compiler and public report. Original application and
credential identities are bound to the persisted preparation record.

The installation retains both policy freezes and all ten delivery Deployments
at zero replicas. It preserves the original preparation, database, registry,
signer and physical build custody. Missing, replaced or partially applied
resources stop reconciliation. Never delete custody records to bypass a stop.
`configurationReady` describes the paused configuration; it does not mean the
runtime or issue intake is ready.

## Authority and isolation

The deployer has a separate image, ServiceAccount, identity and database role.
It receives public build, policy, trust and target mounts, plus its own private
identity and database credential. Its projected API token expires in one hour;
each transport construction rereads the projected token and CA.

Kubernetes permits GET of the two approved namespaces, the named Service and
two named Secrets, plus inspection of production Pods, ReplicaSets and
EndpointSlices. It permits GET/PATCH of only `saferus-console`. Kubernetes RBAC
does not restrict PATCH to one field: the deterministic adapter enforces the
image-only patch with original UID, resource version and generation tests.
No Pod creation, Secret listing, arbitrary Deployment mutation or namespace
administration is granted. The deployer is trusted code: GET of the two Secrets
can return their complete contents, including the application TLS key. The
model and candidate workloads receive none of these permissions.

Mandatory NetworkPolicies isolate the deployer even if the shared runtime
policy is disabled. Ingress allows the relay; egress allows DNS, the installed
database, exact API CIDRs and the production console on 8443. A production
policy admits only the runtime namespace's deployer labels to the console.
Policy enforcement requires a working enforcing CNI and a separate real
positive/negative network test; rendered policies alone do not establish it.

The service accepts only authenticated component envelopes at
`POST /internal/deploy-release`. Readiness checks current advance authority;
liveness does not cancel an already-authorized finite containment attempt.
Missing advance policy does not grant new work. Rollback still requires the
original durable acknowledgement, unchanged valid target and original deadline.

## Validation

```bash
python3 tools/verify_console_deployment.py --provider podman \
  --installed-deployer --output /path/to/new-fixture-output
python3 tools/validate_runtime.py --provider podman
```

The first command creates an owned disposable kind, installs the rendered
release resources with its producer paused, checks actual allowed/denied RBAC,
and uses that restricted ServiceAccount for a real conditional image change,
failed rollout, rollback and TLS smoke test. The business server is synthetic;
this is not vault cryptography, successful candidate rollout, network-isolation
or autonomous workflow evidence. Cleanup checks original container/image
ownership. Reports are public observations; kubeconfigs, tokens and TLS keys
remain private and are excluded from CI artifacts.

Unit tests cover canonical target compilation, altered or expired inputs,
immutable credential custody, closed publication catalogs, ten paused producers,
mandatory network policies and incomplete capability rejection. The full suite
requires real PostgreSQL and records skipped fixture tests explicitly.
