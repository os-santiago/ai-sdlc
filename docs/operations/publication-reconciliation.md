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
   publication reconciliation: effect=branch outcome=not-sent
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

| Effect | Planned from | Planned identity |
| --- | --- | --- |
| tree | candidate delta applied to the pinned source (the controller's own rebuild); its command-and-plan digest must equal the claim's plan digest | `ExpectedTreeSHA`, content-addressed |
| commit | tree receipt, source SHA, claim commit time, fixed message and identity | Git commit object SHA computed locally (tested against `git commit-tree`) |
| branch | `BranchName(command)` and the commit receipt | `refs/heads/<branch>` at the commit SHA |
| pull request | branch and commit receipts, the claim's plan digest | open PR from the branch at the commit whose body carries `Publication proof SHA-256: <plan digest>` |

## Observation and outcomes

The observer (`publication.Reader`) issues only `GET` requests with the
publication credential, to a closed set of paths. It does not follow redirects
and does not retry. Before it trusts any `404`, it must read the publication's
parent commit (`GET /git/commits/<source>`). If the credential cannot see the
repository's Git objects, the result is an error, not absence.

| Effect | Exists exactly → `completed` | Absent → `not-sent` | Anything else |
| --- | --- | --- | --- |
| tree | `GET /git/trees/<sha>` is 200, same SHA, not truncated | 404 | `mismatch` or `unavailable` |
| commit | `GET /git/commits/<sha>` is 200 with the planned tree, single parent, message, author and committer | 404 | `mismatch` or `unavailable` |
| branch | `GET /git/ref/heads/<branch>` is 200: a commit ref at exactly the planned SHA | 404 | at another SHA: `mismatch` |
| pull request | `GET /pulls?state=all&head=<owner>:<branch>` lists exactly one PR: open, not draft, head is the branch at the planned SHA in this repository, base `main`, body carries the plan digest | the list is empty (the forge proves that no PR, open or closed, exists for that head) | several PRs, or a closed, edited or different one: `mismatch` |

Outcome names are closed:

- `completed`: the receipt the original call would have produced is recorded.
  It is marked `"observed": true`.
- `not-sent`: a known no-send is recorded (`not_sent_at`).
- `mismatch`: something exists that is not the planned effect. The publication stays quarantined.
- `unavailable`: the forge, the source or the journal could not be read or
  written, including any non-200, non-404 status. The publication stays quarantined.
- `unplanned`: the journaled facts do not re-derive the journaled request. The publication stays quarantined.

The durable journal of a resolution is the effect row itself: an observed
receipt, or `not_sent_at` set after the claim ended. The non-resolving outcomes
change no row; they appear only in the bounded log line.

## Safety argument

- **No forge write.** Reconciliation has no write transport. `publication.Reader`
  can only `GET`. The tests fail on any other method and count zero writes.
- **No weakened fence.** Recording an outcome takes the publication claim lock
  (`pg_advisory_xact_lock(73421025)`), so it is serialized with claims. In one
  transaction it then re-checks that no publication is claimed, that the request
  is still `failed` and settled with the same plan digest and commit time, and
  that the effect is the same uncertain intent (token and request digest) with
  every predecessor acknowledged. A branch receipt must name the acknowledged
  commit; a pull request receipt must name the acknowledged commit and branch.
  A resolved effect can never change again.
- **Observed receipts never authorize anything.** A live write cannot present an
  observed receipt (`AcknowledgePublicationEffect` rejects it). `FinishPublication`
  and the published-record reader reject one. An observed receipt only closes an
  uncertain effect of a failed request.
- **Absence is proven, not inferred.** Only a `404` after a successful parent
  commit read, or an empty `state=all` pull request list, counts as absence.
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

Consequences for the operator:

- If reconciliation recorded `completed` for a branch or pull request, that
  branch, or that open PR, exists on the forge without a published record. The
  merge controller does not act on it. Close the PR and delete the branch, or
  leave them for review; nothing in the runtime depends on them.
- The issue's candidate is not published. Deliver it again through a new
  generation of the issue.

## When it stays quarantined

A `mismatch`, `unplanned` or persistent `unavailable` outcome needs an operator.
Inspect the forge read-only, then reconcile the effect row by hand, as before
this change. The bounded log line names the effect kind and the reason.

## Configuration and migration

There is no new setting, migration or database privilege. The reconciler uses
the installed publication credential (read endpoints only) and the existing
release-manager column grants on `publication_effects`. The receipt gains an
optional `observed` field. It is omitted for every live write, so existing
receipts and their digests are unchanged.
