# Independent reviewer model delivery

`modelbroker.ReviewDispatcher` connects the separate review model journal to the
bounded provider and original-Pod mailbox capabilities. It is an inactive
component: no installed role gains journal access, no controller invokes it,
and it does not mark a review passed or publish a PR.

A trusted controller must supply `ReviewAuthority`, rechecking the original
signed verification, current issue/policy, immutable candidate and behavior
proof, configuration, profile and claim. Each request is bound to the review
profile, original Pod/start acknowledgement and next sequence. Claim expiry,
mailbox expiry and provider timeouts bound delivery. The generation ledger is
never reused.

Each call requires a fresh committed journal entry. Provider invocation occurs
once. The broker inspects the exact provider bytes before recording completion
and before handing them to SCC. It accepts one complete assistant choice:

- Up to four `read_file`, `list_dir`, `search_text` or `code_query` calls, with
  explicit bounded arguments and relative workspace paths. Shell, writes, Git
  mutations, absolute/parent paths, ambiguous arguments and regex search fail.
- An explicit terminal `VERDICT: approve` or `VERDICT: request_changes`, only
  after an inspection response. Implicit, duplicate or truncated verdicts fail.

The model is offered only these four tools: on installations with
`review.narrowModelTools`, the verifier's review gateway removes every other SCC
tool definition before the upstream call (see
[Review tool allowlist](model-gateway-adapters.md#review-tool-allowlist)). The
journal's request hash still covers the exact request the broker sent to the
gateway, with SCC's full tool list; the narrowed upstream payload is not
recorded. This policy is unchanged and still rejects a call to any other tool.

## Final turn: no tool calls

The signed reviewer profile's `maxSteps` (at most 8) bounds the conversation.
On the last permitted turn (`sequence == maxSteps`, from turn 2), the broker
sends the provider the request with `tool_choice: "none"`, followed by one
fixed user message, `ReviewFinalTurnNote`: "This is your last turn: give the
final verdict now in the required format." The tool definitions stay, because
the history holds tool calls and results, and the Messages API (the
`anthropic` adapter) refuses tool blocks without tool definitions; that adapter
maps `"none"` to `{"type": "none"}`. A request without tools carries no
`tool_choice`. Earlier turns are sent unchanged. The broker decides this from
the envelope sequence it has already bound and the signed profile, never from
the model or the worker.

- **Binding.** The request is bound (envelope digest, attempt, Pod, model and
  token limits) before the change, and the changed request is checked again
  against the same limits. The journal's request hash and input bytes record
  the payload actually sent; the mailbox reply keeps the worker's own request
  digest, so SCC still matches the reply to its request. The journal does not
  keep the worker's digest of the final turn (no schema change): it is the
  reply envelope's `requestSHA256`, and on every earlier turn the two are
  equal.
- **Read-only.** Only `tool_choice` changes, and the review gateway's tool
  allowlist still narrows the definitions. The response policy refuses any
  tool call on the last turn outright (`model-response-policy`), on top of the
  usual four-tool check.
- **Verdict.** The answer is parsed by the same policy; it must still be one
  complete choice ending in exactly one verdict line.

The OpenAI-compatible adapter passes `tool_choice` to the upstream unchanged.
The llama.cpp server honours `"none"`: it builds no tool-call grammar or
parser, so a tool call written anyway stays plain text and fails the verdict
check (`model-response-policy`).

A request beyond `maxSteps` binds but is refused before any journal read,
reservation or provider call, as `model-request-turn-budget` rather than
`model-request-binding` (see
[review request stops](executor-failure-diagnostics.md#review-request-stops)).
Battery 31 (issue 227) spent all 8 turns on one read each, and its ninth
request was refused.

Independent observations retain only tool counts and final-message SHA-256 plus
verdict. The journal binds the exact request/response hashes and handoff. A final
verdict ends the conversation; a rejected answer cannot be followed by another
model request seeking approval. Every failed/uncertain delivery stops its parent,
including a lost successful handoff commit. No call is replayed or refunded.

`MatchReviewSummary` compares SCC's advisory summary with these trusted
observations: iterations, inspection count, exact final-message hash and verdict.
The function does not establish current authorization or semantic correctness.
A future controller must also validate unchanged candidate source, original
resource UIDs, confirmed cleanup and current behavior evidence before creating a
review proof and signed publication command.

## Validation and limits

The real PostgreSQL/HTTP integration test covers successful two-call review,
mutation/implicit-response rejection, authority revocation, failed provider,
failed/mutated mailbox delivery and deliberately lost reservation, completion
and handoff COMMIT replies. Replay is rejected and failed parents cannot yield
review evidence. Separate real SCC/gVisor tests compare actual summaries against
provider observations for approve/request-changes and reject implicit/mutation
responses. Negative transport fixtures deliberately deliver synthetic unsafe
responses to exercise SCC too; the broker test confirms production delivery
rejects those responses before publishing them.

These are isolated component tests with synthetic issues/model responses. They
do not advance the live Homedir workflow or demonstrate an autonomous E2E.
