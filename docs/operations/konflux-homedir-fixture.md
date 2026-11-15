# Isolated Homedir build and provenance fixture

This procedure validates supply-chain components against a real pinned Homedir
revision. It does not submit live AI-SDLC commands, create a Homedir pull request,
release or deployment, or authorize acceptance of a candidate. The operational
issue-to-production flow remains a separate integration milestone.

Install or reconcile the isolated environment through the existing entry point:

```bash
./install.sh supply-chain local --provider podman
```

The installer enables restricted security contexts and read-only root filesystems
for Tekton's generated helper containers. Readiness checks their effective flags
as well as current deployment generations. A namespace enforcing restricted Pod
Security rejects helpers without those settings.

## Run with an immutable publisher

The `artifact-builder` component is built and published by the repository's
container workflow. Its artifact name starts with `supply-chain-image-` so it does
not change the eight-component runtime image-lock contract. Use its published
Quay digest when available:

```bash
python3 tools/verify_konflux_build.py \
  --publisher-image quay.io/os-santiago/ai-sdlc@sha256:REVIEWED_DIGEST \
  --output /tmp/homedir-konflux-build
python3 tools/verify_konflux_provenance.py \
  --build-report /tmp/homedir-konflux-build/report.json \
  --output /tmp/homedir-konflux-provenance
```

For pre-publication local validation, build from a clean, reviewed checkout and
label the image with its source revision. Export an **OCI archive**, then read its
single manifest digest from `index.json`. Pass
`--publisher-image localhost/ai-sdlc-artifact-builder@sha256:MANIFEST_DIGEST`
and `--publisher-archive /absolute/path/publisher.oci` to the build fixture.
The helper verifies every archive blob, rejects unsafe entries, imports through
the original kind node ID and requires exactly the expected cached RepoDigest.
A Docker archive or ambiguous cached aliases fail before Maven executes.

Example local image preparation:

```bash
revision=$(git rev-parse HEAD)
test -z "$(git status --porcelain)"
podman build --label "org.opencontainers.image.revision=$revision" \
  -f runtime/k8s/components/artifact-builder/Containerfile \
  -t "localhost/ai-sdlc-artifact-builder:$revision" runtime/k8s
podman save --format oci-archive --output /tmp/publisher.oci \
  "localhost/ai-sdlc-artifact-builder:$revision"
python3 -c 'import json,tarfile; a=tarfile.open("/tmp/publisher.oci"); print(json.load(a.extractfile("index.json"))["manifests"][0]["digest"])'
```

Output directories must be new. The fixture accepts only an owned Konflux kind
cluster. Its infrastructure adapter currently targets the single local kind node;
it is not a production storage provisioner.

## Boundaries and evidence

- Inputs and tool images are pinned in
  `runtime/k8s/deploy/konflux/homedir/inputs.json`. Java 21/Maven runs the complete
  `verify` lifecycle with an 8 GiB container, four CPUs and a 3 GiB test heap.
- Policies are installed before probe Pods. Positive controls and repeated
  negative TCP probes cover registry/API service and backend addresses. DNS and
  public HTTPS remain available. The actual Maven Pod repeats the negative gate
  before fetching or executing project source. A transport/exec failure is not a
  successful denial. These are point-in-time checks, not continuous isolation.
- Infrastructure creates a dedicated tmpfs inside the owned kind node with a
  physical 4 GiB byte limit and 32,768-inode limit. A node-affine localPV binds to
  the original PVC UID. Workloads have no hostPath or mounting privileges. This
  localPV has node-local backing; it does not establish production CSI isolation.
- Maven receives no registry credential or Kubernetes API token. Its original
  PipelineRun → TaskRun → Pod ownership chain is tracked. The publisher starts
  only after the writer has terminated with exit zero and no restart.
- The separate publisher mounts artifacts read-only. It opens every ancestor and
  entry without following links, rejects hardlinks/special nodes/OCI whiteouts,
  and limits files, depth, per-file size and total bytes. It never executes the
  application's Dockerfile or package contents.
- The reviewed JVM recipe appends `/deployments/{app,lib,quarkus,quarkus-run.jar}`
  to the pinned Red Hat Java base and replaces inherited runtime configuration.
  It starts Java directly; it does not inherit the source Dockerfile's shell
  wrapper or `JAVA_OPTS_APPEND` behavior. Runtime smoke/performance acceptance is
  still required before using this derived recipe for a release.
- Registry requests carrying credentials are restricted to the exact internal
  HTTPS authority, including token realms and upload redirects. Public base
  downloads use a separate anonymous transport. The publisher independently
  resolves the resulting registry digest before writing trusted Tekton results.
- Tekton Chains signs the publisher PipelineRun and OCI image. The isolated
  verifier checks the real signatures with the saved public key, rejects a wrong
  key/missing artifact, and binds subject, source revision, definition digest,
  run UID, task topology, publisher image, entry point, arguments and materials.
  A valid signature with false fields is rejected by separate synthetic-key
  regression tests. Test keys are never the Chains private key.
- Conforma's builtin image/attestation signature checks run separately with no
  release-policy sources. Passing this narrow gate is not enterprise-policy or
  SLSA-level acceptance. Transparency-log verification is explicitly disabled for
  the isolated local signing key; no transparency claim is made.

The Maven PipelineRun and publisher PipelineRun are separate. The signed
publisher statement does not attest a complete hermetic Maven build: dependency
materials and the base-fetch operation are not fully represented by Chains.
The policy does not claim reproducibility, completeness, a SLSA level, production
release authority, continuous network isolation or an autonomous E2E delivery.

## Cleanup and failures

Reports retain task/pod identities, test logs, exact image identities and signed
statements. Namespace and PV deletion use UID preconditions; infrastructure
operations use the immutable node ID and a private ownership marker. Cleanup
waits for stopped writers before unmounting the dedicated tmpfs.

The optional Results archive can retain a terminal fixture TaskRun. After its
local TaskRun/Pod logs are successfully archived and no writer is running, the
fixture may remove only `results.tekton.dev/taskrun`, using UID, resource-version
and exact-finalizer compare-and-swap tests. Cleanup first requests namespace
deletion, then reevaluates terminal ownership while Chains removes its own
finalizer. Only a deleting TaskRun with the Results finalizer alone qualifies;
Chains and foreign/additional finalizers are preserved. The loop is bounded and
API errors never count as confirmed absence. This exception is limited to disposable validation resources and must
not be copied into operational delivery controllers.

An API failure, replaced identity or exhausted cleanup allowance may retain owned
resources. Inspect `cleanupComplete`/`cleanupFailure` and the recorded identities;
never delete another namespace, PV or node path merely because its name matches.
Published test images/signatures remain in the isolated internal registry as
supply-chain evidence. No external Quay application image is published by this
fixture.
