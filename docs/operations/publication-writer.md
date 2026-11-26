# Bounded GitHub publication transport

`internal/publication.Writer` is an inactive transport for the Homedir pilot.
Only a trusted runtime controller may use it after committing a fresh durable
intent and validating current signed issue, candidate, behavior and review
proofs. A `TreePlan` and a successful HTTP response never grant authority by
themselves. No installed route, credential mount or grant activates this writer.

The finite sequence creates a tree, commit, new branch and pull request. It
accepts only the two regular UTF-8 files in the reviewed locale recipe, checks
GitHub's resulting tree SHA, binds the commit to its original parent/tree, and
checks the returned PR repository, branch, head and base SHA. The commit's author,
committer and `Signed-off-by` trailer are the plan's
[commit identity](#commit-identity), and the writer accepts only the commit whose
SHA is the Git object identity of exactly the commit it requested. Both commit
message and PR title retain `[skip release]`; the issue is referenced rather than
closed before the local production-test promotion completes.

## Commit identity

The identity is installation configuration, not a property of the product. It
is the optional `publication.commitIdentity` of the installed
[project manifest](project-manifest-installation.md):

```json
"publication": {
  "allow": ["**/*.go"],
  "maxChangedFiles": 5, "maxChangedBytes": 65536, "maxFileBytes": 32768,
  "commitIdentity": {"name": "Example Delivery Bot", "email": "delivery-bot@example.org"}
}
```

Without it, commits use the neutral default
`ai-sdlc[bot] <noreply@ai-sdlc.invalid>`. The reference repositories (Homedir,
Saferus) are governed only by their built-in manifests, which an installation
cannot replace, so they always use the default. The reserved
`.invalid` domain never resolves, so the address cannot be delivered to or
attributed to a real person or forge account. To attribute commits to the
GitHub account or App that owns the publication credential, set its no-reply
address (for example `ID+login@users.noreply.github.com`).

A name is 1 to 100 bytes, without `<`, `>`, control, format, line-separator or
paragraph-separator characters, and cannot begin or end with Unicode white space
or `. , : ; " \ '`, which Git (or the forge) would trim; the
email is a plain `local@domain.tld` address of at most 254 bytes. The migrator
refuses a manifest with any other identity.

The identity is bound like the other publication settings:

- The installer binds the manifest digest into the authority policy
  (`projectSHA256`), so the identity is part of the authority revision every
  signed publication command carries. Omitting the section keeps the digest of
  existing manifests unchanged.
- The controller reloads the policy whose revision the command carries and
  passes its `projectSHA256` to `PrepareTree`, which accepts only the installed
  manifest with exactly that digest (`project.Bound`). A concurrent reload that
  fails, or that installs a different manifest, refuses the plan instead of
  falling back to a built-in manifest or another manifest's scope and identity.
- `PrepareTree` resolves the identity into the `TreePlan`; the plan digest that
  the publication claim records, the commit request digest that the effect
  journal records before the POST, and the planned commit SHA all derive from
  the same `commitBody` builder.
- A publication keeps the identity it was planned with: the writer reads it from
  the plan, never from the current installation. Changing the identity is a
  manifest reconfiguration, which changes the authority revision; queued
  commands issued under the old revision fail their authority check and are
  not published, and an effect already in flight stays bound to its recorded
  plan and request digests.

### Rollback

The manifest is decoded strictly: unknown fields are refused. An image older
than this change does not know `publication.commitIdentity`, so once a manifest
sets it, rolling back to such an image fails closed. Every component's
authority load refuses the manifest, and no component becomes ready or acts.
To roll back, first reinstall the manifest without `commitIdentity` (a reviewed
reconfiguration that changes its digest and the authority policy), then roll
back the images. Manifests that never set the field are unaffected.

Each operation performs one bounded HTTP/1 POST to the fixed GitHub API, with
no proxy, redirects, transport replay or response-body logging. Current authority
is checked immediately before and after the request. Both fences receive the
SHA-256 of the exact serialized HTTP request, allowing the controller to commit
and validate that same body binding before the single POST. A lost response, changed
receipt, HTTP failure or revoked fence yields no receipt; the caller must mark
its journal uncertain. The writer cannot adopt an existing branch, overwrite a
ref, merge, release or deploy. A future merge stage must independently preserve
`[skip release]` and validate checks, unresolved reviews and current authority.

Its read-only counterpart, `internal/publication.Reader`, serves only
[reconciliation](publication-reconciliation.md). It issues bounded `GET`
requests to fixed tree, commit, ref and pull request list paths, with the same
transport limits. It has no write method. The planned request bodies that
reconciliation re-derives (`publication.Expect*`) come from the writer's own
body builders. Each observation is therefore tied to the exact journaled request
digest.

The API mapping follows GitHub's documented [tree creation](https://docs.github.com/en/rest/git/trees#create-a-tree),
[commit creation](https://docs.github.com/en/rest/git/commits#create-a-commit),
[reference creation](https://docs.github.com/en/rest/git/refs#create-a-reference)
and [pull request creation](https://docs.github.com/en/rest/pulls/pulls#create-a-pull-request).
Tests use actual TLS servers; they do not publish fixture PRs to Homedir.
