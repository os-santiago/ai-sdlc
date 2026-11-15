# Pod-bound helper transport: hidden controller prerequisite

Issue #473 connects the [model broker](model-broker.md) publication capability
to the fixed [offline executor](offline-executor-launcher.md) mailbox. It is an
internal library, with no public route, installed controller, operational RBAC
or live workflow transition. Signed execution authority and durable ownership
of an assignment remain prerequisites for its future caller.

## Binding the workload and container instances

`Bind` recompiles the trusted assignment/profile/A-Dev inputs and immutable
worker/mailbox image references. It reads the actual Pod through verified TLS
and checks its namespace, name, UID, labels, annotations and complete spec.
Unknown JSON fields and duplicate keys are rejected before typed decoding.
Only the concrete Kubernetes 1.35 API defaults observed in the kind fixture are
added to the expected spec. The scheduled `nodeName` is the sole spec field
omitted from equality; extra volumes, init/ephemeral containers, credentials,
commands, privileges and admission mutations fail closed.

The supported kind/containerd profile exposes the running image manifest digest
in `status.imageID`, even when `status.image` reports an imported tag. Each
running manifest digest must equal its compiler pin. This version requires
**platform manifest pins**: a multi-platform index must be resolved and reviewed
to a platform manifest before use. Other CRI representations or admission
defaults require explicit conformance work; they are not silently accepted.

The session captures both containers' `containerID`, `imageID` and reported
image reference. Every operation checks them again, rejects nonzero restart
counts, a deleting/replaced Pod and unexpected container state. The mailbox
must remain running. After publication, the same worker instance may have
terminated after consuming the final response; its exit status is separate
candidate-validation evidence, never proof of a successful delivery here.

These observations are not an atomic Kubernetes precondition. The helper also
checks the read-only projected UID, which rejects execution against a different
Pod with the same name. The API server, kubelet and authorized cluster
administrators remain trusted. An actor allowed to mutate Pods or inject an
ephemeral container is outside this boundary. Workload code and helper callers
must receive no such authority, and executors receive no Kubernetes credentials.

## Fixed operations and publication evidence

The model operations are `/model-mailbox read` and `/model-mailbox respond`, in
the `mailbox` container, with the session's fixed attempt and UID. Callers cannot
supply a shell, binary, container, path or arbitrary command. The helper performs
exclusive filesystem publication and then emits a strict receipt binding
attempt, UID, sequence, request digest, response digest, status and byte count.

The client requires a matching receipt, a successful explicit remote status,
complete bounded output, an accepted WebSocket closure and a second external
Pod observation. A successful socket write or clean EOF alone is insufficient.
Cancellation or an uncertain connection never triggers another `respond`.
Remote process termination is not guaranteed by closing a client connection;
the helper also has its own five-second deadline. The broker's stop/recovery
rules handle uncertainty. Publication still does not prove SCC consumed data.

The pinned Kubernetes client supplies authenticated TLS/WebSocket negotiation.
A small internal V5 channel reader handles these fixed helpers, because the
generic client [accepts empty status on clean EOF and reads status without an
application byte cap](https://github.com/kubernetes/client-go/blob/v0.35.8/tools/remotecommand/errorstream.go). The channel reader requires exactly one complete status,
allows fragmentation, and rejects unknown channels or trailing/duplicate JSON.
It has one writer, one reader, an absolute five-second operation deadline,
1,024 data-message cap and cumulative limits of one mailbox envelope (read) or
2 KiB (receipt), plus 8 KiB each for stderr and status. It joins its writer on
exit. This bounds application channels, not every internal allocation made by
the upstream TLS/HTTP handshake implementation.

API configuration contains explicit HTTPS endpoint, CA bytes and either a
bearer token or client certificate/key. It cannot load kubeconfig auth plugins,
ambient proxies, insecure TLS overrides or caller transport callbacks. API
observations have a five-second limit and a 1-MiB response cap. There is no
SPDY/protocol fallback, redirect following or automatic retry. Raw API errors,
credentials and provider bodies are not returned in diagnostics.

## Permissions and actual validation

No production role is added. A future reviewed controller needs `get` on the
specific Pod and both `get` and `create` on its `pods/exec` subresource for the
V5 GET upgrade. Kubernetes 1.35 enables the additional create authorization by
default ([upstream change](https://github.com/kubernetes/kubernetes/pull/134577)).
**`pods/exec` is powerful execution permission despite the GET verb.** Kubernetes
RBAC does not constrain command or container arguments; the fixed trusted
library does. Give it a separate scoped identity, never the executor identity,
and exclude patch/update and `pods/ephemeralcontainers` permissions.

`tools/verify_pod_transport.py` uses disposable kind/gVisor Pods, pinned Homedir
source, real SCC, PostgreSQL and synthetic HTTP model responses. It exercises
the actual broker and two helper publications, verifies durable handoff rows
and metadata-only duplicate delivery, observes worker termination, and compares
the exported candidate externally. It also checks a real read-only principal's
exec denial, proves GET-only exec permission insufficient, then performs the
successful exchange with only Pod observation and scoped GET/create exec rights.
It replaces the Pod and proves the old UID rejected while the new UID
otherwise binds. Its temporary credential files are private and removed.

Socket tests cover missing receipt/status, clean and abnormal closure,
fragmentation, duplicated/truncated status, channel overflow, cancellation,
Pod mutation/restart, unknown fields and an initially wrong running image.
The installer CI builds the test driver, runs the composed fixture on Docker/
kind and retains its sanitized report. These are component conformance tests;
they do not execute a live issue, call Codex, approve a candidate or deploy a
release. Autonomous signed controller integration remains unfinished.

## Verified workspace lifecycle (issue #492)

The same bound session now exposes three additional fixed operations in the
mailbox container. `ImportSource` verifies the entire bounded source bundle,
source commit and tree digest against the assignment before calling
`/workspace-transfer import`. Its strict receipt must acknowledge exactly the
expected file count and bundle digest. `Start` invokes `/executor-control start`
with the assignment digest, source tree and Pod UID; the helper exclusively
creates the start marker, while the launcher independently validates its input.
A successful marker acknowledgement is not proof of successful SCC execution.

`ExportCandidate` requires the original worker instance to have terminated both
before and after `/workspace-transfer export`. The exit code must remain the
same. It returns bounded, decoded candidate bytes through a copying accessor.
Nonzero worker exits are retained for diagnostics, never accepted as successful
software. A valid bundle and zero exit still require independent validation,
review and release policy. A new session cannot bind an already terminated
worker; durable recovery/reconciliation is not implemented by this library.

Import/export have 30-second transport limits, 32-MiB bundle limits and
40-second outer limits; import receipts are capped at 2 KiB. Export permits at
most 8,192 channel messages; other operations retain 1,024. Start has a
five-second transport limit and requires empty stdout. Every observation and
transport deadline is capped by assignment expiry, and an observation that
finishes after expiry cannot initiate a helper. Caller cancellation can shorten
these bounds. Closing a socket does not atomically revoke remote effects: an
already started import can continue under its own 60-second helper limit.
Uncertain operations require durable controller reconciliation, never blind
retry. Current signed authority and lifecycle ownership remain caller duties.

`tools/verify_workspace_transport.py` exercises import, exclusive start, two
synthetic mailbox replies, observed SCC termination and export through this Go
session on an actual disposable kind/gVisor Pod with the 1,158-file pinned
Homedir tree. It compares every retained file and the single fixture addition,
rejects an export while the writer is running, and verifies actual exec RBAC
and Pod replacement rejection. It does not use PostgreSQL or a real model;
its report explicitly records zero durable model handoffs. Installer CI runs
both transport fixtures and retains their reports. No operational controller,
execution grants, issue transition, pull request or deployment is enabled.
