# Independent review controller

`internal/reviewflow.Runtime` consumes passed behavior verifications and its own
queued reviews. It has no external claim or advance endpoint. This component is
not yet wired into an installed profile and does not publish, merge or release.

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
its reason and emits no approval. Proof commit verifies exact journal/behavior
bindings, five completed effects, identical imported/exported candidate trees,
all handed-off model calls and the model-evidence digest. It grants no publication
capability. The next stage must revalidate this evidence before signing a
publication command.

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

Scenarios cover `approve`, `request_changes`, `mutation` `revoke-model`, `revoke-config` and `lost-config-reply`.
The opt-in `real-model` scenario starts its own local Codex CLI completion adapter;
it does not call the installed runtime gateway. It accepts either a substantiated
approval or request for changes, never a policy/transport failure. The adapter
and disposable namespaces must be retired before the tool reports success.

`.github/workflows/review-controller.yml` runs the six synthetic fault scenarios
on a separate ephemeral runner. This keeps their public source acquisition and
execution budget separate from the longer installer conformance job. Actions
remain quality validation only; they do not process the product backlog.
