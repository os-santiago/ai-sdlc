# Source availability before execution

The worker acquires and verifies the immutable public GitHub source before it
claims an execution. A quota or transport failure leaves the signed request
queued with its original expiration. It creates no Kubernetes workload or model
reservation, extends no lifetime, and never reopens a stopped attempt.

A process-local cache holds one verified bundle keyed by repository and full
commit SHA. Returned copies cannot mutate later reads. It retains the existing
Git manifest/archive checks and bounded bundle codec. No credential or disk cache
is introduced. Current policy, issue admission, profile and A-Dev context are
checked before and after source acquisition; claim and preparation revalidate
again before any workload effect.

HTTP 403 with exhausted quota, a `Retry-After` or a "secondary rate limit" message
(GitHub's secondary limit) and
HTTP 429 are reported as `source-rate-limited`.
Bounded `Retry-After` or rate reset metadata suppress repeated anonymous requests
across source keys; other unavailable reads back off ten seconds for that key.
An already verified bundle remains readable during backoff. Backoff is scheduling
metadata only. Restart loses the cache; it does not replay uncertain work.
`source-unavailable` and `source-preflight-authority` distinguish other read and
authority failures without exposing provider response bodies.

This change does not retry the stopped Homedir attempts. A fresh eligible issue
input remains necessary for new work after those attempts. It does not complete
publication, production promotion or the E2E demonstration.

Conclusive signature/profile/current-generation mismatches retire the queued
request without taking a claim, allowing the next valid request to progress.
Unavailable observations, canceled reads, and failed configuration loads defer
instead. This distinction is exercised with two signed queued requests, including
an obsolete generation followed by current work and a database permission outage.

## Verification and review reads

Independent verification and review fetch the same source again. By default
they read anonymously, like the worker's preflight, and draw on the forge's
anonymous per-address quota (60 requests per hour on GitHub). An installation
can give them a read credential instead; see
[Authenticated verification and review reads](#authenticated-verification-and-review-reads).
When the quota is exhausted, the stop is named, so it cannot be mistaken for
a defect:

- verification stops at `source-rate-limited` (or
  `cleanup-after-source-rate-limited`);
- review stops at `preparation-source-rate-limited`.

Each anonymous read spends two API calls, for the commit and its recursive
tree, then downloads the pinned archive from the fixed archive host. That
download is not counted against the API quota, and the archive is verified
against the tree manifest. A read with a credential asks the API for the
archive redirect, because only that redirect carries a private-archive ticket.

These stops end the attempt like any other source failure. Wait for the reset,
reduce the deliveries that share one egress address, or install a read
credential. An authenticated reader spends one more API call per read (the
archive redirect) but draws on the credential's own quota (5,000 requests per
hour for a GitHub token) instead of the shared anonymous one.

## Authenticated verification and review reads

The verifier can read source with a read-only credential scoped to the
configured repository (#741). It is optional: without it, nothing changes.

**Least privilege.** The installation's existing GitHub clients are not
reused. `ai-sdlc-github-client` and `ai-sdlc-publication-client` hold an
account session token that the installer requires to have write access to the
repository and that usually reaches every repository of the account. Mounting
it in the verifier would give the component that runs alongside untrusted
candidate code a write capability it never needs. The source reader is
therefore a separate Secret, `ai-sdlc-source-reader`, holding a token that
can only read the one configured repository: for example a fine-grained
personal access token restricted to that repository with only the
*Contents: read* permission (*Metadata: read* is implied). A GitHub App
installation token with the same scope also works if an external secret
operator refreshes the Secret before the token's one-hour expiry; the
verifier re-reads the file for every fetch.

Note that the worker's own preflight and execution preparation still read
anonymously: `execution.Prepare` accepts a credential, but the worker passes
none. Only verification and review use this credential.

**Install.** Create the Secret in the release namespace, then install or
upgrade:

1. Write the document described in the
   [Secret contract](helm-chart-secret-contract.md#key-formats) to a private
   file, for example with `access: "read"` and the repository exactly as in
   the installation's policy.
2. `kubectl -n ai-sdlc-runtime create secret generic ai-sdlc-source-reader
   --from-file=credential.json=<file>`, then annotate it with
   `ai-sdlc.io/cluster-uid=<UID of the kube-system namespace>` and label it
   `app.kubernetes.io/part-of=ai-sdlc`. Delete the private file.
3. `./install.sh local --profile verification ...` finds an owned, valid
   Secret and sets `verifier.sourceCredential: true`. It never creates,
   imports or prints the token, and it refuses a Secret without the
   installation's annotation or with any other shape. A chart-only
   installation sets `verifier.sourceCredential: true` itself.

The upgrade tools carry the recorded value: an installation recorded without
it renders exactly as before; one recorded with it keeps the mount.

**Custody.** The Secret is mounted read-only (mode 0440) only into the
verifier controller container, at `/var/run/ai-sdlc/source/credential.json`;
the verifier accepts no other path. It is never mounted into the review model
gateway sidecar, and never into the verification, test or reviewer pods or the
ConfigMaps the verifier creates, which are built from fixed specifications.
The token is read from the file for each fetch, kept in memory only for that
fetch, sent only as the `Authorization` header of fixed-host API GETs for the
credential's own repository, and never sent to the archive host. It never
appears in prompts, logs, metrics, errors, proofs or reports: errors and stop
names are bounded constants. No RBAC rule is added; the kubelet mounts the
Secret.

**Stops.** A configured credential never falls back to an anonymous read:

- missing, malformed, or bound to another repository: the verifier is not
  ready, verification stops at `source-credential-invalid` and review at
  `preparation-source-credential-invalid`. Both stops happen before the work
  is claimed, so no queued delivery is consumed; the poll cycle stops there,
  as for any review preparation stop, until the Secret is repaired;
- refused by the forge (HTTP 401, or 403 that is not a rate limit):
  verification stops at `source-credential-rejected` (this is observed by
  the fetch, after the claim, so that attempt ends) and review at
  `preparation-source-credential-rejected`. Neither is retried.

Replace the token in the Secret; the next poll uses it without a restart.

Two rarer cases:

- The pre-claim check and the fetch read the file separately. A Secret
  rotated into a bad state between the two makes the fetch stop at
  `source-credential-invalid` after the claim, so that one attempt ends.
- The Secret volume is not `optional`. If the Secret is deleted while
  `verifier.sourceCredential` is `true`, a restarted or rescheduled verifier
  Pod stays in `ContainerCreating` until the Secret exists again or the value
  is set back to `false`. This fails safe: nothing reads source anonymously.

A rate limit on the credential's own quota still stops at the
`source-rate-limited` names above. GitHub's secondary rate limit is
classified as a rate limit too, not as a rejection: a 403 or 429 with
`Retry-After`, or a 403 whose message contains "secondary rate limit" (case
ignored; at most 4 KiB of the error body is searched for that phrase, and the
body is never logged or returned).

## Conformance source authentication

Quality workflows supply their read-only `github.token` only to the trusted
conformance step. Test drivers explicitly bind it to the Homedir source reader's
context. Production code does not discover that test environment variable.
The credential is used only for fixed-host API GETs; archive requests, generated
Pods, fixture control documents and model subprocesses do not receive it.

Treat GitHub tokens as opaque strings. The source reader accepts a bounded
header-safe value up to 4096 bytes, including dots and hyphens; it never parses or
trusts JWT claims. [GitHub's stateless installation-token rollout](https://github.blog/changelog/2026-10-02-stateless-github-app-installation-tokens-rolled-out/)
made the earlier 512-byte/alphanumeric-only assumption incompatible with Actions.
