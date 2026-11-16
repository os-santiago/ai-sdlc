# Original-UID build containment

`CleanupQueuedBuild` reads a terminal claim and its archived original resource
identities from the build journal. It grants no build/publication authority and
works after policy freeze or command expiry. The controller invokes it after a
failed execution even if the stop acknowledgement was lost or another actor
already expired the claim. A still-active claim cannot pass the archived read.

Before DELETE, the original cluster and namespace must still match. Each known
PipelineRun is removed with foreground propagation and an original-UID
precondition. No Pod is force-deleted, no finalizer is stripped, and neither PV
nor PVC nor backing data is removed. An unknown create remains quarantined.

Completion additionally requires original PVC/PV, node UID/boot and fresh node
Lease, absent original runs and their TaskRuns, and no Pod using the reserved
PVC. A replaced resource or foreign consumer is never adopted/deleted. Changed
storage/node custody may prevent proof of completion even after the known roots
were safely requested for deletion. Cleanup never makes a physical slot reusable.

Each cleanup attempt is bounded to four minutes. Tekton Results' reviewed
[watcher default](https://github.com/tektoncd/results/blob/e175d25/cmd/watcher/main.go)
reserves 150 seconds after TaskRun completion for log forwarding. The initial
real experiment used a two-minute budget and correctly remained pending with
Results finalizers; its fixture teardown completed separately. The production
transport retains those finalizers and waits for their owning controller.

An unavailable controller/API or unknown create keeps cleanup pending. This
increment does not activate a component or promise complete recovery. A future
reconciler must serialize replicas, retain quarantine, and avoid allowing one
unknown request to starve all other containment work.

## Validation on 2026-11-16

The corrected real journal fixture built and published Saferus commit
`e4fc0085051a9c4dee088b5cc164d8b51b0fae67`, then durably acknowledged original-UID
cleanup without removing finalizers. Both Kubernetes fixture resources and its
disposable PostgreSQL were removed. Evidence:
`/tmp/ai-sdlc-journal-build-cleanup-v2/report.json` (status passed,
`durableBuiltRecordValidated` and `originalUIDCleanupValidated` true).
Published artifact: `sha256:1a3c0e4c1bb34869be5fa20114b898ee49e56103dfbdb4e67b894335eff614d1`.
Full validation passed 1,521 tests across 43 packages
(`/tmp/ai-sdlc-build-containment-validation-v2/report.json`). A subsequent focused
test strengthened the stale-node/lease assertion: exactly one original-root
DELETE is requested while absence acknowledgement stays denied.

The fixture supplies synthetic issue/merge history in a disposable journal.
It proves controller/transport composition, not autonomous issue-to-production.
