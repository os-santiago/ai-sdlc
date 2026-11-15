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
