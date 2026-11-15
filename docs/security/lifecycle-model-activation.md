# Model activation owned by the execution lifecycle

This prerequisite joins the signed execution lifecycle to the model ledger. It
does not install an operational route, grant runtime database privileges, create
a live candidate, or advance a PR, release, or deployment.

`Lifecycle.ActivateModel` accepts a trusted loader of reviewed call/byte limits.
It derives the repository, source revision, attempt, model and profile digest
from its immutable preparation. The Pod UID comes from the acknowledged source
import in the owned lifecycle. Issue prose and model responses cannot select
these bindings or supply a provider endpoint.

One PostgreSQL transaction checks the exact nonrenewable claim and source-ready
phase, changes the work item from ready to running, inserts one immutable model
budget and allocation, and links them to the lifecycle. A final database-time
check covers row and unique-index waits. Any conflict rolls the entire
transaction back. A lost COMMIT acknowledgement returns no allocation capability
and stops only the original claim; committed diagnostic rows are retained.

Preparation and initial claim validation still require ready work. Running work
is accepted only inside the in-process lifecycle with its exact persisted model
linkage. There is no restart constructor, renewal, allocation adoption or replay.
Duplicate calls on the same active object do not reset counters or stop the
successful local activation.

The lifecycle implements the broker's `CurrentAuthority`. It checks exact scope,
current reviewed limits, current signed policy/profile/ADev/placement, fresh
intake/admission/planning evidence, and durable ownership. Authorization requires
the acknowledged start-ready phase. It is denied before start acknowledgement,
after export begins, after expiry, and for a stopped budget or owner.

Model linkage observations precede the final current execution-authority proof.
Each preparation reload checks model configuration before reading execution
configuration. This ordering matters: an independent PostgreSQL regression
demonstrated that a policy freeze during the final model read could otherwise
escape revalidation. The fixed regression covers that failure explicitly.

Activation is bounded to 20 seconds and authorization to five seconds, both
capped by the original claim deadline. Trusted loaders must honor their context;
they are not sandboxed callbacks. Observations across Kubernetes, configuration
and PostgreSQL are not a globally atomic revocation protocol. The broker also
checks authority at reservation, invocation and publication boundaries, while
the ledger checks spending and expiry transactionally.

Call and byte limits are not monetary or provider-token accounting. This
prerequisite does not reconcile a stopped execution's running work item, publish
an attempt outcome, or make exported bytes trustworthy. Those transitions need
separate current authority and evidence. No SLSA level or live end-to-end delivery
is established by the PostgreSQL tests.

## Composed local validation

`tools/verify_lifecycle_model.py` runs the opt-in
`TestRealLifecycleModelFixture` driver against a disposable local
`ai_sdlc_test` PostgreSQL database and a fresh restricted namespace in kind.
The database is reset; this fixture must never target an operational database.
The driver publishes bounded Pod/ConfigMap names before the Python bootstrap
grants permission to those exact resources. Controller credentials stay in a
private control file and are never mounted into the executor.

The first actual run used pinned Homedir revision
`0622cdaeb59a11a27bd5f3d7e18183c401a5e412` (1,158 source files), gVisor and
the fixed SCC executor. It completed five durable effects, two HTTP-model
deliveries with confirmed PostgreSQL handoffs, and candidate export in 17.900
seconds. Python independently compared every exported file and mode against the
original source plus the one expected synthetic addition. Model authority was
denied after export; original resources and the fixture namespace were removed.

The fixture now also exercises [bounded candidate capture](candidate-capture.md):
it seals the model budget and independently reconstructs the persisted delta.
The lifecycle reaches `candidate-captured` while the work item remains running.
Authority, admission facts and model replies are explicitly synthetic. Neither
the fixture nor these APIs manufacture completion, acceptance, a PR, a release,
deployment, or a real issue-to-production result.

The composed fixture now uses the [bounded innerloop controller](innerloop-controller.md)
rather than implementing its own fixed two-response loop. The controller follows
the original worker through exit and retires its acknowledged resources.
