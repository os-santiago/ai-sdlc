# Single-send merge transport

`internal/mergewriter` is an inactive transport for the Homedir pilot. It is not
an installed controller, external input endpoint or source of merge authority.
An operational controller must provide a fresh signed capability, an exclusive
journal claim, current issue/policy evidence and authenticated merge gates.

The transport accepts only a validated AI-SDLC branch and fixed Homedir PR. It
sends one `PUT /pulls/{number}/merge` with an exact expected head SHA, the `merge`
method, a fixed commit title containing `[skip release]` and the fixed message
`Refs #<issue>`. The message has no `Signed-off-by` trailer: GitHub sets the merge
commit's author and committer from the credential's account, so a trailer naming
any other identity would attest a sign-off by someone who did not create the
commit. The merged change keeps the publication commit's sign-off by the
installation's [commit identity](publication-writer.md#commit-identity). Its caller must
verify applicable strict required checks and no bypass actors immediately before
sending. The GitHub API has no base-SHA precondition; native strict protection
is necessary to prevent a concurrent base update from bypassing validation.

The trusted pre-send callback receives a digest of method, path and JSON body.
It must commit an effect intention before returning success. The transport then
requires a synchronous successful merge response and reads the returned commit
once. Its tree must match the reviewed candidate, and its parents must be exactly
the expected base and head, in that order. The same digest reaches the post-send
callback before a receipt is returned.

Every ambiguous response remains an unknown effect. There are no HTTP retries,
redirects, environment proxies, ref updates, async merge adoption, issue closure,
release or deployment operations. Errors omit credentials and server payloads.
The caller owns a bounded deadline; the transport cannot create authority for an
unbounded caller. Each request additionally waits at most 20 seconds for response
headers and 25 seconds in total, inside the controller's claim deadline. GitHub
answers a merge only after it has written the merge commit and moved the base,
which took up to about five seconds in battery 39; a shorter deadline turns a
merge GitHub completed into an unacknowledged effect that quarantines every later
merge.

TLS fixture tests cover request binding, commit identity, missing/duplicate or
oversized response fields, connection loss, redirects, conflicts and revocation
at either callback. These tests exercise real HTTP/TLS locally; they do not merge
a live GitHub PR or prove the autonomous E2E.
