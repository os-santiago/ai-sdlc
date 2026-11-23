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

The first managed Saferus attempt on 2026-11-17 stopped before this capability
existed. Its seven successful model HTTP responses do not explain its nonzero
executor exit, and cleanup removed the transient diagnostic files. This change
cannot reconstruct that historical cause. Installing a migration over a runtime
with workflow history requires a separately validated upgrade/recovery path;
the empty-install preparation and initial activation commands do not provide it.
