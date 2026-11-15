# Bounded GitHub publication transport

`internal/publication.Writer` is an inactive transport for the Homedir pilot.
Only a trusted runtime controller may use it after committing a fresh durable
intent and validating current signed issue, candidate, behavior and review
proofs. A `TreePlan` and a successful HTTP response never grant authority by
themselves. No installed route, credential mount or grant activates this writer.

The finite sequence creates a tree, commit, new branch and pull request. It
accepts only the two regular UTF-8 files in the reviewed locale recipe, checks
GitHub's resulting tree SHA, binds the commit to its original parent/tree, and
checks the returned PR repository, branch, head and base SHA. Commit authorship
and signoff are fixed to Sergio Canales <sergio.canales.e@gmail.com>. Both commit
message and PR title retain `[skip release]`; the issue is referenced rather than
closed before the local production-test promotion completes.

Each operation performs one bounded HTTP/1 POST to the fixed GitHub API, with
no proxy, redirects, transport replay or response-body logging. Current authority
is checked immediately before and after the request. A lost response, changed
receipt, HTTP failure or revoked fence yields no receipt; the caller must mark
its journal uncertain. The writer cannot adopt an existing branch, overwrite a
ref, merge, release or deploy. A future merge stage must independently preserve
`[skip release]` and validate checks, unresolved reviews and current authority.

The API mapping follows GitHub's documented [tree creation](https://docs.github.com/en/rest/git/trees#create-a-tree),
[commit creation](https://docs.github.com/en/rest/git/commits#create-a-commit),
[reference creation](https://docs.github.com/en/rest/git/refs#create-a-reference)
and [pull request creation](https://docs.github.com/en/rest/pulls/pulls#create-a-pull-request).
Tests use actual TLS servers; they do not publish fixture PRs to Homedir.
