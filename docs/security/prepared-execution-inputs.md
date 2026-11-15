# Verified preparation of executor inputs

`internal/execution.Prepare` composes the signed execution claim, current
admission proof, reviewed executor/A-Dev inputs and pinned GitHub source into
immutable input bytes. It is a hidden controller prerequisite: the installed
worker still has no execution endpoint or queue/proof privileges. Preparation
creates no Pod, provider call, candidate, PR or release.

## Input ownership

The caller supplies an existing claim and a trusted installation loader. The
loader rereads current policy, intake configuration, executor profile and A-Dev
bytes, honoring the supplied context. It must keep returned values stable while
they are copied; subsequent reloads may reuse and modify their own buffers.
Preparation makes independent copies of policy keys/grants, actor IDs and byte
buffers, so later mutation cannot retroactively change its reference snapshot.

The profile's exact digest must match the signed command, and its A-Dev digest
must match the bounded UTF-8 policy bytes. Issue content cannot supply a profile,
model endpoint, credential, executable command or filesystem destination.
The optional source credential is trusted installation input; it is absent from
prepared bytes, model inputs and returned errors. Public source acquisition is
validated; private repository credentials still need their own integration test.

The durable claim's ID, random token, exact lease deadline and claimed state
must match, with both lease and command lifetime valid at database time. The
stored receipt and immutable command columns must verify against current
authority. The existing [admission proof](signed-execution-requests.md) establishes
repository/plan ownership, generation, criteria, decision and per-issue freshness.
Title, body and criteria are derived from that validated snapshot.

## Bounded acquisition and revalidation

Preparation has a 45-second cooperative deadline capped by the claim's remaining
lease. The fixed [source client](pinned-source-acquisition.md) has its own shared
30-second HTTP deadline and archive/tree bounds. It accepts only the verified
repository and commit SHA, never an issue-supplied URL or branch. Unsupported
assignment shape is rejected before network acquisition.

The acquired bundle must match that source SHA. Its canonical tree digest binds
the assignment, and its bounded encoded bytes become the prepared source.
After source I/O and hashing, preparation repeats the durable claim, current
admission and input checks. Policy/configuration are reloaded around bounded
database reads, including final receipt validation. Mutable loader buffers are
compared with independent snapshots. The claim API also captures its final
configuration digest as an immutable string before a reload can reuse actor IDs.

Failure returns no prepared bytes and attempts to stop only the supplied claim
token through an independent three-second cleanup context. A wrong token cannot
stop its owner. If cleanup cannot reach the database, the claim may remain
recorded; this operation never renews or reopens it. Cancellation has the same
bounded cleanup behavior.

Accessors return byte copies. The prepared assignment preserves the signed
command expiry; it does not extend the claim lease or grant a new execution
capability. Preparation success leaves the durable lifecycle in `claimed`.

## Controller responsibilities that remain

Prepared data is not permission to start an executor. Before external effects,
the controller must persist unique lifecycle ownership, revalidate current
claim/authority/admission, create and bind the exact isolated Pod, and reconcile
uncertain creation, expiry and termination. There is no distributed lock across
GitHub, configuration files, PostgreSQL and Kubernetes. A later change can occur
after any observation, so each later effect needs its own boundary check.

Model budget allocation/dispatch, candidate extraction after writer termination,
independent verification, publishing, required review, provenance, release and
deployment remain separate gates. Supervising sessions cannot replace those
transitions. Live work still enters through issues only.

## Validation

PostgreSQL tests exercise derived fields, immutable accessors, wrong-token
isolation, cancellation, and changes to generation, profile, A-Dev, configuration,
policy, lease, source and claim state while source I/O is in progress. Three
regressions mutate shared key/grant/actor buffers on the final reload; all failed
before the independent-copy fix and pass afterward. A related claim regression
proves that mutating the actor slice on its last reload cannot return a claim.

An explicit optional fixture sets `AI_SDLC_TEST_HOMEDIR_PREPARATION=1` with a
disposable `AI_SDLC_TEST_DATABASE_URL`. It uses synthetic signed issue/admission
rows and actual public acquisition of Homedir commit
`0622cdaeb59a11a27bd5f3d7e18183c401a5e412`, with the packaged reviewed A-Dev profile.
The 1,158-file result must match independently verified canonical tree
`a392d4d3be50cc89cb1cc324fb736952fe9e86b47e5d131912163d363c147277`.
This is a composed preparation fixture, not live issue admission, a generated
software improvement or autonomous E2E delivery.
