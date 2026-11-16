# Saferus static artifact recipe

`ai-sdlc.saferus-go-static/v1` packages the Saferus console as a single static
Linux/amd64 executable in a scratch OCI image. OpenBao remains a separately
pinned upstream container; this recipe does not rebuild vault cryptography.

The reviewed Go build uses the pinned Go 1.27.4 image recorded in
`runtime/k8s/deploy/konflux/saferus/inputs.json`. It fetches the exact public source
commit, runs `go vet` and race tests, then builds with CGO disabled, a local
(non-downloading) toolchain, fixed target platform and trimmed paths. Source
execution has no API token or registry credential. Public HTTPS is available for
source access; this is not a hermetic-build claim.

The trusted publisher runs in a separate PipelineRun only after the original
writer Pod has terminated successfully. Its input PVC is mounted read-only. It
accepts exactly one regular executable named `saferus`, rejects symlinks,
hardlinks, special nodes, unexpected files and unsafe modes, and bounds its size
to 32 MiB. The publisher checks a static executable ELF layout, rejects dynamic
loaders and writable executable segments, and never runs the binary or a source
Containerfile. ELF validation does not prove that the application is safe or that
its bytes came from a particular compiler; build identity and provenance remain
separate checks.

Image configuration is fixed: `/saferus`, UID/GID 1000, port 8443, no inherited
command, environment or volumes. Source SHA, recipe, definition digest and run UID
are recorded in labels and the assembly report. Packaging is deterministic for
identical input bytes and identity. Runtime TLS keys, workload identity and vault
CA are deployment inputs and never enter the image.

Publisher credentials are restricted to the internal HTTPS registry. Saferus
publisher network policy permits only DNS and that registry; it does not need a
public base-image download. The publisher independently resolves the written
digest before emitting Tekton results. The recipe and destination are checked
together, so the Saferus recipe cannot write the Homedir artifact path.

## Isolated validation

Prepare the publisher OCI archive as described in
[the Konflux fixture guide](konflux-homedir-fixture.md), from a clean checkout with
its exact source revision label. Run the common fixture with the new project:

```bash
python3 tools/verify_konflux_build.py \
  --project saferus --cluster ai-sdlc-konflux-reviewed \
  --publisher-image localhost/ai-sdlc-artifact-builder@sha256:REVIEWED_DIGEST \
  --publisher-archive /absolute/path/publisher.oci \
  --output /tmp/saferus-konflux-build
```

The fixture performs positive/negative/positive network controls, a real Go build,
separate OCI publication and observation of Chains signing, then cleans up only
its owned resources. Signing observation alone is not cryptographic acceptance.
Run `tools/verify_konflux_provenance.py --project saferus` with the same cluster
and the build report to validate the real image/attestation signatures and bind
the signed source, destination, recipe, arguments, materials and run identity.
This adapter validates the installed legacy SLSA v0.2 statement format; migration
to SLSA v1 is separate work. The Homedir runtime validator remains project-specific
and must not be treated as a Saferus runtime release gate. No SLSA level, production release or autonomous E2E is established by
this isolated build. Operational orchestration must consume the immutable merge
record and acquire its own current authority; this fixture cannot advance it.
