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

SCC receives a shorter time budget than the launcher's kill: one call window
plus 15 seconds earlier, at most an eighth of `maxSeconds` (525 of 600 seconds
by default). SCC checks that budget before each model call, so a run that runs
out of time normally ends as `worker-exit workerExitCode=22`, SCC's budget
exhaustion, with no call in flight. Code 22 also covers the step budget; both
are failures, and partial changes are never promoted. Code 124 remains possible
when the last call or tool outlasts the margin, for example a slow model whose
call window is longer than the margin; the stop line above still names it.

The first managed Saferus attempt on 2026-11-17 stopped before this capability
existed. Its seven successful model HTTP responses do not explain its nonzero
executor exit, and cleanup removed the transient diagnostic files. This change
cannot reconstruct that historical cause. Installing a migration over a runtime
with workflow history requires a separately validated upgrade/recovery path;
the empty-install preparation and initial activation commands do not provide it.
