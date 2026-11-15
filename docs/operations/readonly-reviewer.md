# Independent reviewer workload

The fixed SCC launcher supports `ai-sdlc.reviewer-profile/v1` as a separate,
bounded workload profile. It is a component prerequisite, not an activated
autonomous review controller. The candidate runtime still accepts only generation
profiles. No runtime route, model budget, publication grant or Helm deployment is
added by this change.

The reviewer starts a fresh SCC conversation with the A-Dev policy, issue criteria
and an immutable source/tree assignment. Its source mount is read-only. Its home,
temporary files and summary occupy a separate bounded volume; the mailbox can
only read that volume. The process has no credentials, service-account token,
external network or mailbox-files mount. It runs in the existing offline gVisor
runtime. The original source tree is checked before and after execution.

The reviewed SCC configuration selects only the reviewer role and expected model,
disables rework, blocks shell commands and Git mutations, and automatically permits
four inspection tools. It does not use SCC's blanket `-y` approval. Source-owned
configuration and SCC output remain untrusted; a future broker must independently
reject every proposed tool outside the four-tool allowlist before handoff.

## Result contract

The pinned SCC version reports a completed read-only review with exit code **10**
and `exit_reason=no_changes`, not a successful code modification. Export requires
that exact exit on the original container before and after the fixed helper read.
An exit code alone is not an approval.

The summary parser requires one reviewer phase, the expected model, explicit
zero rework, no model fallback, a bounded number of actual inspection calls, and
one final standalone `VERDICT: approve` or `VERDICT: request_changes`. Implicit,
conflicting, quoted, truncated or missing verdicts are rejected. Both explicit
verdicts are valid observations; `request_changes` must never authorize publishing.
Unknown telemetry does not grant authority. The summary reader rejects symlinks,
FIFOs, non-regular files and oversized content.

Even a valid summary is **advisory**. The remaining controller must bind it to
the exact candidate delta/tree, independent behavior proof, reviewer profile,
current signed authority, exclusive durable claim and trusted model-call ledger.
It must compare the complete final model response against the summary, reject
any ambiguous/undelivered call, independently compare the exported source, and
confirm cleanup. Executor-produced JSON cannot replace this evidence.

## Reproduce the component check

Build the worker and model-mailbox images from the same source and load their OCI
archives into a disposable kind cluster with the installed offline runtime. Use
`tools/fixture_images.py` to resolve the loaded images to immutable references.
Build `go test -c -o /private/reviewer.test ./internal/podtransport` from
`runtime/k8s`, then run:

```bash
python3 tools/verify_reviewer.py \
  --kubectl /path/to/kubectl --kubeconfig /private/disposable-kind.yaml \
  --worker-image localhost/ai-sdlc-worker@sha256:<digest> \
  --mailbox-image localhost/ai-sdlc-model-mailbox@sha256:<digest> \
  --test-binary /private/reviewer.test --scenario approve \
  --report /private/reviewer-approve.json
```

Repeat for `request_changes`, `implicit` and `mutation`. The last two must reject
the summary. Every scenario requires the original source to remain unchanged.
The fixture uses real SCC/Kubernetes and synthetic model replies, creates its own
namespace and removes it by UID. It does not ingest or advance a live issue,
publish a PR or establish end-to-end completion. Installer CI runs the same cases.
