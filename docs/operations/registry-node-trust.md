# Local node registry trust

`install.sh registry-node` installs public resolution and TLS configuration for
the original Helm-owned registry on the original kind node. Run it against a
frozen installation after registry persistence/ACL acceptance. Supply the public
registry CA as a separately reviewed file; neither a downloaded image nor issue
content can choose that trust root.

```sh
./install.sh registry-node plan --cluster ai-sdlc-demo --provider podman \
  --registry-ca /reviewed/registry-ca.crt
./install.sh registry-node local --cluster ai-sdlc-demo --provider podman \
  --registry-ca /reviewed/registry-ca.crt
./install.sh registry-node status --cluster ai-sdlc-demo --provider podman \
  --registry-ca /reviewed/registry-ca.crt
```

This adds only the exact registry host to `/etc/hosts` and its `hosts.toml` and
`ca.crt` under `/etc/containerd/certs.d/registry-service.kind-registry`. It preserves
the hosts file inode and every original byte, including when that file is a
container bind mount. It installs no passwords, changes no workload, and grants
no delivery authority. Registry TLS/hostname verification stays enabled, with
pull/resolve capabilities restricted to that host.

The reviewed containerd 2.3.4 implementation uses the certificate directory when
the configured path and mirrors are both empty. The installer validates the
version and configuration and does not restart containerd or change its main
configuration. See [the versioned initialization code](https://github.com/containerd/containerd/blob/v2.3.4/plugins/cri/images/plugin.go#L58)
and [live host configuration behavior](https://github.com/containerd/containerd/blob/v2.3.4/docs/hosts.md).

A protected `registry-node-trust.json` records cluster, node, boot, Service,
registry storage, CA and configuration identity before effects. Per-file creation
intents and inode identities prevent re-creation or adoption after deletion or
drift. Interrupted creation can bind only the exact existing file within the
original nonce-owned directory. A partially created directory without its nonce
is rejected. Reapply never overwrites a changed file or silently learns a new CA.

The real read-only node probe requires ordinary DNS resolution, verified TLS 1.3
and an anonymous denial from the private registry. The report deliberately sets
`imagePullValidated: false` and `workloadExecutionValidated: false`: node TLS is
not a kubelet pull or application rollout. A separate restricted Pod with the
original reader credential must validate image pulling before deployment
acceptance; the runtime-owned deployment must additionally prove digest, health
and user-visible behavior. No probe is workflow or provenance evidence.

`tools/verify_registry_node_files.py` runs only in an owned, offline disposable
container. CI exercises real file writes, lost replies, inode-preserving reapply,
absence quarantine, symlink rejection and hosts drift, then removes that original
fixture container.
