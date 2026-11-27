# Merge reconciliation

A merge effect is *uncertain* when the release manager journaled its intent (the
exact request digest of the merge `PUT`) and may have sent it, but recorded
neither a receipt nor a known no-send. That happens when GitHub's answer arrives
after the claim deadline, the connection drops, or a fence fails after the
request left. (A run that stopped before journaling any intent, or whose intent
the pre-send fence sealed, sent nothing: it fails with reason `not-sent`, does
not quarantine and its publication gets a bounded fresh attempt; see the
[merge controller](merge-controller.md#stopping-before-the-send).) The request
then fails with reason `uncertain`, and
[`MergeBlocked`](merge-journal.md) quarantines every later merge: the merge loop
logs `merge reconciliation stopped safely: merge-quarantined`.

In battery 39 a merge `PUT` answered after the transport deadline. GitHub had
merged the pull request, but the journal held an uncertain effect and no later
merge could run. The release manager now resolves such an effect itself, by
**read-only observation of the forge**, with the same design as
[publication reconciliation](publication-reconciliation.md). It never writes to
the forge while reconciling, and it changes the journal only when the forge
proves what happened.

## When it runs

Each merge poll (every 60 s) that finds the quarantine:

1. Lists up to 16 uncertain merge effects of failed requests, oldest first. An
   effect is offered only once its request's claim ended more than **120 s** ago
   (database time). This is the settle window:
   - A merge claim lasts at most 90 s, and the signed command and gate cap it to
     about 30 s. The transport's context ends at the claim deadline, so it cannot
     send after the claim ends.
   - GitHub answers a merge only after it has written the merge commit and moved
     the base (up to about 5 s in battery 39). A request it accepted just before
     the deadline is applied well within the further 120 s.
   - The window equals the publication settle window. It is fixed in
     `store.MergeReconciliations` and `store.ResolveMergeEffect`; no caller can
     shorten it.
2. Takes the first effect that is not pacing. After any outcome that leaves the
   effect quarantined, the effect is observed again after 30 s, then 1, 2, 4 and
   8 min, then every 15 min. Because the loop polls once a minute, the first
   retries happen at the next poll. Pacing is in memory: a restart observes
   sooner but gains no authority.
3. Re-derives the planned merge and observes it (see below), records the outcome,
   and logs one bounded line:

   ```text
   merge reconciliation: effect=merge outcome=completed
   ```

   The line holds only a closed outcome name, never a body, token, URL or forge
   message.
4. Re-reads the quarantine. If no uncertain effect remains, the poll reports
   `merge reconciliation: merge-reconciled`, and the next poll selects work as
   usual.

## Re-deriving the planned merge

The reconciler trusts no forge content to say what was planned. From the
journaled signed command only (`merge_requests.payload`, whose SHA-256 must equal
`payload_digest`), it rebuilds the exact request the transport sends with
`mergewriter.PlanMerge`. That is the function `Writer.Merge` itself uses, so the
path, body and digest cannot drift apart:

- `PUT /repos/<repository>/pulls/<number>/merge`;
- body `sha` (the planned head), `merge_method: merge`,
  `commit_title: fix(ai-sdlc): integrate issue #<n> [skip release]` and
  `commit_message: Refs #<n>`.

It continues only when the digest of that request equals the request digest
journaled before the `PUT`. Otherwise the outcome is `unplanned` and nothing is
read.

## Observation and outcomes

The observer (`mergewriter.Reader`) issues only `GET` requests with the
publication credential, to a closed set of paths. It does not follow redirects
and does not retry. Every status other than `200`, including `404` and `403`, is
an error. No response is ever read as absence.

| Forge state | Reads | Outcome |
| --- | --- | --- |
| The pull request is merged and closed at the planned head, from the planned branch of this repository into `main`. Its merge commit has the planned tree, exactly two parents (planned base, then planned head), and the message `<title>\n` followed by `Refs #<n>` (blank lines aside). | `GET /pulls/<n>`, `GET /git/commits/<merge commit>` | `completed` |
| The pull request is open, not a draft and not merged, at the planned head, from the planned branch into `main`, and `refs/heads/main` still points at the planned base. | `GET /pulls/<n>`, `GET /git/ref/heads/main` | `not-sent` |
| The same open pull request, but `main` moved: GitHub's comparison of the current `main` with the planned head reports `ahead` or `diverged` (with at least one commit ahead), so the head is not contained in `main`. | the reads above, then `GET /compare/<main>...<head>?per_page=1` | `not-sent` |
| Anything else: a moved head, a fork head, a retargeted base, another tree, parents or message, a squash or rebase merge, a closed unmerged pull request, a draft, or a moved `main` that the comparison reports as `behind` or `identical` (it already contains the head). | | `mismatch` |
| Any read failed: transport, status (`403`, `404`, `5xx`), body shape or size. | | `unavailable` |

Outcome names are closed:

- `completed`: the receipt the original call would have produced is recorded,
  in the format the live acknowledgement stores (`ai-sdlc.merge-receipt/v1` with
  the merge commit and the planned head, base, tree and pull request) and marked
  `"observed": true`.
- `not-sent`: the forge proves that the planned merge did not happen, so the
  effect is recorded as not sent (`not_sent_at`). The pull request is open at the
  planned head, and its head is not part of `main`. A merged pull request cannot
  be reopened, so after the settle window that request can no longer apply.
- `mismatch`: the forge shows neither the planned merge nor the untouched pull
  request. Merges stay quarantined.
- `unavailable`: the forge or the journal could not be read or written. Merges
  stay quarantined.
- `unplanned`: the journaled command does not re-derive the journaled request
  digest. Merges stay quarantined.
- `resolved`: another release-manager replica, or an operator, resolved the
  effect after this step listed it and before it could record its own outcome.
  Nothing is written, and the effect is no longer paced.

Both `completed` and `not-sent` lift the quarantine but still need an operator
follow-up for the issue (see [what follows](#state-machine-the-request-stays-failed)
and [follow-up after a resolution](#follow-up-after-a-resolution)).

The durable record of a resolution is the effect row itself: a receipt, or
`not_sent_at` set more than 120 s after the claim ended. Outcomes that leave the
effect quarantined change no row. Each attempt is also counted in
`ai_sdlc_merge_reconciliations_total{outcome}` (see
[metrics](metrics.md#merge-reconciliations)).

## Principle exception: an observed merge receipt

Elsewhere the runtime holds that an observation never manufactures a missing
receipt. Merge reconciliation is the second narrow, named exception, next to
[publication reconciliation](publication-reconciliation.md#principle-exception-an-observed-receipt).
A merge receipt is recorded from an observation only when all of the following
hold:

1. The effect is uncertain: begun, with neither a receipt nor a known no-send.
2. Its request is already `failed`. Reconciliation never touches a queued,
   claimed or merged request, and never changes the request's state.
3. The settle window has passed: the claim ended more than 120 s ago (database
   time).
4. No merge is claimed. The resolution takes the merge claim lock
   (`pg_advisory_xact_lock(73421026)`), the same lock `ClaimMerge` takes.
5. The observation is read-only (GET only) and matches exactly the planned merge.
   The plan is re-derived from the journaled signed command, and its request
   digest equals the journaled one.
6. The receipt names the command's pull request, head, base and tree; the
   journal re-checks this.

7. The receipt is marked `"observed": true`. The field is omitted for every live
   write, so live receipts and their digests are unchanged.

An observed receipt never authorizes anything. `AcknowledgeMergeEffect`,
`FinishMerge` (the merge proof) and the merged-record reader all refuse one, and
the journal records an observed receipt only through `ResolveMergeEffect`. A
merged record (`merged_records`) can only be written under a live claim of the
request, before its signed authority expires. A failed request is never claimed
again, so an observed merge receipt never becomes a merged record, nor its own
build, provenance record, release or deployment.

This does not keep the change off the release path. The merged code is on `main`.
The next change that the runtime merges is built from a tree that contains it,
so it ships inside that change's artifact, covered only by that change's
provenance record. No record names the reconciled change itself.

## State machine: the request stays failed

Reconciliation changes only the effect row. The request keeps state `failed`
and reason `uncertain`; `ClaimMerge` cannot select it again, and
`NextMergePublication` never selects its publication again: only a request the
journal itself recorded as `not-sent` admits a fresh attempt, and a
reconciliation outcome never changes the request's reason.

**The request does not finish as merged (decision, can be changed later).** The
merge journal's schema allows a merged record only under a live claim, before the
signed merge command expires (about 30 s after the gate observation), and the
merged-record reader revalidates that authority at the merge time. By the time
the settle window ends, that authority has expired. Finishing the request as
merged would mean backdating a terminal fact, weakening those checks, or adding
a separate "merged by observation" fact. This change does none of these.

What follows for each outcome:

- **`completed`.** The pull request is merged on GitHub. For a repository
  governed by a project manifest, the pull request body says `Closes #N`, so
  GitHub closed the issue with the merge itself, and the intake records the
  closure as a new generation. No runtime issue-closing step is skipped: the
  runtime never closes an issue after a merge; only the release's terminal stage
  does that, after a deployment. Because no merged record exists, the
  merged-source build, provenance, promotion and deployment stages do not start
  for this change, and the work item stays `running` (or becomes `blocked` once
  the orchestrator sees the closed issue as a superseding generation). The code itself is on
  `main` and ships inside the next merged change's tree, without its own
  provenance record (see the principle exception above). For a built-in
  reference repository the merge commit says `Refs #N`, so the issue also stays
  open.
- **`not-sent`.** The pull request is still open and unmerged. The runtime does
  not try again: the request may have been sent, so it keeps reason `uncertain`
  and its publication keeps its one request that may have been sent (decision,
  can be changed later; only a stop the journal proves pre-send is retried). To deliver
  the issue, follow the [re-delivery procedure](publication-reconciliation.md#re-delivery-is-an-operator-action-decision-can-be-changed-later):
  close the old pull request and delete its branch, then re-label the issue.

In both cases the next merge poll lifts the quarantine and merges other
publications as usual.

## Follow-up after a resolution

A resolution lifts the quarantine but leaves work for an operator. Alert on both
outcomes (see [metrics](metrics.md#merge-reconciliations)).

**After `completed`:**

1. Find the work item and the issue (administrator session):

   ```sql
   SELECT e.work_item_uuid, w.status,
          convert_from(m.payload, 'UTF8')::jsonb ->> 'issueNumber' AS issue,
          convert_from(m.payload, 'UTF8')::jsonb ->> 'pullRequest' AS pull_request
     FROM merge_requests m
     JOIN publication_requests p ON p.id = m.publication_id
     JOIN execution_requests e ON e.id = p.candidate_id
     JOIN work_items w ON w.uuid = e.work_item_uuid
     JOIN merge_effects f ON f.request_id = m.id
    WHERE m.state = 'failed' AND f.completed_at IS NOT NULL
      AND f.receipt_digest IS NOT NULL
      AND convert_from(f.receipt, 'UTF8')::jsonb ->> 'observed' = 'true';
   ```

2. Confirm read-only that the pull request is merged and, for a manifest
   repository, that the issue is closed. For a built-in reference repository,
   close the issue by hand with a comment naming the pull request.
3. Decide how the change reaches production. Either the next merged change
   carries it (its tree contains the code), or you trigger a release by hand.
   Note that no provenance record names this change.
4. Close out the work item, which otherwise stays `running` or `blocked`:

   ```sql
   UPDATE work_items SET status = 'completed', updated_at = clock_timestamp()
    WHERE uuid = '<work item>' AND status IN ('running', 'blocked');
   ```

   This records no production fact. Use `failed` instead if you decide the
   change must not ship.

**After `not-sent`:**

1. The pull request is still open and unmerged, and the runtime will never merge
   it (its request may have been sent, so it is never retried). Close it with a short
   comment that it came from an unresolved merge attempt, and delete its branch.
2. Re-deliver the issue with the [re-delivery procedure](publication-reconciliation.md#re-delivery-is-an-operator-action-decision-can-be-changed-later)
   (remove and re-add the trigger label). Clean up before you re-label, so that
   two pull requests for the same issue are never open at the same time.
3. `not-sent` proves only that this merge request did not merge the planned
   head. If someone already put the same change on `main` another way (a
   cherry-pick or a squash outside the pull request), check `main` before you
   re-deliver, so the work is not done twice.

## Readiness and the quarantine

Release-manager readiness no longer depends on the merge quarantine. Readiness
still requires the database, the authority, intake and execution configuration,
the review configuration, the publication credential, the merge runtime (merge
policy, gates and transport) when merge is enabled, and no publication
quarantine.

Before this change an uncertain merge effect also failed readiness. The Service
then lost its endpoints, so the verifier's `publish-candidate` deliveries timed
out silently, although publication itself was unaffected. In battery 39, five
issues that had passed review were lost this way. A merge quarantine blocks only
later merges, so it now shows as:

- the stop `merge reconciliation stopped safely: merge-quarantined` each minute,
  counted as `ai_sdlc_stops_total{component="release-manager",loop="merge",stage="merge-quarantined"}`;
- the gauge `ai_sdlc_quarantined{journal="merge"}` at `1` (the publication
  journal is reported as `journal="publication"`);
- `ai_sdlc_merge_reconciliations_total{outcome}` for each observation;
- the flow console's `merge · merge · quarantined` event, and
  `merge · reconciled · completed` or `not-sent` once it is resolved.

## When it stays quarantined: operator procedure

A `mismatch`, `unplanned` or persistent `unavailable` outcome needs an operator.
Every step below is read-only on the forge until the final decision, and nothing
in the runtime retries the merge.

1. **Find the effect.** With a PostgreSQL administrator session on the runtime
   database (the effect tables are row-level protected for the component
   roles), list the uncertain merge effect:

   ```sql
   SELECT r.id, r.publication_id, r.claim_until, e.token, e.request_digest,
          convert_from(r.payload, 'UTF8')::jsonb ->> 'pullRequest' AS pull_request,
          convert_from(r.payload, 'UTF8')::jsonb ->> 'headSHA' AS head,
          convert_from(r.payload, 'UTF8')::jsonb ->> 'baseSHA' AS base,
          convert_from(r.payload, 'UTF8')::jsonb ->> 'treeSHA' AS tree,
          convert_from(r.payload, 'UTF8')::jsonb ->> 'issueNumber' AS issue
     FROM merge_effects e JOIN merge_requests r ON r.id = e.request_id
    WHERE r.state = 'failed' AND e.completed_at IS NULL AND e.not_sent_at IS NULL;
   ```

   The payload holds only identifiers and digests, no credential.
2. **Read the log and the metric.** The last
   `merge reconciliation: effect=merge outcome=<outcome>` line names why the
   reconciler kept the quarantine.
3. **Inspect the forge read-only** (`gh pr view <n> --json state,merged,mergeCommit,headRefOid,baseRefName`,
   then `gh api repos/<repository>/git/commits/<merge commit>` for its tree, parents
   and message):
   - **Merged, but not exactly as planned** (`mismatch`): for example a squash or
     rebase merge, a manual merge with another message, or a merge after `main`
     moved, so the first parent is not the planned base. The change is on `main`,
     but not as the gate authorized it. Review the merge commit, then decide.
   - **Open, but `main` already contains the head** (`mismatch`): GitHub compares
     `main` with the head as `behind` or `identical`, so the head reached `main`
     by another route (for example a manual merge of the branch). Find that
     merge before you decide.
   - **Closed without merge, or a draft** (`mismatch`): someone acted on the pull
     request. Confirm with the repository's activity or audit log that no merge
     happened.
   - **Head moved** (`mismatch`): someone pushed to the runtime's branch. Do not
     merge it.
   - **`unavailable`**: check the publication credential's access to the
     repository (a `403` or `404` on the pull request is never read as absence),
     GitHub's status and the release manager's egress.
   - **`unplanned`**: the journaled command does not re-derive the journaled
     digest, which indicates a changed transport or a damaged row. Escalate; do
     not resolve it by hand.
4. **Record the decision by hand**, in one transaction of the administrator
   session:
   - The pull request is merged at the planned head: write the receipt in the
     format above, with `receipt_digest` the SHA-256 of the exact receipt bytes,
     and set `completed_at`. Use `UPDATE ... WHERE request_id=<id> AND token=<token>
     AND completed_at IS NULL AND not_sent_at IS NULL`.
   - The forge proves that nothing was merged: set `not_sent_at` with the same
     guard.

   Never change the request's state and never insert a merged record: the
   request stays failed, exactly as after an automatic reconciliation.
5. **Confirm.** The next merge poll reports `merge-reconciled` or selects the
   next publication, and `ai_sdlc_quarantined{journal="merge"}` returns to `0`.
6. **Follow up the issue.** For a merged pull request, the issue is closed by the
   merge for manifest repositories; the build and deployment stages do not run
   for it (see above). For a pull request that was not merged, close it, delete
   its branch and re-label the issue.

## Safety argument

- **No forge write.** Reconciliation has no write transport. `mergewriter.Reader`
  can only `GET`. The tests fail on any other method and count zero writes.
- **Positive evidence only.** A receipt needs the merged pull request and its
  merge commit to match the plan field by field. A no-send needs an open pull
  request at the planned head over an unchanged `main`, or a comparison that
  shows the head outside a moved `main`. Every error, including a `404` or
  `403`, keeps the quarantine.
- **No weakened fence.** Recording an outcome takes the merge claim lock and, in
  one transaction, re-checks that no merge is claimed, that the request is still
  `failed`, settled and carries the same signed payload, and that the effect is
  the same uncertain intent (token and request digest). A resolved effect can
  never change again; a second resolution reports `resolved`.
- **No new authority.** The request stays failed and no merged record is
  written, so nothing downstream starts for the reconciled change itself. Its
  code is on `main`, though, and ships inside the next merged change's tree,
  without a provenance record of its own.

## Configuration and migration

There is no new setting, migration or database privilege. The reconciler uses
the installed publication credential (read endpoints only) and the existing
release-manager column grants on `merge_effects`.
