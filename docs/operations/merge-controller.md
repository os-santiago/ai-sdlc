# Publication-to-merge controller

`internal/mergeflow` is an inactive component controller with no public route or
installer activation. It accepts only identifiers selected from the publication
journal. Eligible issues remain the product's only external workflow input.

CI waiting has no active merge lease. The controller reads the immutable
publication fact, verifies original generation/verification/publication signatures
at the database publication time, and binds the original behavior and independent
review proofs. Separately, it requires the current issue, low-risk admission,
configuration, review profile and authority revision to remain valid. Expired
historical leases are never renewed or treated as present authorization.

A ready authenticated gate permits a fresh signed command lasting at most 25
seconds from the observed gate. The exclusive journal further bounds every
operation to that command. Immediately before committing a write intention, the
controller collects the native gate again and requires unchanged PR/head/base/tree,
protection, checks and review digests. The trusted transport sends a single
head-conditioned merge and verifies its returned commit. Policy, configuration,
publication-fact, gate and journal fences precede acknowledgement and immutable
terminal completion.

The current-issue check is part of every fence up to the moment the merge request
is sent: an issue closed, edited or relabelled before then still stops the run
without a write. After the request may have been sent (the transport's
post-response check, the acknowledgement and the finish), the fences check
everything except issue currency. For a repository governed by a project manifest
the generated pull request says `Closes #N`, so the merge itself closes the issue
and the intake records the closure as a new generation. That observation follows
from the effect being recorded; it cannot undo the merge, so it must not report
the merge as uncertain or leave an unacknowledged effect that quarantines every
later merge. The claim, lease, signed authority, publication record digest,
expected PR/head/base/tree, request digest and merge receipt are still checked,
as other post-effect stages bind only to their own durable request and receipt.
The built-in references keep `Refs #N`, so their issues stay open after the merge.

## Selecting a publication

Each poll selects one published publication that has no merge attempt (or
only bounded `not-sent` ones). The release manager remembers, in memory only,
when it last observed each publication, and selection takes one it has never
observed first, then the least recently observed, in publication order on
ties. So a publication whose gate stays unready, for example one held by a
review thread, is observed in turn with the others and no longer holds every
later merge. The memory holds at most 256 publication IDs and forgets an entry
after a day; after a restart, selection starts again from the oldest.

When the gate observes the expected pull request (same number, branch and
head) closed, the release manager logs `merge reconciliation:
merge-pull-request-closed` (or `merge-pull-request-merged-outside` when
something else merged it) instead of stopping with
`merge-observation-unavailable`, and parks the publication for 30 minutes
(`mergeflow.ParkClosed`): it is not selected, then observed once more, so a
reopened pull request resumes. Nothing is written. To deliver the issue again,
delete the branch and re-label the issue (the
[re-delivery procedure](publication-reconciliation.md#re-delivery-is-an-operator-action-decision-can-be-changed-later));
the new generation's publication replaces the parked one.

## Stopping before the send

A claimed run can stop before any merge request leaves: the request or claim is
refused, the publication fence fails, or the pre-send gate observation is
unavailable or differs from the queued one. The release manager then logs the
bounded stage (`mergeflow.DiagnosticStage`):

```text
merge reconciliation stopped safely: merge-stopped stage=gate-changed-checks
```

Stages that end before any send are `request`, `claim`, `publication-fence`,
`gate-policy`, `gate-manifest` (the installed merge gate's project manifest is
not the one the authorizing policy binds: another policy load's manifest, or a
cleared one; `mergegate.ErrManifest`), `gate-unavailable` (a forge read failed or a collection-internal
consistency check refused), `gate-deadline` (the claim, capped by the 25-second
gate lifetime, ended while the gate was being observed again),
`gate-queued-invalid`, `gate-not-ready`, `gate-<reason>` for a closed set of gate
reasons (`gate-native-merge-not-ready`, `gate-native-protection-insufficient`,
`gate-checks-not-passed`, `gate-review-feedback-unresolved`),
`gate-changed-expected`, `gate-changed-policy`, `gate-changed-checks`,
`gate-changed-reviews`, `effect-intent` and `transport-before-send`. After the
request may have been sent the stage is `transport`, `sent-fence`, `acknowledge`
or `finish`. Any other value prints as `unknown`.

The same manifest refusal when a publication is selected, before any merge
request is queued, logs `merge reconciliation stopped safely:
merge-gate-manifest` (its stage is also `gate-manifest`).

The journal, not the controller, decides whether the stop sent anything. Under
the request's row lock (which an effect intent also takes) a request with no
merge effect, or whose only effect the pre-send fence sealed not-sent, fails
with reason `not-sent`; any other stop stays `uncertain` and quarantines later
merges. The stage is diagnostic only: a `publication-fence` stop whose not-sent
seal failed, or an `effect-intent` stop whose intent commit landed, stays
`uncertain` and is never retried.

A `not-sent` request leaves its publication selectable again: the next attempt
observes the gate afresh, signs a new command and queues a new request. Each
publication receives at most three requests (`store.MergeAttempts`). The wait
grows (`store.MergeRetryWaits`): the second attempt may start 30 seconds after
the first command expired, the third 2 minutes after the second expired (a
fourth, if the limit ever grew, 5 minutes). Two signed commands for one
publication never overlap. Because the merge loop polls once a minute, the
attempts usually run about 1, then 3, minutes apart, so a forge fault lasting a
few minutes does not use them all up.

When the last allowed attempt stops before any send, the publication is never
attempted again. The release manager then logs

```text
merge reconciliation stopped safely: merge-stopped stage=gate-unavailable attempts=exhausted
```

and increments `ai_sdlc_merge_attempts_exhausted_total` (see
[metrics](metrics.md#merge-attempts-exhausted)). The pull request stays open. To
deliver the issue, close the pull request, delete its branch and re-label the
issue (the [re-delivery procedure](publication-reconciliation.md#re-delivery-is-an-operator-action-decision-can-be-changed-later)).
The flow console lists each attempt as its own merge request with reason
`not-sent`.

In battery 42 a pull request whose required check had passed was queued with a
ready gate, and its run stopped about 13 seconds later with no merge effect: the
pre-send gate observation failed or differed (the log then named no stage).
GitHub showed no change to the pull request, checks, statuses or reviews in that
window, so the likely cause was a transient forge read failure or a transient
mergeability recomputation. Because a publication then had exactly one merge
request, it could never merge and needed a full new attempt.

## Uncertain sends

A missing response or uncertain commit stops the original claim and quarantines
an unacknowledged effect. [Merge reconciliation](merge-reconciliation.md) resolves
it from a read-only observation once the claim has settled: a receipt when GitHub
shows the merge exactly as planned, a no-send when the pull request is still open
at the planned head and its head is not part of `main`, and otherwise the
quarantine stays.
Reconciliation cannot adopt that claim, repeat the write, finish the request as
merged or give the same publication a new attempt; only a `not-sent` stop does. The quarantine does not make
the release manager unready; it blocks only later merges. A terminal merge fact does not grant
artifact promotion, release or deployment authority.

The current PostgreSQL fixture uses synthetic forge observations and receipts.
It validates journal/controller transitions, not a live GitHub merge or the
issue-to-production E2E. Installation must separately enable the finite merge
grant, least-privilege columns and native strict branch protection without bypass.
