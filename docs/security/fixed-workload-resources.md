# Fixed executor resource operations

Issue #497 extends the hidden [Pod helper transport](pod-helper-transport.md)
with fixed Kubernetes resource operations. This is not an installed controller
or authorization to execute a work item. Its future caller must record durable
intent, hold current signed execution authority, and reconcile uncertain effects.

`PlanResources` accepts the already reviewed assignment/profile/A-Dev bytes,
namespace and immutable worker/mailbox images. It copies bounded inputs and
compiles the existing restricted workload internally. It cannot accept an
arbitrary manifest, resource kind, namespace path, command or transport callback.
Its resource names are fixed by the assignment; an initial Pod UID is rejected.

## Creation and observation

`CreateConfig` sends one authenticated POST for the exact immutable ConfigMap.
It requires HTTP 201, a valid UID, exact data/labels/annotations and immutable
state, and rejects binary data, owner references, finalizers or deletion state.
`CreatePod` verifies that ConfigMap's UID and content before and after its one
POST, checks the returned Pod UID and complete spec against the compiler plus
supported Kubernetes defaults, and returns only a validated UID. Conflict does
not adopt an existing resource. Redirects and ambiguous responses fail without
automatic retry.

Each API exchange has a five-second deadline, a 1-MiB request/response bound and
the existing explicit CA/credentials with no ambient proxy or auth plugin.
Creation and observation are also capped by assignment expiry. Pod creation's
three exchanges share a 15-second outer deadline. There is no shell or kubectl
inside this library.

`WaitRunning` polls only the compiled Pod name and expected UID for at most
30 seconds. Pending scheduling and initial `ContainerCreating`/`PodInitializing` states are
allowed within that bound, including when the Pod phase is already Running.
A container the runtime has created but not yet started, which the kubelet
reports as waiting with its container ID and no reason, is allowed only in
sequence (init containers in order, then the worker and mailbox) and only
until it is observed started; its container ID is then fixed.
Both original containers must actually be Running before binding; restarts,
terminated containers and other waiting reasons are rejected. Replacement,
mutated templates, deletion or terminal Pod phases fail. Once Running is
observed, the existing strict `Bind` verifies the container instances, immutable
running image identities, restart counts and complete workload before exposing
helper operations.

ConfigMap name references are not atomic UID references in Kubernetes. The
before/after checks detect observed replacements, while the launcher separately
checks mounted assignment/profile/A-Dev digests. These checks do not claim an
atomic transaction across ConfigMap lookup, Pod creation and kubelet mounting.
Cluster administration and the authenticated Kubernetes API remain trusted.

## Cleanup by identity

`DeletePod` and `DeleteConfig` send DELETE with the original UID as a Kubernetes
precondition and never issue a second DELETE automatically. A 404 confirms
absence at that read; a 200/202 response is followed by bounded GET observations
until 404. A same-name replacement is rejected and preserved. A successful
DELETE acknowledgement alone is not a claim that the resource disappeared.
API object absence does not prove universal physical process termination during
node/network failures; observed worker exit remains separate evidence.
Cleanup has a ten-second outer deadline and is permitted after assignment
expiry so expired work can still be removed safely. There is no forced finalizer
removal, patch, broad list or namespace deletion capability.

Lost create acknowledgements can leave a resource whose UID was not confirmed.
The caller must retain the uncertain intent and handle it conservatively; this
library does not adopt, replay or guarantee cleanup of that case. Deleting a Pod
is not software acceptance, release or application rollback. The eventual
controller must apply its own ownership and policy checks around these methods.

## Permissions and real validation

No operational role is granted. The isolated fixture gives its temporary
service account `create` for Pods/ConfigMaps in its disposable namespace,
name-scoped `get/delete` and name-scoped `get/create pods/exec`. Kubernetes RBAC
cannot restrict create by `resourceNames`, and `pods/exec` cannot restrict the
command. These are powerful trusted-controller permissions; executors receive
neither the token nor those permissions. The fixture first verifies actual
creation denial before installing its scoped role.

`tools/verify_workload_resources.py` creates only fixture namespace/RBAC bootstrap
objects through kubectl. The Go client creates the actual ConfigMap and Pod,
imports the pinned 1,158-file Homedir source, starts SCC, supplies two synthetic
model replies, waits for worker termination, exports and externally compares the
candidate. It removes the original Pod, creates a same-name replacement, proves
that cleanup with the old UID is rejected without deleting the replacement,
then removes the replacement and ConfigMap and confirms absence. The Python
harness removes its own namespace using a UID precondition, including on failure.

The local kind/gVisor run passed in 20.161 seconds. Its report declares synthetic
replies, zero durable broker handoffs and no live workflow advancement. Installer
CI runs this resource fixture alongside the existing workspace and broker
fixtures and retains all three reports. TLS/socket tests cover mutation,
conflict, lost response, bounds, expiration during observation and incomplete or
replacement cleanup. These are component tests, not issue-to-production proof.
