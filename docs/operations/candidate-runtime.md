# Candidate runtime

The opt-in `candidate` profile connects signed issue intake to isolated SCC
execution and durable, **unvalidated** candidate capture. It does not publish a
PR, merge, build a release or deploy Homedir. The default remains `maintenance`;
the existing Homedir example remains `shadow` until the candidate rollout is
validated against published images.

## Install

Use Linux amd64, the same kind/Podman prerequisites as the
[local installation](../installation.md), a working Codex authentication file,
and a reviewed image lock containing the candidate implementation. The installer
requires matching publication evidence for both worker and model-mailbox images.
A source checkout with an older image lock does not enable the new runtime.

```bash
./install.sh plan --config examples/homedir-candidate.local-install.json
./install.sh local --config examples/homedir-candidate.local-install.json \
  --github-auth gh-cli --codex-auth-file "$HOME/.codex/auth.json"
```

The installer configures gVisor, an offline executor namespace, least-privilege
controller RBAC, immutable image identities and bounded model budgets. It imports
credentials into owned Kubernetes Secrets; credentials never enter Helm values,
Git, candidate artifacts or executor Pods. Reapplying preserves the existing
identities and model Secret. To deliberately replace model authentication:

```bash
./install.sh local --config examples/homedir-candidate.local-install.json \
  --codex-auth-file "$HOME/.codex/auth.json" --refresh-model-credential
```

The Secret's public resource version changes the worker Pod template. A new Pod
receives a new model home; restarting only a container preserves CLI refreshes
within the current Pod. Credential rotation can interrupt an in-flight attempt;
that attempt is terminalized rather than replayed.

## Work and boundaries

Only eligible issues with the configured `ai-sdlc:shadow` trigger label enter the
runtime. The label name is retained for compatibility; the installation profile
controls whether accepted low-risk plans may execute. A new authority revision
causes intake to record current issue eligibility again. The orchestrator emits
an expiring signed command bound to repository, issue generation, admission,
source SHA and profile digest. The worker rechecks current observations and
policy before every effect. There is no public run/claim/advance endpoint.

The worker Pod contains a controller and a completion-only Codex gateway, using
the same reviewed worker image. Only the gateway mounts model authentication;
only the controller mounts database, component and Kubernetes credentials. The
separate gVisor executor Pod contains SCC and the model mailbox, with no network
access or service account token. The gateway cannot run workspace tools; SCC
executes its requested tools inside the offline executor.

The installation permits only `admit-issue`, `plan-admission` and `execute-work`
authority. It gives the worker narrow database columns and executor namespace
Pod/ConfigMap operations. No verifier, publisher, merge or deploy authority is
introduced by this profile. Candidate capture is never software acceptance.

## Failure recovery

Claims are nonrenewable and cannot be adopted by another process. On expiry or
a stopped controller, the reconciler closes model budgets without refund,
records uncertain provider outcomes, and fails work without a captured candidate.
It deletes only resource UIDs acknowledged in the durable lifecycle ledger.

Cleanup uses at most three bounded attempts. Pending cleanup blocks new claims,
including the cooldown between attempts. A lost create acknowledgement is
recorded as uncertain: the controller does not discover/adopt a resource by
name. Uncertain or exhausted cleanup keeps readiness false and requires operator
investigation. Pod active deadlines and namespace quotas bound abandoned
resources. Recovery does not replay a provider call or generated execution.

Returning to maintenance freezes authority and revokes execution database grants.
Finish recovery before downgrading if there are outstanding executor resources;
a maintenance installation does not have cleanup privileges.

## Validation and remaining gates

`tools/verify_candidate_runtime.py` runs `Runtime.PollOnce` with real Kubernetes,
SCC, PostgreSQL and a public pinned Homedir revision. Its issue authority and
model replies are synthetic. It checks candidate contents, no replay on a second
poll and removal of acknowledged resources. The `crash-after-model-start`
scenario exits the controller process abruptly, starts a fresh process, waits
for actual claim expiry and checks terminal budgets, ambiguous model outcome
and UID-based resource cleanup. These fixtures refuse an existing executor
namespace and require an explicitly disposable database.

The gateway also requires a separate real-model container validation. Neither
fixture nor gateway success establishes autonomous issue-to-production delivery.
The [independent verification boundary](independent-verification.md) has an isolated
recipe fixture but is not yet connected to live candidates. Independent
verification, authenticated audit consumption, PR publication,
current merge gates, Konflux release policy and test-production promotion remain
separate outerloop work. See the [architecture](../innerloop-outerloop.md).

The [recorded validation](../evidence/2026-11-15-candidate-runtime.json) separates
real infrastructure/model observations from synthetic issue/model fixtures and
lists the unverified delivery boundaries.

## Diagnose a stopped attempt

The worker logs one allowlisted stage with `candidate reconciliation stopped
safely`. Broker categories distinguish authorization, reservation, provider,
completion commit, publication and acknowledgement. Publication additionally
identifies envelope/encoding checks, before/after Pod observations, helper
transport/exit and receipt validation. These are operational categories, not
proof of a particular network, model or application root cause.

The diagnostic deliberately excludes exception text, request/response bodies,
source code and credentials. A completed HTTP response without a confirmed
handoff remains ambiguous; a stage label never permits retry or adoption.
Inspect the ledger and resource cleanup before supplying a new issue revision.
The [first live Homedir candidate installation](../evidence/2026-11-15-homedir-candidate-installation.json)
confirmed two model handoffs and then stopped with an unconfirmed third response.
Its resources were removed and no candidate or release was produced. The older
image had no failure-stage diagnostic, so its precise cause remains unknown.


A cleanup failure is reported separately as `cleanupPending=true`. The primary
fixed diagnostic stage is retained when both model handoff and cleanup fail;
raw helper/provider exceptions are still excluded. A successful cleanup recovery
cannot turn an ambiguous model call into a completed handoff or replay it.

### Opt-in controller check with the real completion adapter

`tools/verify_real_controller.py` exercises `RunController` with actual Codex CLI
completions, SCC, Kubernetes and PostgreSQL in an isolated fixture. Build
`go test -c -o /private/controller.test ./internal/execution`, select a disposable
`ai_sdlc_test` database through `AI_SDLC_TEST_DATABASE_URL`, and pass the same
`--kubectl`, `--kubeconfig`, `--worker-image`, `--mailbox-image`, `--test-binary`,
`--source-repository` and `--source-sha` arguments as `verify_lifecycle_model.py`.
Use an isolated fixture cluster; the helper resets only its explicitly selected
local fixture database. It starts its own loopback completion adapter using the
existing host Codex authentication. Do not point it at the installed model gateway.

The bounded task creates one prescribed text file. The helper compares the entire
candidate and persisted delta independently, verifies durable handoffs and confirms
UID-bound resource cleanup. This is synthetic work and authority; a pass does not
prove live issue intake, independent verification, PR publication or production.
The first real-controller observation used five model calls and removed all its
resources. It does not establish the cause of the separate Homedir #1581 failure.

### Tool steps and provider calls are different budgets

The installed generator profile uses 24 SCC tool steps, 600 seconds and 4,096
requested output tokens per response by default. Its separate durable model
allocation still permits at most 16 calls, 2 MiB cumulative input, 4 MiB
cumulative response reservations and 256 KiB per response. Increasing tool steps
does not increase these provider limits. The independent reviewer retains its
existing profile.

SCC counts individual tools, including tools in the same response, and checks its
step limit between batches. A 16-step run can therefore stop after six model
responses containing 18 tool calls. This is a cooperative execution budget,
not a strict per-operation security quota. Exit 22 remains a failure; partial
workspace changes cannot be promoted as a successful candidate.

The offline regression runs the actual SCC process from an immutable executor
image against a deterministic loopback provider, with no credentials or external
network. It requires exit 22 with 16 steps and successful fixture completion with
24 steps, checks separate call/byte bounds, and confirms container cleanup:

```bash
python3 tools/verify_executor_tool_budget.py --provider podman \
  --image quay.io/os-santiago/ai-sdlc@sha256:EXECUTOR_DIGEST
```

Installer CI runs this check on its built executor. The
[measured result](../evidence/2026-11-17-scc-tool-budget.json) reproduces the budget
mechanism, not Saferus delivery or real-model success under the new profile.
An actual Saferus attempt retained exit 22 after six HTTP 200 completions, before
candidate creation. Its original 600-second bound was not reached and its fixed
environment supplied no total-token limit; tool-budget exhaustion is inferred
from those observations, not a budget dimension retained by the controller.

### Selecting the generation budget

A slow model endpoint can need more time than the default 600 seconds. A local
model on an integrated GPU, for example, generates about 20-30 tokens per second
and reads the prompt at about 250-500 tokens per second, so a single call can
take minutes. The candidate and verification installations accept two options:

```bash
./install.sh local ... --model-upstream upstream.json --generation-seconds 900 --generation-steps 32
```

`--generation-seconds` (600-900, default 600) sets the profile's `maxSeconds`,
and `--generation-steps` (24-32, default 24) sets its `maxSteps`. A local
installation configuration can record the same choice as `generationSeconds`
and `generationSteps`. The values are written into the reviewed execution
configuration, so they are part of the profile digest that every signed
command binds. Without the options, the configuration, its digest, the chart
render and every compiled workload are byte-identical to earlier releases.

The provider-call ledger follows the model path, and so does the step range:

- With a [model upstream](model-gateway-adapters.md), the allocation derives
  from the steps: one call per step, with the response limit reserved per call
  (32 calls and 8 MiB at 32 steps). Any step budget from 24 to 32 is accepted.
- With the bundled Codex adapter, the allocation stays at its reviewed 16 calls
  and byte quotas. Reaching 32 tool steps within 16 calls needs two or more
  tools per response on average, so the call ledger, not the step budget, would
  usually end the run: a larger step budget would only look larger. The
  installer therefore refuses `--generation-steps` above 24 without
  `--model-upstream`. `--generation-seconds` is accepted on both paths. Raising
  the Codex provider quota is a separate reviewed decision.

#### Answer-token bound

`--generation-answer-tokens` (4096-8192, default 4096), or
`generationAnswerTokens` in a local installation configuration, sets the
profile's `maxTokens`: the output tokens the generator may request for one
model answer. It is recorded in the reviewed execution configuration like the
other budget options. With the default the configuration bytes and digest are
unchanged.

The bound should fit a full rewrite of the largest file publication accepts.
A model whose `edit_file` patches are refused falls back to `write_file` with
the whole file. An answer that reaches the bound is truncated, the gateway
refuses it as `truncated`, and the attempt stops. Publication accepts files up
to the manifest's `maxFileBytes`, typically 32,768 bytes. In one evaluation,
rewriting an 8.8 KB Go file took 2,781 completion tokens (about 3.2 bytes per
token). Rewriting the next file, its test, reached 4,096 and was refused. At
that rate 4,096 tokens hold a rewrite of about 13 KB and 8,192 tokens about
26 KB. 8192 is the runtime's own cap: the executor profile, the model mailbox
and the broker request limits all refuse more. So the maximum fits most
publishable files but not every file near 32,768 bytes. One tool call's
arguments are also limited to 32 KiB.

Everything else derives from the recorded `maxTokens`: the mailbox's
`--max-tokens` cap in the executor Pod, the broker's request limits, and SCC's
`maxTokens` setting. The independent reviewer keeps 4096, since it answers
with a short verdict. The call ledger reserves the 256 KiB response limit for
every call, whatever the bound. That covers 8192 tokens at up to 32 bytes per
token, and 32 calls stay within the 8 MiB output quota, so no byte quota
changes.

With a model upstream, the worker gateway's startup benchmark uses the bound as
its answer envelope (`modelGateway.benchmark.completionTokens`). The benchmark
only admits the model: a model whose measured speed cannot write the longest
answer the profile admits within the upstream `seconds` ceiling fails its
benchmark and never reports ready. Each real call may still run up to the
ceiling (bounded by the broker's call window), so an admitted model can still
time out on an unusually slow answer. Raise `seconds` (and `callSeconds`) in
the upstream document, or keep a lower bound. A longer answer also takes more
of the generation time budget.

Values above 4096 require `--model-upstream`. The bundled Codex adapter checks
`max_tokens` (at most 8192) but does not pass it to the Codex CLI, so a larger
bound would change the reviewed configuration without changing any answer.

#### Timing envelope

All bounds below keep their original values at the default budget and grow
second for second with the seconds above 600:

| Bound | Default (600 s) | At 900 s | Margin it keeps |
| --- | --- | --- | --- |
| Lifecycle claim: `maxSeconds` + 120 s for Pod setup, source transfer and candidate capture | 720 s | 1,020 s | 120 s beyond generation |
| Executor Pod `activeDeadlineSeconds` | 1,020 s | 1,320 s | 300 s beyond the claim |
| Launcher's absolute bound from process start | 980 s | 1,280 s | 380 s beyond `maxSeconds`; ends 40 s before the Pod deadline |
| Signed execution command and the worker's bound on one poll | 20 min | 25 min | 480 s beyond the claim for queueing, preparation and the verification handoff |

With a [generation toolchain](project-manifest-installation.md#optional-a-toolchain-for-the-generator),
the executor Pod first pulls the pinned verification image and copies the
toolchain. That start adds 180 s to the readiness budget (120 s to 300 s), to
the lifecycle claim and to the Pod deadline (1,200 s at the default budget,
1,500 s at 900 s), so the claim and the Pod keep their margins. The launcher's
bound starts with the worker, after the copy, and does not change. The
signed command and the worker's poll bound grow by the same 180 s (23 minutes
at the default budget, 28 minutes at 900 s), so they keep 480 s beyond the
longer claim, well under the one-hour bound every command verifier enforces.
Without a toolchain every bound is unchanged.

Before this option, the Pod deadline (1,020 s) and the launcher's bound (980 s)
were fixed. Left fixed at 900 s, the claim would have reached the Pod deadline
with no margin, and the launcher would have kept only 80 s for its 60-second
start wait, 20-second baseline and source verification, so a slow start would
have killed a run that the claim still allowed. Growing both keeps the margins
of the default. The claim remains the binding limit: it covers setup, the whole
generation and capture, and the worker stops observing at its end, so the
longer Pod and launcher bounds only backstop a stalled Pod and never extend
the time the runtime grants. The command lifetime stays far below its one-hour
verification limit. A [review repair](review-repair.md) uses the original
command's remaining time, as before. A longer budget also spends more model
time on a failed attempt.

#### SCC ends before the launcher's kill

The launcher kills SCC at `maxSeconds`. SCC checks its own time budget only
before each model call and never interrupts one in flight, so with the same
deadline a call that is still running at `maxSeconds` is killed with exit 124.
Above the default, SCC therefore receives a shorter `--max-seconds`: one call
window plus its 15-second provider-wait slack earlier, capped at an eighth of
the budget and taken only from the seconds above 600. The last call it starts
can usually answer before the kill, and SCC then stops with exit 22 and its
summary. Examples: 600 s (default) keeps exactly 600, as before; 650 s gives
600; 700 s gives 625; 900 s gives 825; and 900 s with a 330-second call window
gives 788. SCC never receives less than the 600 seconds it had before. To make
time-outs end gracefully, select a budget at least one call window plus 15
seconds above 600. See [executor failure diagnostics](executor-failure-diagnostics.md).

### Existing installations

This default applies to newly configured installations. Existing installations
must use a reviewed configuration transition that preserves their history and
original authority. An image-only diagnostic upgrade does not authorize changing
the executor profile or replaying a failed execution. Ordinary candidate or
verification reconciliation reads the installed execution configuration first
and rejects a different tool budget or generation time before changing runtime
resources or credentials. It refuses a different answer-token bound in the same
way. Repeat the recorded `--generation-seconds`, `--generation-steps` and
`--generation-answer-tokens` on every reconciliation; omitting them never resets an
installed budget to the default. No runtime transition changes a selected
generation budget yet: changing it on an existing installation requires a
reinstallation. The closed 16-to-24-step transition in
[image-lock upgrades](image-lock-upgrades.md) applies only to its original
source and carries the recorded generation time and answer-token bound unchanged. Missing configuration permits first-time setup; an API error,
malformed configuration or duplicate JSON field does not count as absence.
