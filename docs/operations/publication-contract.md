# Publication handoff foundation

`internal/publication` defines the bounded `verifier → release-manager`
`publish-candidate` command and a pure Git tree plan. It is not installed as a
route, controller or forge writer. The current verification profile grants no
publication operation and still stops at independent behavior evidence.

The signed command binds the verification request, candidate, work item, issue
revision/generation, source commit, exact delta/tree, behavior proof, independent
review proof and reviewer profile. Missing review evidence cannot be waived by
adding a field. Repository, authority revision and expiry must match current
policy; only the verifier may sign this operation for the release manager.
Publication may not extend the original verification expiry.

These functions do not attest that a database row, review or current issue exists.
The remaining controller must independently read those records, validate the
original signatures, enforce current policy and journal each external effect.
A hash-shaped value, a pure contract test or a signed destination alone must never
be treated as a completed audit or permission to merge.

## Exact Git tree planning

The initial plan supports only the reviewed Homedir locale recipe. It reconstructs
the full source and candidate, preserves every other file and accepts only the
two prescribed regular UTF-8 files. Canonical Git blob/tree identities are
computed directly without running attributes, filters or hooks. SHA-256 remains
the evidence binding; SHA-1 is used only for Git object compatibility.

A future writer must send the verified base tree plus those two modifications,
then compare GitHub's returned tree against the independently computed candidate
Git tree. The branch is derived from the issue and immutable candidate identifier.
The fixed commit message includes `[skip release]` for the local-kind pilot;
merge and release controllers must also enforce that environment boundary.
See GitHub's [Git trees API](https://docs.github.com/en/rest/git/trees#create-a-tree)
for the `base_tree` and UTF-8 content semantics. This plan performs no API call.

## Reproduce the local Git object comparison

Build `go test -c -o /private/publication.test ./internal/publication` from
`runtime/k8s`, then run:

```bash
python3 ../../tools/verify_publication_tree.py \
  --source-repository /path/to/isolated-homedir \
  --source-sha 0622cdaeb59a11a27bd5f3d7e18183c401a5e412 \
  --candidate-directory /private/prepared-verifier-fixture \
  --test-binary /private/publication.test
```

Prepare the two candidate files with `../../tools/prepare_verification_fixture.py`.
The check compares the Go plan with real `git hash-object`, `read-tree`,
`update-index` and `write-tree` results in a disposable bare repository.
The source checkout is read-only, the procedure has no credential input and
performs no push, PR creation, merge or runtime advancement.
