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

HTTP 403 with exhausted quota and HTTP 429 are reported as `source-rate-limited`.
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

Independent verification and review fetch the same public source again,
without a credential (#741). These reads draw on the forge's anonymous
per-address quota, as the worker's preflight does. When the quota is
exhausted, the stop is named, so it cannot be mistaken for a defect:

- verification stops at `source-rate-limited` (or
  `cleanup-after-source-rate-limited`);
- review stops at `preparation-source-rate-limited`.

Each anonymous read spends two API calls, for the commit and its recursive
tree, then downloads the pinned archive from the fixed archive host. That
download is not counted against the API quota, and the archive is verified
against the tree manifest. A read with a credential asks the API for the
archive redirect, because only that redirect carries a private-archive ticket.

These stops end the attempt like any other source failure. Wait for the reset,
or reduce the deliveries that share one egress address.

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
