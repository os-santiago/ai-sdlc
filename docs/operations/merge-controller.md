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
head-conditioned merge and verifies its returned commit. Current issue/policy and
journal fences precede acknowledgement and immutable terminal completion.

A missing response or uncertain commit stops the original claim and quarantines
an unacknowledged effect. Reconciliation cannot adopt that claim, repeat the write
or give the same publication a new attempt. A terminal merge fact does not grant
artifact promotion, release or deployment authority.

The current PostgreSQL fixture uses synthetic forge observations and receipts.
It validates journal/controller transitions, not a live GitHub merge or the
issue-to-production E2E. Installation must separately enable the finite merge
grant, least-privilege columns and native strict branch protection without bypass.
