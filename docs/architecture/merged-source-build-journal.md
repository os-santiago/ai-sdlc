# Merged-source build journal

The build journal is an inactive outerloop primitive. It does not start a
controller, provision storage, create a Tekton run, publish an image, or authorize
a release. Issues remain the only external work input.

A build request binds the actual merge commit and tree, immutable publication
and merge facts, the current issue generation and revision, both loop policies,
and the installed recipe and storage inventory. The release manager signs the
request; only the distinct build-controller database role can queue and claim
it. Historical merge evidence never renews authority.

## Fences and storage

Each claim and effect rechecks the signed deadline, current policies, current
issue eligibility and observation freshness, intake configuration and exact
installed storage custody. The deadline is fixed and cannot be extended by a
queue wait or restart. Row-lock waits are followed by another authority check.

A physical storage identity is consumed once. Its allocation key remains the
same when Kubernetes cluster, node, namespace, PV or PVC identities change;
those identities are separately bound into the custody digest. The future
installer must derive the physical identity from observed storage custody, not
invent a new identifier for an existing workspace. The only supported inventory
shape is one to eight dedicated 4 GiB tmpfs slots with 32768 inodes each.

The journal writes a unique create intention before an external request. The
original acknowledged Kubernetes UID can be recorded once. A lost create reply
must stop and quarantine the request and slot; deterministic names are not
permission to retry, adopt an object, or recycle storage. No reclamation API is
provided. Database grants prevent the component from rewriting signed requests,
immutable source records or allocated storage.

## Validation and remaining integration

The tests use PostgreSQL with the actual publication/review/merge fixture chain
and synthetic forge responses. They cover concurrent claims, stale or changed
issue/policy/configuration, immutable provenance, database role isolation,
unknown-create quarantine, Kubernetes identity replacement over consumed storage,
and expiry while waiting for a database lock. They are not autonomous E2E proof.

Before activation, the runtime still needs observed storage installation,
a closed build recipe compiler and admission checks, component-only scheduling
and transport, bounded Kubernetes execution, cleanup that remains available after
authority revocation, build completion and provenance verification, and separate
promotion/deployment authority. The existing isolated Konflux fixture supplies
integration evidence for the recipe; it does not exercise this journal.
