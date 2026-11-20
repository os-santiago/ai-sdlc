# Registry boundary: admitting Tekton Chains

Provenance verification needs the signature and attestation that Tekton Chains
attaches to each published image. Chains authenticates with the build run's
service account mounted secrets (k8schain `UseMountSecrets`): the `publish`
account already lists the `registry-auth` credential. The managed registry's
`registry-boundary` NetworkPolicy, however, admitted only the runtime, build and
production namespaces. Without the Chains controller in that list, every
upload timed out and provenance refused every build. That refusal is correct
and fails closed.

New installations get the corrected boundary from the chart. An existing
installation applies it with one closed transition:

```bash
./install.sh runtime-upgrade registry-boundary-status --cluster <cluster>   # read-only
./install.sh runtime-upgrade registry-boundary --cluster <cluster>
```

The transition changes only the boundary. It adds the Chains controller pods
(namespace `tekton-pipelines` plus the four controller labels) on port 5000:

- **Selection:** only an accepted managed registry qualifies: verified, with a
  passed replacement proof, a recorded chart inventory and bound storage.
- **Before the effect:**
  - the live boundary must equal the original;
  - the rendered boundary must be the original plus exactly that peer;
  - every other chart object must match with the same UIDs;
  - the three private registry Secrets keep their UID, custody nonce and
    immutability;
  - the Helm release is `deployed`.

  The journal records all of this before the effect.
- **Effect:** `helm upgrade` (never `--install`) of the same release with the
  same values. Helm receives the same kubeconfig and explicit context as the
  fences.
- **Completion:** the full chart check, unchanged custody and storage, and the
  release `deployed` at exactly the journaled revision + 1. The record's
  `chartSHA256` then moves to the new chart.

## Interrupted Helm release

If the host stops while Helm is upgrading, the release can stay
`pending-upgrade` or `failed`. The transition never resends the change and never
records completion in that state. It refuses and names the journaled revision.
Recovering the release is an operator decision, because Helm refuses new
operations while a release is pending:

1. Restore the release to the journaled revision, for example with
   `helm rollback <release> <revision>` once it is no longer pending. The
   original boundary and release must be deployed again.
2. Check `registry-boundary-status` reports `original`.
3. Remove the `boundaryUpgrade` entry from `managed-registry.json` and run
   `registry-boundary` again.
