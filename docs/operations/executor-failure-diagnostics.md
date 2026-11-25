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
time budget or its tool-step budget. The launcher tells them apart from a fact
it measures itself, never from SCC's output, summary or audit log, which the
generated tools can write:

- The pinned SCC checks its time budget before its step budget, before every
  model call, and measures it from its own start, which is later than the
  launcher's clock starts. The launcher configures no token budget, so time
  and steps are the only budgets that end a generation run.
- If SCC exits 22 strictly before its time budget (`--max-seconds`, above) has
  elapsed on the launcher's monotonic clock, the time budget cannot have ended
  it: the launcher exits **25**, the step budget.
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
