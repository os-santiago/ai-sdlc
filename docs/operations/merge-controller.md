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

A missing response or uncertain commit stops the original claim and quarantines
an unacknowledged effect. Reconciliation cannot adopt that claim, repeat the write
or give the same publication a new attempt. A terminal merge fact does not grant
artifact promotion, release or deployment authority.

The current PostgreSQL fixture uses synthetic forge observations and receipts.
It validates journal/controller transitions, not a live GitHub merge or the
issue-to-production E2E. Installation must separately enable the finite merge
grant, least-privilege columns and native strict branch protection without bypass.
