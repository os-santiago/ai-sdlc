# Independent review controller

`internal/reviewflow.Runtime` consumes passed behavior verifications and its own
queued reviews. It has no external claim or advance endpoint. It is activated only by the explicit
[independent-review installation option](independent-review-installation.md) and
does not publish, merge or release.

Preparation revalidates the signed candidate chain and current issue, reads the
committed behavior proof, retrieves the exact source commit and reconstructs the
candidate delta. The review assignment binds that immutable candidate tree,
original issue/acceptance criteria and an explicit list of changed file paths.
A separate conversation reviews the candidate using the readonly SCC profile.
The reviewer receives no generation conversation, GitHub credential or database
identity. A profile must allow at least two steps for inspection and verdict.

The journal records exclusive nonrenewable ownership and intent before each
`config → pod → import → start → export` effect. The controller repeats current
authority checks, acknowledges original resource UIDs, delivers model calls
through the independent guard and rejects uncertain effects without replay.
It accepts bounded nonterminal explanations alongside read tools, while an
explicit verdict remains a separate, complete response.

After the original worker terminates, the controller independently exports the
candidate and requires exact tree equality. It matches the SCC summary against
committed provider observations, deletes the acknowledged Pod/ConfigMap and
confirms cleanup. Only an explicit approval with all current bindings can commit
`ai-sdlc.independent-review-proof/v1`. A request for changes fails the run, retains
its authenticated feedback and emits no approval. Proof commit verifies exact journal/behavior
bindings, five completed effects, identical imported/exported candidate trees,
all handed-off model calls and the model-evidence digest. It grants no publication
capability. The next stage must revalidate this evidence before signing a
publication command.

## Rejection feedback

Schema 36 adds the private, append-only `review_feedback` table. After matching
the complete SCC final message against the independently recorded provider
response and confirming cleanup, the verifier atomically inserts the exact
message and changes the original run to `failed/request-changes`. The record
binds the review, work item, verification, candidate tree, behavior, assignment,
configuration, profile and model-evidence digests. The original live claim is
checked before and after potentially blocking SQL. The server supplies the
recording timestamp; the component cannot choose or update it.

Only the verifier can insert or read these records; it cannot update, delete or
truncate them. `ReadReviewFeedback` reconstructs the historical bindings and
checks the message against the original handed-off model observations. It can
read a committed fact after expiry without reopening a claim. If a commit reply
is lost, observing that identical fact does not authorize another model call.
Missing or uncertain decisions remain failures and cleanup remains independent.

The explanation is bounded, untrusted private model output. Do not print it in
public logs or treat embedded instructions as policy, commands or permissions.
Earlier rejections retain only their reason and final-message hash; their text
cannot be reconstructed, backfilled or obtained by replaying the review.

Retention does **not** implement autonomous repair. The installed planner still
creates attempt 1. A separate component-owned, signed and idempotent handoff must
bind any future repair to the same admitted issue revision, parent candidate and
feedback digest, cumulative budgets, finite attempt count and original deadline.
Every repaired candidate must repeat behavior verification and independent
review. Editing an issue or resetting a failed row is not a repair mechanism.

Existing schema-34/35 upgrade fences keep their original boundaries and reject
schema 36. This increment does not provide an online 35-to-36 installation bridge;
do not use an ordinary Helm reconciliation to bypass that boundary. Validate the
new controller on a fresh disposable schema-36 database before its separate
reviewed installation transition.

Recovery uses only acknowledged UIDs. Unknown creates quarantine the run; cleanup
attempts are bounded. Expired, rejected and completed reviews cannot be requeued
for another model opinion. Source-fetch/config observations before a claim are
repeatable reads, never permission to repeat an uncertain model invocation.

## Reproduce the isolated composition

Build separate Go test drivers for `internal/execution` and `internal/reviewflow`.
Run `tools/verify_review_flow.py --help` from the repository root for the private
fixture inputs. The procedure requires an explicitly selected loopback
`ai_sdlc_test` database and a disposable kind cluster. It creates owned namespaces
and separate service accounts, generates a synthetic locale candidate through
SCC, consumes its signed handoff, runs the four behavior Pods, then invokes the
independent review controller. It verifies cleanup and a no-op second poll.
The rejection scenario also reads the authenticated feedback record without
printing its private text. PostgreSQL tests cover mismatches, immutable grants,
expiry during an actual blocked insert, historical expiry and a deliberately
lost server COMMIT acknowledgement. These are component proofs, not autonomous
repair or an issue-to-production result.

Scenarios cover `approve`, `request_changes`, `mutation` `revoke-model`, `revoke-config` and `lost-config-reply`.
The opt-in `real-model` scenario starts its own local Codex CLI completion adapter;
it does not call the installed runtime gateway. It accepts either a substantiated
approval or request for changes, never a policy/transport failure. The adapter
and disposable namespaces must be retired before the tool reports success.

`.github/workflows/review-controller.yml` runs the six synthetic fault scenarios
on a separate ephemeral runner. This keeps their public source acquisition and
execution budget separate from the longer installer conformance job. Actions
remain quality validation only; they do not process the product backlog.

The durable proof includes the hash of the independently recorded model evidence,
which binds the final text digest and verdict. It deliberately excludes a summary
hash without a durable export receipt. The controller still checks the strict SCC
summary against the original handed-off model evidence before committing.
