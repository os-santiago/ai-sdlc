# Verify the paused release network

After installing the reviewed release target and published runtime images, verify
actual network enforcement before `initial-runtime activate`:

```bash
python3 tools/verify_release_network.py \
  --cluster ai-sdlc-e2e-managed-20261116 --provider podman \
  --output /absolute/path/to/new-release-network-report
```

The command requires the original, complete `configured-paused` installation. It
uses its pinned worker image for disposable, restricted Pods without service
account tokens. It verifies the approved deployment and Service specifications,
original cluster/namespaces, immutable reader and TLS credential custody, and the
installed release policies. It does not create workflow inputs or change Saferus.

Two consecutive observations must establish each boundary: the deployer labels
in the runtime namespace can reach the original console with its approved TLS CA;
the same labels in a fresh foreign namespace cannot. The foreign client reaches
its own live control server before and after the denial, and the permitted TLS
route succeeds before and after it. TLS validation errors, malformed responses,
missing images and `exec` failures are failures, never isolation evidence.

Resources have random names and ownership nonces. Cleanup attempts each resource
independently using its original UID, refuses replacements, and does not remove a
namespace containing remaining Pods. Reports preserve any cleanup failure; inspect
those resources rather than deleting a replacement. The complete installation is
checked again after cleanup, including unchanged application configuration.

This proves this installed network boundary at the observation time. It is an
administrative readiness check, not evidence that an autonomous issue reached a
release, and it does not establish performance capacity or remote-cluster parity.
