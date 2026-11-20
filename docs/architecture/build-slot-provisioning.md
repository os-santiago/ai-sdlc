# Build slot provisioning for live installations (design)

Status: proposed. Evidence: the issue-only E2E on 2026-11-20.

## Problem

A build slot is a dedicated node tmpfs mount with its own PV and PVC, and it is
**single-use by design**. `store/build_queue.go` `UnusedBuildSlot` returns a
configured slot only if it has no `build_slots` row, and a row is inserted
permanently for each build. The build foundation provisions exactly one slot
(`build-slot-0`). After the first build, every later merged change is queued but
never claimed, and expires. On 2026-11-20 the runtime reviewed, published and
merged a second delivery whose build could never start.

## Constraints found

- `building/configuration.go`: 1 to 8 slots. `ProfileDigest()` hashes the slot
  list, so any change to the slot list changes the profile digest. Downstream
  digest checks: provenance, promotion, deployment and release commands.
- `buildworkload/admission.go`: the build pod admission policy is compiled from
  the slot list (PVC names, node, storage identity). It is checked live
  (`podtransport/build_inventory.go`) and must be recompiled and updated in place
  (same name and UID).
- `BuildTrust` has no slot fields. Re-observing trust after adding slots must
  equal the immutable `build-trust` ConfigMap. If it differs, stop for human
  review.
- The Python build foundation is single-slot: the `storageIdentitySHA256`,
  `mountIntent`, `initializer` and `slot` keys, and the hard-coded
  `build-slot-0`. `build_foundation_node.initialize()` only uses the state dict
  it is given, so it can work on a per-slot sub-record.
- `release-refresh` refuses any `build.configJSON` change (closed delta) and
  requires a newer publication.
- **Slot burning:** `podtransport/build_controller.go` claims a slot
  permanently *before* the live profile and slot preflight. If the
  configuration and the admission policy ever disagree, every attempt consumes
  a fresh slot. Fixing this requires reworking
  `execution/build_containment_test.go` (it triggers expiry during the
  post-claim preflight) and validating with the kind tests.
- **Slots consumed by failures:** after `ClaimBuild`, any failure (an in-loop
  reinspection, the fence or the profile check) stops the build and cleans its
  cluster resources, but the `build_slots` row stays, and no code path releases
  it (the controller has no DELETE on the table). Moving the preflight before
  the claim narrows this window; it does not remove it. A post-claim failure
  consumes the slot. Slot capacity must be sized for failures, or a reviewed
  release path (a new slot identity after verified cleanup) must be added.
- A handed-off build whose slot never arrives is stranded: `merge_id` is unique
  and handoff happens once. Only new merges build after slots are added.

## Design

1. **Preflight before claim** (first PR). Run `inspectInstalledProfile` and
   `inspectBuildSlot` before `ClaimBuild`, and keep the in-loop rechecks. Move
   the containment test's injected expiry to a post-claim boundary. Validate in
   the kind jobs.
2. **`add-slots` (build foundation, live).**
   - Fence first: no PipelineRuns; no claimed build and none pending cleanup
     (a claimed build between phases re-inspects the admission policy and
     would fail if it changed); cluster, node and boot identity unchanged;
     no unused slot in the current configuration.
   - For each new slot: a new storage identity, `initialize(sub-record)`, then
     PV and PVC `build-slot-N`.
   - Recompile the admission policy for the old slot plus the new ones, and
     update it in place with UID and resourceVersion preconditions. Record the
     old and new hashes.
   - Re-observe trust and require it to equal the installed ConfigMap. Observe
     the profile and write the desired bundle.
   - This is safe while live: the current configuration's only slot is consumed,
     so nothing can be claimed until step 3.
   - Keep slot 0 in the existing top-level keys; new slots go in a `slots`
     sub-record.
3. **`build-slot` refresh generation (runtime).**
   - A chained release-refresh mode with no new publication: images and
     approvals are inherited.
   - Closed delta: `build.configJSON` with the old slot first, the new slots
     appended, and the admission dependency hash.
   - Same phases, intake enabled last.
   - Pause fence: the in-flight predicate plus no claimed or uncleaned build and
     nothing pending provenance, promotion or deployment.
4. **Rehearse** on a disposable installation before the real one.

Approval windows expire, and release-refresh requires a newer publication. A
renewal-only refresh (same images, renewed reviewed approvals) is needed so an
installation can extend its windows without a code change.

## Effort (estimate)

The PRs are separable, in this order:

| PR | Scope | Estimate |
|---|---|---|
| 1 | Preflight before claim | ~2 h, plus kind validation |
| 2 | `add-slots` | ~1 day |
| 3 | `build-slot` refresh mode | ~1.5 days |
| 4 | Live rehearsal | ~0.5 day |

The 8-slot limit makes this a capacity bridge. Returning cleaned slots to the
pool (a new slot identity on the same storage after verified cleanup) is the
longer-term option and needs its own security review.
