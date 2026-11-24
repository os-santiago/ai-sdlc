# Executor failure diagnostics

A nonzero exit from the original executor stops the attempt before candidate
export. The controller reobserves that same Pod and container instance, including
the termination container ID and zero restarts. Within the original live claim
it appends the bounded exit code to `execution_failures`, then cleans up.

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

#### Telling a time-out from a step-budget stop

Code 22 is SCC's budget exhaustion for both its time and its tool-step budget.
Both are failures, and partial changes are never promoted. The diagnostic keeps
only the exit code, not SCC's summary, so tell them apart from durable records:

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
| 124 | The helper's own 5-second deadline ended (for example a slow exec stream) |
| 3 | The request is no longer pending in the mailbox |
| 1 | Any other refusal (malformed or mismatched response) |

As with every publication failure, the call stays uncertain and is never
retried by this diagnostic.
