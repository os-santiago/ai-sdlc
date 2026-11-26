# Publication reconciliation

A publication effect is *uncertain* when the release manager journaled its
intent (the exact request digest) and may have sent the write, but recorded
neither a receipt nor a known no-send. That happens when the forge returns a
5xx, the connection drops or a fence fails after the request left. The request
then fails with reason `uncertain`, and
[`PublicationBlocked`](publication-journal.md) quarantines all publication:
the release manager reports `publication-quarantined` and reports itself not
ready.

The release manager now resolves such an effect itself, by **read-only
observation of the forge**. It never writes to the forge while reconciling,
and it changes the journal only when the forge proves what happened.

## When it runs

Each publication poll (every 5 s) that finds the quarantine:

1. Lists up to 16 uncertain effects of failed requests, oldest first. An effect
   is offered only once its request's claim ended more than 120 s ago (database
   time). A claim lasts at most 120 s and the writer cannot send after it, so the
   original POST can no longer be in flight. The window is fixed in
   `store.PublicationReconciliations`; no caller can shorten it.
2. Takes the first effect that is not pacing. After any outcome that leaves the
   effect quarantined, the effect is observed again after 30 s, then 1, 2, 4 and
   8 min, then every 15 min. Pacing is in memory: a restart observes sooner but
   gains no authority.
3. Re-derives the planned write and observes it (see below), records the
   outcome, and logs one bounded line:

   ```text
   publication reconciliation: effect=tree outcome=not-sent
   ```

   The line holds only the journal's effect kind and a closed outcome name, never
   a body, token, URL or forge message.
4. Re-reads the quarantine. If no uncertain effect remains, the poll reports
   `publication-reconciled` and the next poll selects queued work as usual.

## Re-deriving the planned write

The reconciler trusts no forge content to say what was planned. From journaled
facts only (the signed command, the claim's plan digest and commit time, and the
receipts of earlier effects), it rebuilds the exact request body the writer
sends. It continues only when that body's SHA-256 equals the request digest
journaled before the POST. Otherwise the outcome is `unplanned`.

For a tree or a commit it first rebuilds the plan as the controller did: the
candidate delta applied to the pinned source, under the manifest bound by the
policy that authorized the command (`PrepareTree` with that policy's
`ProjectSHA256`). The installed policy's revision must equal the command's
`authorityRevision`. If no policy can be read, the outcome is `unavailable`. If
another policy is now installed, the outcome is `unplanned`, because the
authorizing manifest can no longer be bound. The rebuilt plan's digest must
equal the claim's plan digest. That digest binds the plan's recorded commit
identity, so a reconfigured identity cannot change what is observed.

| Effect | Planned from | Planned identity |
| --- | --- | --- |
| tree | candidate delta applied to the pinned source (the controller's own rebuild); its command-and-plan digest must equal the claim's plan digest | `ExpectedTreeSHA`, content-addressed |
| commit | the rebuilt plan (its tree must equal the tree receipt, its message and recorded commit identity), source SHA, claim commit time; built by the writer's own `commitBody` | Git commit object SHA computed by the writer's `commitSHA` (tested against `git commit-tree`), the SHA `CreateCommit` requires the forge to return |
| branch | `BranchName(command)` and the commit receipt | `refs/heads/<branch>` at the commit SHA |
| pull request | branch and commit receipts, the claim's plan digest | open PR from the branch at the commit whose body carries `Publication proof SHA-256: <plan digest>` |

## Observation and outcomes

The observer (`publication.Reader`) issues only `GET` requests with the
publication credential, to a closed set of paths. It does not follow redirects
and does not retry. Before it trusts any `404`, it must read the publication's
parent commit (`GET /git/commits/<source>`), and it reads that commit again
after the `404`. If the credential cannot see the repository's Git objects at
either read, the result is an error, not absence. The second read narrows the
window in which lost access could look like absence; it cannot prove access at
the instant of the `404`.

| Effect | Exists exactly → `completed` | Absent | Anything else |
| --- | --- | --- | --- |
| tree | `GET /git/trees/<sha>` is 200, same SHA, not truncated | 404: `not-sent` | `mismatch` or `unavailable` |
| commit | `GET /git/commits/<sha>` is 200 with the planned tree, single parent, message, and the plan's identity as author and committer | 404: `not-sent` | `mismatch` or `unavailable` |
| branch | `GET /git/ref/heads/<branch>` is 200: a commit ref at exactly the planned SHA | 404: `unproven` (stays quarantined) | at another SHA: `mismatch` |
| pull request | `GET /pulls?state=all&head=<owner>:<branch>` lists exactly one PR: open, not draft, head is the branch at the planned SHA in this repository, base `main` at the planned source SHA (the check the writer applies to the creation response), body carries the plan digest | the list is empty and `GET /git/ref/heads/<branch>` still shows the branch at the planned SHA: `not-sent` | several PRs; a closed, edited, retargeted or differently based one; or an empty list while the branch is gone or moved: `mismatch` |

Outcome names are closed:

- `completed`: the receipt the original call would have produced is recorded.
  It is marked `"observed": true`.
- `not-sent`: the forge proves that the planned object is absent, so the effect
  is recorded as not sent (`not_sent_at`). This is a statement about the forge's
  current state, not about the network. A request may have left and been rejected
  or never applied; what matters is that the planned object does not exist and,
  after the settle window, cannot appear.
- `mismatch`: something exists that is not the planned effect. The publication stays quarantined.
- `unavailable`: the forge, the source or the journal could not be read or
  written, including any non-200, non-404 status. The publication stays quarantined.
- `unplanned`: the journaled facts do not re-derive the journaled request. The publication stays quarantined.
- `unproven`: the planned branch does not exist, but that does not prove it was
  never created: a maintainer may have deleted it after a lost reply. The
  publication stays quarantined, and the journal itself refuses a branch no-send.

The durable journal of a resolution is the effect row itself: an observed
receipt, or `not_sent_at` set after the claim ended. Outcomes that leave the
effect quarantined change no row.

**Attempt records (decision, can be changed later).** No per-attempt table is
kept. Every attempt is visible in two places: the bounded log line, and the
counter `ai_sdlc_publication_reconciliations_total{effect,outcome}` (see
[metrics](metrics.md#publication-reconciliations)), whose labels come only from
the closed effect and outcome sets. A durable per-attempt table would add a
table to the schema. Configuration upgrades admit only constraint relaxations,
so that table would need its own upgrade bridge.

## Principle exception: an observed receipt

Elsewhere the runtime holds that an observation never manufactures a missing
receipt. For example, deployment recovery never turns an observed image into a
missing API acknowledgement (see [deployment recovery](deployment-recovery.md)).
Publication reconciliation is the **one narrow, named exception** to that rule.
An observed receipt is recorded only when all of the following hold:

1. The effect is uncertain: begun, with neither a receipt nor a known no-send.
2. Its request is already `failed`. Reconciliation never touches a queued,
   claimed or published request, and never changes the request's state.
3. The settle window has passed: the claim ended more than 120 s ago (database
   time), so the original write can no longer be in flight.
4. The observation is read-only (GET only) and matches exactly the planned
   effect. The plan is re-derived from journaled facts, and its request body
   matches the journaled request digest byte for byte.
5. The receipt is marked `"observed": true`. It is never accepted where a live
   receipt is required: `AcknowledgePublicationEffect`, `FinishPublication` and
   the published-record reader all reject it. It therefore never leads to a
   publication proof, a merge or a deployment.

Any other journal keeps the rule without exception.

## Safety argument

- **No forge write.** Reconciliation has no write transport. `publication.Reader`
  can only `GET`. The tests fail on any other method and count zero writes.
- **No weakened fence.** Recording an outcome takes the publication claim lock
  (`pg_advisory_xact_lock(73421025)`), so it is serialized with claims. In one
  transaction it then re-checks that no publication is claimed, that the request
  is still `failed` and settled with the same plan digest and commit time, and
  that the effect is the same uncertain intent (token and request digest) with
  every predecessor acknowledged. A branch receipt must name the acknowledged
  commit and the branch the signed command plans; a pull request receipt must
  name the acknowledged commit and that same branch. An effect whose
  predecessors no longer verify is reported `unplanned` and stays quarantined,
  without stopping reconciliation of other effects.
  A resolved effect can never change again.
- **Observed receipts never authorize anything.** A live write cannot present an
  observed receipt (`AcknowledgePublicationEffect` rejects it). `FinishPublication`
  and the published-record reader reject one. An observed receipt only closes an
  uncertain effect of a failed request.
- **Absence is proven, not inferred.** Only a `404` for a tree or commit with a
  successful parent commit read before and after it, or an empty `state=all`
  pull request list while the head branch still points at the planned commit,
  counts as absence. A missing branch never does, because a ref is mutable.
  Every error keeps the quarantine.
- **Content-addressed objects.** A tree or commit is the last begun effect, so
  nothing on the forge references it. Even a wrongly computed SHA could at worst
  mark an unreferenced object not sent. Because the request is never retried, it
  is never written twice.

## State machine: the request stays failed

The original request is **closed as failed, with every effect known**. It is
never retried. Reconciliation changes only effect rows: the request keeps state
`failed` and its original reason, and `ClaimPublication` cannot select it again.

Why closing, not retrying with a fresh claim:

- The journal has no `failed → queued` or `failed → claimed` edge. Claims are
  non-renewable and non-adoptable by design.
- Effects are keyed by `(request_id, kind)`. A retry would collide with the
  journaled effects, or need them erased.
- A partially completed request (for example, the branch exists but no PR was
  sent) would need the writer to adopt an existing branch. The writer refuses to
  do that by design.
- The signed command and review have finite expiries, often already passed by the
  time the forge recovers.

### Re-delivery is an operator action (decision, can be changed later)

The runtime does not re-deliver the issue automatically. The candidate of a
reconciled request is not published. To deliver the issue again, re-label it:

1. Remove the installation's trigger label (the intake configuration's
   `triggerLabel`) from the issue.
2. Wait for one intake poll (`pollSeconds`). Intake records a new, ineligible
   generation.
3. Add the trigger label back, as one of the authorized actors
   (`authorizedActorIDs`). Intake records a new eligible generation. The
   issue is then admitted, generated, verified, reviewed and published again from
   the start, under a new claim and a new branch name.

### Leftover branch or pull request (decision, can be changed later)

The runtime never deletes or closes anything on the forge, and reconciliation
only reads. If reconciliation recorded `completed` for a branch or a pull
request, that branch (and, for a pull request, that open PR) exists on the forge
without a published record. The merge controller never acts on it, and nothing
in the runtime depends on it.

The flow console shows it as `publication · reconciled · completed` with
attribute `effect=branch` or `effect=pull-request`
([flow console](flow-console.md)). To clean up:

1. Confirm, read-only, that the branch is `ai-sdlc/issue-<n>-<candidate>` at the
   commit the receipt names. For a pull request, confirm that its head is that
   branch and that its body carries the plan digest.
2. Close the pull request with a short comment that it came from a reconciled,
   unpublished attempt.
3. Delete the branch.

Do the cleanup before you re-label the issue, so that two pull requests for the
same issue are never open at the same time.

## When it stays quarantined

A `mismatch`, `unplanned`, `unproven` or persistent `unavailable` outcome needs an operator.
Inspect the forge read-only, then reconcile the effect row by hand, as before
this change. The bounded log line names the effect kind and the reason.

- **Pull request closed or edited** (`effect=pull-request outcome=mismatch`): the
  forge has a PR for the planned head, but it is closed, a draft, retargeted, or its
  body no longer carries the plan digest. A PR was sent, so the effect can never be
  recorded as not sent. Confirm read-only that it is the runtime's PR (head branch
  and head SHA). Then record its receipt by hand and leave the PR closed, or
  reopen and restore it so that the next observation completes it.
- **Pull request on another base** (`effect=pull-request outcome=mismatch`): the
  PR's recorded base is not the planned source commit. GitHub may have opened it
  after `main` moved, which the writer would have refused. Decide by hand whether
  it is acceptable; the current `main` never proves the original base.
- **Pull request absent but branch gone** (`effect=pull-request outcome=mismatch`):
  the list is empty, but the head branch was deleted or moved, so a PR opened
  from it may no longer match the head filter. Search the PRs for the plan digest.
- **Branch at another SHA** (`effect=branch outcome=mismatch`): something
  moved the branch. Do not delete it before you have established who moved it.
- **Branch absent** (`effect=branch outcome=unproven`): the branch does not
  exist now. Check the repository's activity or audit log for a creation and
  deletion of `ai-sdlc/issue-<n>-<candidate>`. Record the receipt by hand if it
  was created, or the no-send if the forge shows it never was.

## Configuration and migration

There is no new setting, migration or database privilege. The reconciler uses
the installed publication credential (read endpoints only) and the existing
release-manager column grants on `publication_effects`. The receipt gains an
optional `observed` field. It is omitted for every live write, so existing
receipts and their digests are unchanged.
