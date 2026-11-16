# Local build workspace custody guard

The trusted artifact-builder image provides a separate `--guard-workspace` mode.
It inspects `/slot` and exits; it does not provision storage, execute source,
load registry credentials, access the network or publish an image. No installed
controller invokes this mode yet.

The installer must create a new dedicated tmpfs with exactly 4 GiB and 32768
inodes. Its root belongs to UID/GID 0 and has mode 0555. `custody.json` is a
root-owned regular file with mode 0444 and one link. It records the installed
physical identity, current node boot identity and filesystem identity. Only the
`workspace` child belongs to UID/GID 1000 with mode 0700. A source writer cannot
replace the root or custody marker.

The guard checks actual filesystem limits and identities through bounded,
no-follow file descriptors. It rejects symlinks, hard links, unexpected owners
and permissions, changed metadata and ambiguous marker JSON. Build inspection
requires an empty writable workspace; publication inspection requires a read-only
mount. Filesystem identity remains the same across these two views.

The controller must supply expected identities from reviewed installation
configuration and bind the result to its signed build receipt. The marker is
public metadata, not a credential or proof of execution authority. This check
does not establish Kubernetes/PV ownership, continuous network isolation,
admitted Pod integrity, writer termination, provenance or release acceptance.
Those remain separate runtime boundaries. The profile is local-kind specific;
it makes no claim about arbitrary remote CSI storage.

## Reproducible isolated proof

Build the artifact-builder binary and the `internal/buildstorage` test binary,
then run `tools/verify_build_workspace_guard.py --binary PATH --prepare-test PATH
--output NEW_REPORT.json`. The fixture uses a disposable container with no
network. Fixture setup alone requires root and SYS_ADMIN to set the tmpfs inode
quota; the actual guard runs as UID 1000 with zero effective capabilities. The
production binary contains no fixture setup or repair route.

The proof exercises valid build and read-only publication inspection, rejection
of nonempty build workspaces and writable publication mounts, wrong identities,
wrong capacities/inode limits, marker/root permission changes, hard links and
symlinks. It also attempts custody modification as the writer identity. Cleanup
checks the original container ID and ownership label. This is a real local
filesystem integration test, not autonomous E2E or Kubernetes runtime evidence.
