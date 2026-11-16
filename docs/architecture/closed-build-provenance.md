# Closed Saferus artifact provenance

The isolated verifier accepts an explicitly selected source commit and tree. It
checks the archived build and publication request hashes, reconstructs the closed
publisher recipe, and recomputes the compiler's acyclic definition digest from
both complete requests, the profile and the physical slot. The build request must
contain the selected source and tree in its verification step.

Cosign verifies the actual image signature and DSSE attestation using the installed
public key. The identity binder then requires the expected artifact digest,
source, original publisher Run UID, definition digest, builder, materials, task
and both publisher steps. The signed definition digest commits to the complete
build request, including the tree SHA. Rewriting local reports and recomputing
local hashes cannot reuse an attestation signed for another definition.

Conforma runs its builtin image and attestation signature checks. The fixture
rejects a different public key and a missing artifact. It uses a local registry
and local key custody, skips transparency-log verification, and makes no SLSA
level claim. Chains currently emits SLSA provenance v0.2. Successful verification
has `releaseAuthorized: false`.

Run against an existing isolated evidence bundle:

```sh
python3 tools/verify_konflux_provenance.py \
  --cluster ai-sdlc-konflux-reviewed --project saferus \
  --build-report /path/to/closed-build/report.json \
  --closed-source-sha EXACT_COMMIT --closed-tree-sha EXACT_TREE \
  --output /path/to/new-verification-evidence
```

The bundle must include `cosign.pub`, obtained independently from the installed
signer's public-key resource, and the original compiled requests/profile/slot.
The two `--closed-*` arguments are required together. Legacy isolated Homedir and
Saferus fixture verification retains its distinct recipe checks.

This is a component integration fixture. Its local evidence bundle is not an
authority source for operational promotion. The runtime verifier must read the
signed built-record journal, obtain its expected identities from that record and
reviewed installation, and emit a separately authorized verification fact. That
runtime integration and issue-to-test-production acceptance remain pending.
