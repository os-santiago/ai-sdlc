# Executor failure diagnostics

A nonzero exit from the original executor stops the attempt before candidate
export, with one exception: the [step-budget exit](#step-budget-exit-25), whose
change is exported for independent verification and review. The controller
reobserves that same Pod and container instance, including the termination
container ID and zero restarts. Within the original live claim it appends the
bounded exit code to `execution_failures`, then cleans up.

The worker can insert identity/code fields and read this table. It cannot supply
the database observation time, update or delete a diagnostic. Identical repeated
observations retain the original timestamp; contradictory observations reject.
No failure observation grants a candidate, retry, claim renewal or release.
One observation is also a precondition: a candidate after the
[step-budget exit](#step-budget-exit-25) is captured only when its code-25 row
exists for the same request and Pod. The row still grants nothing by itself;
the comment in migration 0034 predates this and migrations are not edited.

`workerExitCode` is the observed launcher exit code. The launcher can collapse
several SCC errors to code 1, so this does not always identify the underlying SCC
cause. Logs expose only this bounded code and `failureRecorded`; source text,
prompts, model responses and arbitrary process output are not retained here.
If observation or persistence fails, cleanup still runs and the attempt remains
stopped. A false `failureRecorded` must never be treated as durable evidence.

`workerExitCode=0` in a stop line means that the stop was not a worker exit,
not that the worker exited 0. When the worker has already exited while a model
response waits for publication, the stop names that exit code instead, for
example `model-publication-worker-exit-124`. Code 124 is the launcher ending
the run at the profile's `maxSeconds` while the call was still in flight: the
generation ran out of time, not a transport fault. The publication still stops
the call as uncertain, as before.

When the installed `maxSeconds` is above the 600-second default, SCC receives a
shorter time budget than the launcher's kill: one call window plus 15 seconds
earlier, at most an eighth of `maxSeconds`, and taken only from the seconds above
600 (825 of 900 seconds with the default window). SCC checks that budget before
each model call, so a run that runs out of time then normally ends as
`worker-exit workerExitCode=22`, with no call in flight, instead of exit 124. At
the default 600 seconds SCC keeps the full 600, exactly as before, so a
time-out with a call in flight still ends with 124. Code 124 also remains
possible above the default when the last call or tool outlasts the margin, for
example a slow model whose call window is longer than an eighth of the budget;
the stop line above still names it.

#### Step-budget exit 25

SCC ends a generation with exit 22 (`BUDGET_EXCEEDED`) when it runs out of its
time budget or its tool-step budget. The launcher tells them apart from facts
it measures itself, never from SCC's output, summary or audit log, which the
generated tools can write:

- The pinned SCC checks its time budget before its step budget, before every
  model call, on the wall clock (`Date.now()`), measured from its own start,
  which is later than the launcher's clock starts. The launcher configures no
  token budget, so time and steps are the only budgets that end a generation
  run.
- The launcher reads SCC's run on its monotonic clock and on the wall clock.
  The monotonic clock stops while a host or VM is suspended, and the wall
  clock can be stepped (NTP), so it compares the larger reading.
- If SCC exits 22 while both readings are strictly below its time budget
  (`--max-seconds`, above), the time budget cannot have ended it: the launcher
  exits **25**, the step budget.
- At or after that point the cause is ambiguous, because a step budget reached
  at the deadline looks the same, so the launcher keeps **22**. A time-out
  therefore stays exit 22 and stops before export, as before. Reviewer runs
  always keep 22.

A step budget ending is a generator efficiency signal, not a safety signal: in
the evaluation batteries the generator had usually made its change and seen the
manifest test pass before it spent its remaining steps re-running variants. On
exit 25 the controller first retains the observation in `execution_failures`
(code 25). Only once that row exists does it run the same export and capture as
a clean exit, with every bound (workspace bundle checks, size and change
limits, the nonempty reconstructable delta, completed model handoffs). The
store accepts a nonzero export receipt only for code 25 and only with that row
for the same Pod, so every such candidate carries its durable marker. If the
workspace holds no change (its tree equals the source tree, or the rejected
candidate's tree for a repair), the attempt stops as `worker-exit
workerExitCode=25 failureRecorded=true`. If the observation cannot be retained,
it stops before export.

A captured step-budget candidate is logged as `candidate reconciliation:
candidate-captured generationEnded=step-budget`, and the durable records are the
`execution_failures` row with code 25 next to an `execution_candidates` row for
the same request, and the export receipt (`execution_effects` kind `export`)
with `exitCode` 25. Verification, review, publication and merge do not read
either: the candidate is the same unvalidated data as any other, and it must
pass the repository tests in the pinned offline image, the test that must fail
on the current code and the strict independent review. It is never stronger
evidence than a clean exit, and a clean exit is not evidence of completeness
either.

This is the default for every installation and changes no configuration: the
execution configuration, its digest and every compiled workload are unchanged.
It applies on the bundled agent-CLI path as on a model upstream; there the
16-call ledger usually ends a run first, as a provider refusal rather than an
exit 22.

#### Telling a time-out from a step-budget stop

Exit 25 is a step-budget stop. Exit 22 is now a time-out, or a step-budget stop
reached at SCC's time budget. Before this launcher, code 22 covered both
budgets; for those older records, or to look into an exit 22, use the durable
records:

- **Elapsed time.** Compare `execution_failures.observed_at` with the attempt's
  `execution_lifecycles.created_at`. A time-out ends at or after SCC's
  `--max-seconds` (above), plus the setup before SCC starts. A step-budget stop
  usually ends well before it.
- **Model calls.** Count the attempt's `model_calls`. With a model upstream,
  where one call is allowed per step, a step-budget stop typically uses close to
  `maxSteps` calls; a time-out uses fewer calls of longer duration.
- **SCC's audit and summary.** `run/summary.json` reports `exitReason`
  `budget_exceeded` for both, and SCC's audit log in `run/audit.jsonl` shows
  which budget ran out (the [isolated tool-budget evidence](../evidence/2026-11-17-scc-tool-budget.json)
  checked a step-budget marker). These files exist only while the executor
  workspace does; cleanup removes them, so they are not durable evidence.

The first managed Saferus attempt on 2026-11-17 stopped before this capability
existed. Its seven successful model HTTP responses do not explain its nonzero
executor exit, and cleanup removed the transient diagnostic files. This change
cannot reconstruct that historical cause. Installing a migration over a runtime
with workflow history requires a separately validated upgrade/recovery path;
the empty-install preparation and initial activation commands do not provide it.

When publishing a model response into the executor pod fails because the
mailbox helper exits nonzero, the stop names the helper's bounded exit code,
for example `model-publication-helper-exit-124`:

| Code | Meaning |
|---|---|
| 124 | The helper's own 5-second deadline ended, or it received a termination signal |
| 3 | The response staging file vanished before it could be linked into place |
| 1 | Any other refusal: the request cannot be read (missing or still pending), a response already exists, the response is malformed or mismatched, or the helper's environment (Pod UID, bridge directory) is invalid |

If the controller's own exec deadline ends first, the stop is
`model-publication-helper-transport` instead.

A code 1 after a call that took about 300 s usually means SCC had already
abandoned the request, so the mailbox removed it before the broker published.
Before the mailbox kept long calls active, Node's `fetch` gave up after 300 s
without response headers; see
[calls longer than 300 seconds](../security/offline-model-mailbox.md#calls-longer-than-300-seconds).

As with every publication failure, the call stays uncertain and is never
retried by this diagnostic.

## Model authorization stops

The broker checks current execution authority before it reserves a model call,
before it sends the call to the provider, and again before it publishes the
response into the executor pod. A refusal names the check and the step that
refused, for example `model-authorization-publication-snapshot`. The
`reservation`, `dispatch` and `publication` checks share these issue-evidence
steps:

| Step | Meaning |
|---|---|
| `snapshot` | The issue evidence is missing or changed: a new issue revision or labels, a withdrawn or changed admission or plan, other criteria, or another authority revision |
| `snapshot-stale` | The issue evidence is unchanged, but the intake poll watermark or the issue's observation is older than the evidence window (twice `pollSeconds`, at most 300 s) |

Intake records the poll watermark only after a complete scan. One incomplete
scan (`intake scan incomplete; cursor retained`: a GitHub error, rate limit,
an issue that changed while it was read, or the 90-second scan bound) keeps
the previous watermark. The next scan starts after a backoff of 2^failures ×
30 seconds (60 seconds after one failure, whatever `pollSeconds` is; a
GitHub `Retry-After` can make it longer), and the watermark is recorded only
when that scan completes. With the installed 30-second poll interval the
60-second window can end before then, even though nothing about the issue
changed. Battery 29 lost a 16-call attempt to this at the publication check
(issue 187).

### Bounded wait for stale evidence

On `snapshot-stale` the authorization is checked again instead of refusing at
once. The bound of one wait is `max(pollSeconds, 60 s) + 30 s`, at most
2 minutes: 90 seconds with the installed configuration. It is sized for one
missed scan: the 60-second retry plus up to 30 seconds for that scan to
complete. It does not cover more: a slower retry scan, a second failure
(120-second backoff), a longer `Retry-After`, or with `pollSeconds` above 90
the 2-minute cap, can end the wait before a complete scan, and the stop is
then `snapshot-stale`.

| Where | Checks every | Also ends | Stop when the wait ends |
|---|---|---|---|
| Broker, `reservation` check | 3 s | 35 s plus 15 s before the mailbox request expires | `model-authorization-reservation-snapshot-stale` |
| Broker, `dispatch` check | 3 s | 35 s before the call deadline (the call's or mailbox's expiry less 15 s) | `model-authorization-dispatch-snapshot-stale` |
| Broker, `publication` check | 3 s | 10 s before the mailbox request expires | `model-authorization-publication-snapshot-stale` |
| Generation loop, between model calls | 100 ms | the claim | `execution-authority-snapshot-stale` |

Every wait also ends at the claim and model scope expiry. Before reservation
and dispatch the broker keeps 30 seconds for the provider, the provider's
default completion timeout and the smallest one an installation may select,
plus 5 seconds for the last authorization check (its own bound): a wait that
would leave less refuses instead of reserving or claiming a call that cannot
be answered in time. A check that passes without waiting is not affected.
While the generation loop waits it neither observes the worker nor reads or
delivers model requests.

A request still waits in the mailbox while the generation loop waits, so it
can age. The broker therefore refuses a request with less than those 35
seconds left before its mailbox deadline (its expiry less 15 s) as
`model-provider-time`, after the reservation check and before anything is
reserved: no call is spent, and the stop is not a provider failure. The
30 seconds are fixed, not the gateway's configured completion timeout: that
timeout is a maximum, a fast model answers well within it, and using it as a
minimum would refuse most calls. The 30 seconds are not a guarantee that a
slow model answers in time; the call keeps its own deadline as before. The
database reservation and claim between the check and the provider call take
part of the 5-second margin.

Each wait starts at its own first stale refusal, so on one call the waits can
add up: at most 90 s each at reservation, dispatch and publication, 270 s in
all with the installed configuration, still inside the call's mailbox window
(the reservation and dispatch waits end at least 50 s and 35 s before their
deadlines, the publication wait 10 s before the mailbox expires). The
generation loop can wait another 90 s per stale period between calls. All
waits spend the run's own time budget; none extends the claim, scope, call or
mailbox deadline.

Every repeated check is a complete authorization. A stale result still runs
every other check (`reload`, `record`, `claim-read`, `prepared`, `binding`),
so a stopped lifecycle, an expired claim, a changed placement or a changed
snapshot refuses at once by its own step, even while the evidence is also
stale. Stale evidence never authorizes a step, and the window itself is
unchanged.

`snapshot` never waits. Other steps (`claim`, `record`, `reload`,
`model-policy` and so on) also refuse at once, as before. Before the stale
step existed, stale evidence was reported as `snapshot` by the broker and as
`execution-authority` by the generation loop.

## Review request stops

The independent review broker refuses a reviewer model request before any
reservation or provider call, and names the reason:

| Stop line | Meaning |
|---|---|
| `independent review stopped safely: model-request-binding` | The request does not bind: an unreadable or changed reviewer profile, a mailbox envelope for another attempt or with a wrong digest, a model or `max_tokens` outside the signed profile, a gap in the recorded turns, or a request after a final verdict |
| `independent review stopped safely: model-request-turn-budget` | The request binds, but its sequence is beyond the signed profile's `maxSteps` (at most 8): the reviewer spent its turns without a final verdict |

The broker sends the last permitted turn (`sequence == maxSteps`, from turn 2)
with [`tool_choice: "none"`](review-model-guard.md#final-turn-no-tool-calls)
and refuses a tool call there as `model-response-policy`, as it does an
answer without a valid verdict line. A well-behaved reviewer therefore never
reaches `model-request-turn-budget`; it appears with a worker that sends
another request regardless, or with a controller image older than this
change. Before this stop had its own name, it was reported as
`model-request-binding` (battery 31, issue 227). Like every review stop, it
fails the run without replay.

## Mailbox read stops

The generator's drive (`candidate reconciliation stopped safely:
execution-read-<step>`) and the independent review's drive (`independent
review stopped safely: model-mailbox-read-<step>`) read each model request
from the executor pod with the fixed `/model-mailbox read` helper. A failed
read names one bounded step:

| Step | Meaning |
|---|---|
| `before-observation`, `after-observation` | The pod observation before or after the read failed |
| `helper-transport-exec` | The exec upgrade or its stream failed: a dropped connection or refused dial, an abnormal closure, or an upgrade answered with a Kubernetes `Status` body and code 429, 500, 502, 503 or 504 |
| `helper-transport-relay` | The API server accepted the upgrade, but its own hop to the kubelet failed: from Kubernetes 1.30 its stream translator answers `101`, then sends an `InternalError` status with code 500 on the error channel (for example "error dialing backend") and closes normally |
| `helper-transport-deadline` | The exchange's own 5-second deadline, or the caller's deadline, ended before a complete answer. A hung helper also ends here, not as `helper-exit-124`: the exchange's deadline fires before the helper's own 5-second limit can report its exit |
| `helper-transport-expired` | The assignment expired during the read |
| `helper-transport` | Any other exchange failure: a permission refusal; another refused upgrade, for example a vanished pod or a 5xx answer without a `Status` body (a proxy's plain-text page cannot be told apart from a misrouted answer); a certificate or TLS verification failure; a protocol violation (unknown channel, text frame, oversized output, missing, malformed or other failure status, unexpected stderr); or a cancelled controller |
| `helper-exit-N` | The helper exited with code N (1 to 255) |
| `envelope` | The request envelope was invalid or bound to another attempt |

Batteries 36, 37 and 39 lost whole attempts at
`model-mailbox-read-helper-transport` and `execution-read-helper-transport`
although the work was fine (issue #826). The read helper only reads the
pending request file and receives no stdin, and the drive publishes a
response only after the read has returned, so repeating a read never
duplicates an effect or reads after an acknowledgement. An `exec`, `relay` or
`deadline` failure is therefore repeated within the same read, bounded by
attempt starts as for
[transient pod reads](../security/offline-executor-runtime.md#transient-reads-reasons-and-the-readiness-grace): another
attempt starts while 3 seconds have not passed since the first or until 3
attempts were made, and only while a whole 5-second attempt plus the
after-observation still fit before the assignment expiry and the drive's
deadline. Each attempt keeps its own 5-second bound. The step names the last
attempt's cause. An expiry, the plain `helper-transport` causes and a helper
exit stop at once. The `respond` exchange that publishes a response is never
repeated. Other connection errors before the upgrade (for example a refused
connection while the API server restarts) count as `exec` and are retried
within the same bounds. Controllers older than this change report all these
causes as `helper-transport`.

## Readiness stops

An attempt that stops before the executor runs logs a `readiness` stage. A
refused toolchain copy is named `readiness-toolchain-*` (see
[project manifest installation](project-manifest-installation.md)). Every
other refusal of the starting pod now names one bounded step; the stop,
cleanup and the exact pod acceptance are unchanged:

| Stage | Meaning |
|---|---|
| `readiness-uid` | The recorded pod UID is malformed (a programming guard) |
| `readiness-request` | Reading the pod failed (no answer within 5 s, or the connection failed) |
| `readiness-permission` | Reading the pod was denied |
| `readiness-status` | The read returned a status other than 200 |
| `readiness-deadline` | The start budget or the claim ended while the pod was still starting |
| `readiness-template` | The pod differs from its compiled template plus the observed RuntimeClass additions |
| `readiness-phase` | The pod left Pending/Running |
| `readiness-containers` | A worker or mailbox container restarted, terminated or reported an unexpected state |
| `readiness-containers-before-init` | A worker or mailbox container was created or ran before every init container completed |
| `readiness-containers-unstable` | A worker or mailbox container observed earlier (created or running) changed its container ID or vanished |
| `readiness-containers-created` | A worker or mailbox container reported waiting with a container ID that is not the kubelet's created state, or returned to it after running |
| `readiness-init-count` | The init container statuses were not the compiled ones (extra, missing, or an empty list when one is required) |
| `readiness-init-identity` | An init container was out of order, restarted or has a previous termination |
| `readiness-init-image` | An init container runs another image than its pin |
| `readiness-init-terminated` | An init container terminated out of order, without a container ID, or with a terminated container ID different from its status |
| `readiness-init-running` | An init container ran before the previous one completed, or without an ID or image |
| `readiness-init-created` | An init container reported waiting with a container ID that is not the kubelet's created state, or before the previous one completed |
| `readiness-init-waiting` | An init container waited for a reason other than PodInitializing or ContainerCreating |
| `readiness-init-state` | An init container reported no single state |
| `readiness-init-unstable` | An init container observed earlier vanished, changed its container ID, or returned to created after it ran |
| `readiness-bind` | The executor session could not be bound to the running pod |
| `readiness-bind-unstable` | The bound session observed other init containers than readiness did |

### The created-but-not-started state (issue #790)

Between two kubelet status updates a container can be *created* by the
container runtime but not yet *started*. The kubelet then writes it as
`waiting` with its container ID, its image, and no reason or message
(`convertToAPIContainerStatuses`, `ContainerStateCreated` in
`pkg/kubelet/kubelet_pods.go`). Before this change, readiness required every
waiting container to have no container ID, so this legitimate state refused
the pod and stopped the attempt at plain `readiness` with 0 model calls.

Evidence from evaluation battery 25: an init-state recording of
`executor-a22df1c0-…-1` shows `install-runner` terminated with exit code 0 at
03:58:57Z, `copy-toolchain` running at 03:58:59Z, and the pod Failed at
03:59:02Z with `copy-toolchain` terminated by SIGTERM (exit code 143): the
controller deleted a healthy pod a few seconds into its init sequence. Two
consecutive attempts did this, with no RuntimeClass failure logged. The
recorder polls every 0.3 to 2 s and did not capture the status the controller
refused. Checked against the kubelet source, the candidates fall into three
groups: `started: false` and an empty `state.terminated.containerID` are
already accepted; a shorter or reordered list and a previous state are not
written for this pod (the kubelet writes one status per spec container in spec
order, and a previous state needs a restart); the created state is written and
was refused. This is the best-supported cause; the refused status itself was
not captured.

Readiness now accepts exactly that state and nothing broader:

- waiting with no reason and no message, a well-formed container ID, no
  restart and no previous termination; an image ID, when already reported,
  must match the pin (a started container must report one);
- an init container only when every earlier one exited 0; a worker or mailbox
  container only after the init sequence completed;
- the created container's ID is held like any other: it must be observed
  again with the same ID (for a worker or mailbox container too), and a
  container observed running or terminated is never accepted as created
  again;
- a running pod (`Bind` and every later session observation) still requires
  every init container terminated and both main containers running;
- a created container whose start fails never becomes acceptable: the kubelet
  then reports a reason (for example `RunContainerError`), a restart or a
  failed pod, which refuses it (`readiness-init-created`,
  `readiness-containers-created`, `readiness-init-identity` or
  `readiness-phase`). If it simply stays created, the start budget ends at
  `readiness-deadline`. Either way the attempt fails closed.

A waiting status with a container ID and any reason (for example
`CreateContainerError`, or `PodInitializing` with an ID) is still refused, as
is every other rule above. The repository-tests observation of the verifier
accepts the same created state as waiting.

## Candidate export stops

After the generator's worker exits (exit 0, or the step-budget exit), the
controller exports its workspace. A failed export used to stop as a bare
`export`. It now names the step:

| Stop | Meaning |
|---|---|
| `export-before` | The observation before the export failed (a transport or permission error). |
| `export-before-running` | The worker was not terminated when the export began. |
| `export-helper-transport` | The export helper exchange failed. |
| `export-helper-exit-N` | The export helper exited with code N (1 to 255). For a refused workspace, N names the rule: 3 a path outside the bundle rules, 4 a link, hard link, special node or setuid/setgid/sticky file, 5 one file over the size limit (for example a compiled test binary left in the workspace), 6 the total content over its limit, 7 too many entries or files. Any other failure is 1, and 124 is the helper's own time limit. |
| `export-decode` | The exported bundle was invalid or bound to another source. |
| `export-after` | The observation after the export failed. |
| `export-after-identity` | The worker's exit changed between the two observations. |

The export has a 46-second budget. The first observation keeps 35 seconds for
the 30-second export and the observation after it, so a quick failure of the
first observation can still be retried for about 5 seconds instead of losing a
finished candidate.

## Verification transport stops

The verifier logs `verification reconciliation stopped safely: <stage>`. For
the repository-tests recipe, a stop while its three runs (`baseline`, `tests`,
`candidate`) execute now names one bounded step. A stop that concerns all
three runs is `runs-deadline` (the runs outlived their deadline), `runs-renew`
(renewing the verification claim failed) or `runs-context` (the verification's
deadline ended between observations). Any other stop names the run and its
step, for example `candidate-import-transfer-timeout`:

| Step | Meaning |
|---|---|
| `import-renew`, `import-fence` | The claim renewal or the authority fence before an import failed |
| `unimported-success` | The pod succeeded before its source was imported |
| `observe-request`, `observe-permission`, `observe-status` | Reading the pod failed, was denied, or returned another status |
| `observe-template`, `observe-phase`, `observe-container`, `observe-progress` | The pod differs from its template, failed, has a refused container state, or ended without a completed tests container |
| `import-before-*`, `import-after-*`, `ready-after-*` | The same observation steps around the import and the ready marker; `-state` means the observed containers changed |
| `import-transfer-*`, `ready-transfer-*` | The helper exchange through the API server failed: `-request`, `-permission` or `-timeout` |
| `import-exit`, `ready-exit` | A helper exited nonzero |
| `import-receipt` | The import receipt did not match the source |
| `import-repeat` | A second import was refused |
| `observe-uid` | The recorded pod UID is invalid |

The stop and cleanup are unchanged. A failed cleanup after such a stop keeps
the step, for example `cleanup-after-runs-renew`.

In battery 24 a verification stopped at plain `candidate` 3 s after the
verifier's readiness probe timed out, while its pods were transferring source.
That image named the last run for a claim renewal, deadline or context stop
too, so plain `candidate` covers those as well as any candidate observation or
import step; it cannot be attributed further. It was not caused by the
RuntimeClass check: no in-flight path reads the class (see
[transient reads](../security/offline-executor-runtime.md#transient-reads-reasons-and-the-readiness-grace)).
Two causes remain possible and are not established: a short stall of the
verifier's dependencies (the API server or PostgreSQL), or the
created-but-not-started state above, which the repository-tests observation
also refused (`observe-container`) and now accepts.
