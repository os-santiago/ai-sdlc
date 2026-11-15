# Independent review journal

Migration `0015` adds an inactive ledger for the verifier's future generative
review stage. No installed profile grants access, no controller invokes these
methods, and no model call or publication endpoint is added. Generation budgets,
allocations and calls remain separate and unchanged.

A review can be queued only against an already passed verification whose cleanup
is complete. It pins the exact behavior-proof digest, reviewer profile,
installation configuration and assignment. One immutable row per verification
and candidate prevents resetting its budget by changing the profile. Queueing is
not authorization: the controller must independently check the original signed
command, current issue, policy and every binding before claiming or dispatching.

Claims are exclusive, nonrenewable and limited to the earlier of the original
verification expiry or five minutes. A lost claim commit exposes no owner and
stops only the newly generated token. Parent rows serialize subordinate changes;
expiry is checked again after potentially blocking writes and before commit.

## Effects and model calls

The ledger orders `config → pod → import → start → export`. Each effect has a
unique durable intent; acknowledgements bind the expected resource name, original
Pod UID and, for import/export, content digest. A controller must persist intent
before calling Kubernetes and recheck current authority after the commit.

A review has at most eight model calls, 128 KiB per request, 1 MiB total input
and 32 KiB per response (256 KiB maximum reserved output). Each sequence starts
only after the previous handoff is durably confirmed. Calls bind the claim, Pod,
request digest and a fresh token. Existing calls never authorize invocation.
Completion records contain response hashes and bounded controller observations;
SCC summaries are not trusted observations. The caller must stop the review on
any uncertain completion or handoff, including a lost successful commit reply.
No request/response bodies or credentials are stored in this journal.

Export cannot begin without confirmed model evidence. Once export begins, or
cleanup has completed, no further model call is allowed. These APIs intentionally
provide no `passed` transition or publication handoff yet: a later controller
must validate model observations, strict verdict, unmodified source and all
current bindings before implementing that transition.

## Recovery

A known unsent intent may be durably sealed only when its owner knows the external
call was never made. Lost replies cannot be converted to no-send records. Cleanup
uses only acknowledged resource UIDs. An unknown ConfigMap/Pod create outcome
quarantines the run and blocks subsequent claims; it is never adopted by name.
Expired/failed claims have at most three cleanup attempts, spaced by 30 seconds.
The runtime controller and actual UID-removal observer remain to be connected.

The integration tests use a disposable PostgreSQL database and operational
verifier identity, a signed candidate handoff and a synthetic behavior proof.
They cover concurrent claims, wrong identities, effect ordering, model quotas,
foreign Pods, unconfirmed handoffs, expiry while a real row is locked, and actual
PostgreSQL COMMIT replies deliberately dropped by a local proxy. They do not
advance the installed Homedir workflow or prove autonomous E2E completion.
