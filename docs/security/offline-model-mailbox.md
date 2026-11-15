# Offline model mailbox: hidden transport prerequisite

The executor's [offline runtime](offline-executor-runtime.md) has no external
network. `model-mailbox` provides a loopback endpoint for SCC and fixed helper
commands for exchanging bounded data. It has no provider client, credentials,
database connection, GitHub client or delivery-stage API. This is a hidden
prerequisite for #422, not an execution mode or a completed E2E pipeline.
The image also includes the separate hidden [workspace file-transfer helper](offline-workspace-bundles.md).

## Boundary and protocol

`serve --attempt <id> --pod-uid <uid>` listens only on `127.0.0.1:8091`.
The only model route is `POST /v1/chat/completions`; streaming is rejected.
Requests are limited to 128 KiB, responses to 256 KiB, one outstanding request,
32 requests per process, eight connections and a 60-second exchange deadline.
HTTP headers, reads and idle connections also have finite bounds.

The server publishes `request.json` inside `/bridge`. A trusted controller can
invoke the immutable `/model-mailbox read` helper using Kubernetes exec with
an explicit container and assigned identity. `respond` takes a response envelope
on stdin. Both helpers have a five-second process deadline, including blocked
stdin/stdout, and exit 124 on timeout; an absent or still-publishing request is exit 3.

Every invocation compares the expected UID with the read-only Downward API file
`/var/run/ai-sdlc/pod/uid` before opening the mailbox. Envelopes have exactly these
fields: `version`, `kind`, `attempt`, `podUID`, `sequence`, `requestSHA256`,
`status`, `expiresAt` (Unix milliseconds), and `payload` (base64 preserving exact
bytes). Version is `ai-sdlc.model-mailbox/v1`. The SHA-256 always identifies the
request payload. Responses must match its identity, sequence, digest and deadline.
Unknown/duplicate/case-variant/null envelope fields and expired responses fail.
No response headers, upstream URLs or external transport options are forwarded.

Filesystem access uses an opened directory descriptor and fixed flat names.
Reads use no-follow and nonblocking opens, require a regular file with one link,
bound bytes, and check size/timestamps again on the same descriptor. Writes stage
privately, sync, create the final name atomically with `linkat` (no replacement),
then unlink staging. Readers return pending while the file has multiple links,
without reading its bytes; the server retries only until the exchange deadline.
A persistent hostile hard link stays unavailable. A concurrent writer or symlink
cannot replace the winner. The tested gVisor/backing-filesystem combination
returned EINVAL for `renameat2(RENAME_NOREPLACE)`; ordinary replacing rename is
never used as a fallback. Unsupported publication fails closed.
A server refuses stale pending state at startup; recovery requires a new attempt
volume and controller reconciliation, not replaying ambiguous model calls.

## What this does not authorize

The mailbox and its contents remain untrusted. The executor can forge its own
data, restart a process or deny service to itself. Local counters and expiry
fields are containment aids, not authoritative cost or authorization controls.
Payload/schema checks here do not make model messages or tool arguments trusted.

Before any real provider invocation, a separate trusted controller must:

- Bind the attempt to an admitted issue, immutable Pod UID/image/template and
  reviewed model policy; ignore executor-supplied destinations or authority claims.
- Reserve durable per-attempt and total cost/call budgets outside the Pod, key
  idempotency to actual request bytes, and stop on ambiguous provider outcomes.
  The hidden [call reservation ledger](model-call-reservations.md) supplies
  per-work-item call/byte accounting; provider billing and integration remain pending.
- Keep provider secrets outside the executor; independently validate provider
  output limits and sanitization. No generic URL or header proxy is permitted.
- Use scoped exec RBAC, fixed executable/arguments and explicit container names;
  reject Pod replacement and recheck assigned identity. RBAC alone does not
  restrict which executable an exec-capable client can invoke.
- Enforce immutable workload templates, no service-account token or credentials,
  restricted security context, bounded emptyDir and runtime limits, and verified
  offline runtime availability. Neither Helm nor Kustomize launches these Jobs yet.
- Treat candidate files and claimed checks as untrusted; independent verification,
  publication, release and deployment authority remain separate components.

## Reproducible component validation

Run `go test ./internal/modelmailbox ./components/model-mailbox/cmd` from
`runtime/k8s`. Tests include malformed identities, exact payload preservation,
HTTP limits/concurrency/deadlines, hostile file types, directory replacement,
growing files, late/forged responses and concurrent publication.

Build `components/model-mailbox/Containerfile` with context `runtime/k8s`, load
the local fixture image into an installer-owned kind cluster after `install.sh
sandbox`, then run from the repository root:

```sh
python3 tools/verify_model_mailbox.py \
  --kubectl /path/to/verified/kubectl \
  --kubeconfig /path/to/installer/kubeconfig \
  --mailbox-image localhost/ai-sdlc-model-mailbox:conformance
```

The fixture creates a temporary restricted namespace and offline gVisor Pod,
uses the pinned worker image for actual SCC, and supplies synthetic model replies
through the fixed helper. It tests identity, correlation, concurrency and size
rejection, then verifies SCC's no-change exit against HEAD, indexed/worktree diff
and untracked files independently. Namespace deletion uses a UID precondition.
The report explicitly says `isolated-component-fixture` and
`autonomousExecutionEnabled: false`. It does not consume or advance a live issue.

Known SCC limitation: the pinned adapter interpreted “Do not modify files” as
mutation scope and triggered its zero-mutation guard; evidence is recorded in
#419. The transport fixture uses an unambiguous read-only question. Successful
transport does not resolve that classifier limitation or prove convergence.
