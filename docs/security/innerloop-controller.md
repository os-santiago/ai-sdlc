# Bounded innerloop controller

`execution.RunController` joins the signed execution lifecycle, fixed Kubernetes
resources, SCC mailbox/broker and durable candidate capture. It is reusable
runtime code, exercised by the isolated kind fixture. The installed services
still expose maintenance/shadow only; this API does not add a live route,
change role grants, schedule work or authorize promotion.

A trusted controller supplies a freshly prepared signed claim, current reviewed
configuration loaders, Kubernetes transport and a configured model provider.
Issue/model content cannot choose those capabilities. The sequence is:

1. Transfer the preparation claim once into a bounded lifecycle.
2. Persist intent and acknowledge the fixed ConfigMap and Pod by UID.
3. Wait for the original Pod, import the exact source and activate its model budget.
4. Acknowledge start, exchange model requests serially and recheck authority on
   every idle cycle and each broker boundary.
5. Observe the original worker's zero exit, export and capture the unvalidated delta.
6. Delete acknowledged resources using UID preconditions, including on failure.

Completion is driven by the worker instance, not by a fixed call count, model
prose or `finish_reason`. Duplicate reads must be byte/identity equivalent to
the preceding request. Gaps, rebinding, nonzero exit, revoked authority,
ambiguous dispatch or cancellation stop the attempt; there is no provider retry.
Read/exit races use a distinct transport outcome emitted only after validating
the helper result and observing the original worker exit. Protocol rejection
remains terminal even if a subsequent observation would show exit zero. Export/capture still require the unchanged identity, completed handoffs
and a nonempty reconstructable delta.

All operations use the nonrenewable lifecycle deadline. Failure stops the
**current transferred claim token**, including errors outside lifecycle methods.
Cleanup gets a separate 25-second budget; failure prevents returning a candidate
as a successful controller result. Previously committed evidence is retained.
A create whose acknowledgement/UID was lost is never adopted or deleted by name.
Such uncertain resources require a separate ownership-aware recovery path;
this API is not a restart/recovery controller.

The candidate remains unvalidated and the work item remains running. An outerloop
verifier must independently enforce approved scope, recipe, source and tests
before any publisher receives authority. A generation success cannot stand in
for those controls or for application-user acceptance.

## Actual component checks

`tools/verify_lifecycle_model.py` invokes this controller in disposable PostgreSQL
and a fresh kind namespace using the pinned Homedir source, real SCC and gVisor.
The fixture supplies synthetic signed authority and HTTP model replies. Python
independently compares the persisted delta and full reconstructed tree against
its prescribed change, and checks that the named Pod and ConfigMap are absent
before namespace cleanup. It does not claim a live issue result.

Use `--scenario` to select `success`, `three-turns`, `provider-failure`,
`cancel-provider` or `no-change`; all five run in installer conformance CI.
The successful variants exercise different request counts. The rejection
variants require zero captured candidates, one stopped current claim and no
provider replay. The no-change case reads an existing file without modifying it and requires
an explicitly observed nonzero SCC exit; an unrelated transport/capture failure
cannot pass that scenario. Separate candidate tests reject a zero-exit empty delta. Local unit regressions additionally exercise idle revocation,
idle cancellation, duplicate substitution, sequence gaps and exit races.
