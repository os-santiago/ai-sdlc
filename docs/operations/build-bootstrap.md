# Closed build installation primitives

The normal migrator binary provides two bounded, JSON-stdin installation
operations. Neither reads issues, signs requests, writes the runtime journal,
builds software or authorizes a release. Operational build execution remains
behind the signed journal and its independent policy/custody observations.

`migrator build-admission` consumes `admission` (version
`ai-sdlc.build-admission-input/v1`, cluster/namespace UIDs, pinned publisher image,
actual network targets) and `slots` (the finite installed custody inventory).
It emits a Kubernetes `List` containing the reviewed validation policy/binding
and bounds mutation policy/binding. It does not require fabricated dependency
fingerprints or a synthetic issue. The operational profile still requires every
real dependency fingerprint and must be separately observed and approved.

`migrator prepare-build-slot` consumes `storageIdentitySHA256` and `nodeBootID`.
The administrator runs this operation as root on the selected kind node after
mounting a dedicated 4 GiB, 32,768-inode tmpfs at
`/var/local/ai-sdlc/build-slots/<storageIdentitySHA256>`. The initial mount must be
empty, writable and root-owned with mode 0755. Symlink traversal, a different
boot, shared parent filesystem and changed bounds are rejected. The default
migrator container remains non-root; this node administration operation is not
a capability granted to the runtime build service.

Initialization writes root-owned, exclusive custody metadata and creates a
UID/GID 1000 workspace. The root then becomes mode 0555. A complete reapply
checks existing custody and preserves workspace data; partial state is denied
without repair or erasure. A preserved workspace is **not** permission to reuse
it: the journal and execution guard independently enforce single use and an
empty build workspace. Root/node administrators remain trusted.

Validate the production installer in an isolated, networkless container:

```sh
cd runtime/k8s
CGO_ENABLED=0 go build -o /tmp/ai-sdlc-migrator ./components/migrator/cmd
cd ../..
python3 tools/verify_build_install.py --binary /tmp/ai-sdlc-migrator \
  --output /tmp/ai-sdlc-build-install.json
```

The fixture checks initialization, non-root denial, malformed/unknown input,
boot/path rejection, reapply with existing data, altered capacity/inodes and
partial/symlink custody. Its SYS_ADMIN capability belongs only to the disposable
fixture setup. Docker's fixture additionally disables its default AppArmor
profile to permit the intentional tmpfs remount controls; production containers
receive neither this exception nor SYS_ADMIN. It deletes only its nonce-labelled original container. This
component validation is not the autonomous end-to-end demonstration.
