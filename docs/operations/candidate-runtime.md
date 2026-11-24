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
./install.sh local ... --generation-seconds 900 --generation-steps 32
```

`--generation-seconds` (600-900, default 600) sets the profile's `maxSeconds`,
and `--generation-steps` (24-32, default 24) sets its `maxSteps`. A local
installation configuration can record the same choice as `generationSeconds`
and `generationSteps`. The values are written into the reviewed execution
configuration, so they are part of the profile digest that every signed
command binds. Without the options, the configuration, its digest, the chart
render and every compiled workload are byte-identical to earlier releases.

The provider-call ledger follows the model path:

- With the bundled Codex adapter, the allocation stays at 16 calls and its byte
  quotas at any step budget. Steps count tools, and one model response can
  request several, so 16 calls do not cap a 32-step run at 16 steps. Raising the
  Codex provider quota is a separate reviewed decision, not a side effect of
  this option.
- With a [model upstream](model-gateway-adapters.md), the allocation already
  derives from the steps: one call per step, with the response limit reserved
  per call (32 calls and 8 MiB at 32 steps).

The budget fits the existing timing envelope. The lifecycle claim is
`maxSeconds` plus a fixed 120 seconds for the executor Pod's setup, source
transfer and candidate capture, which is 1,020 seconds at 900 and matches the
executor Pod's `activeDeadlineSeconds`. The signed execution command lived 20
minutes, sized for 600 seconds: 1,200 - 720 leaves 480 seconds for queueing,
preparation and the verification handoff, which inherits the command's expiry.
At 900 seconds the same 20 minutes would leave only 180 seconds, so the command
lifetime, and the worker's bound on one poll, grow by the seconds above 600:
25 minutes at 900 (1,500 - 1,020 = 480). The default budget keeps exactly 20
minutes. A [review repair](review-repair.md) uses the original command's
remaining time, as before. A longer budget also spends more model time on a
failed attempt.

SCC itself receives a shorter `--max-seconds` than the launcher's kill at
`maxSeconds`. SCC checks its time budget only before each model call and never
interrupts one in flight. Its deadline is therefore one call window plus its
15-second provider-wait slack earlier, capped at an eighth of the budget: 525
seconds for the default profile (60 s window), 825 at 900 seconds, and 788 at 900
seconds with a 330-second window. The last call it starts can usually answer
before the kill, and SCC then stops with exit 22 and its summary instead of being
killed with exit 124. See [executor failure diagnostics](executor-failure-diagnostics.md).

### Existing installations

This default applies to newly configured installations. Existing installations
must use a reviewed configuration transition that preserves their history and
original authority. An image-only diagnostic upgrade does not authorize changing
the executor profile or replaying a failed execution. Ordinary candidate or
verification reconciliation reads the installed execution configuration first
and rejects a different tool budget or generation time before changing runtime
resources or credentials. Repeat the recorded `--generation-seconds` and
`--generation-steps` on every reconciliation; omitting them never resets an
installed budget to the default. Missing configuration permits first-time setup; an API error,
malformed configuration or duplicate JSON field does not count as absence.
