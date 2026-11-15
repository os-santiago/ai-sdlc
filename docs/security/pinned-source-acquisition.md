# Pinned source acquisition

`internal/sourcefetch` obtains one configured GitHub repository at an exact
40-character commit SHA and returns the existing regular-file workspace bundle.
It neither runs Git nor extracts archive paths into the host filesystem. It
grants no execution authority and is not installed as a service or live route.

## Acquisition and integrity chain

1. Read the immutable commit through the fixed GitHub Git API and require its
   returned commit identity to match the requested SHA.
2. Read the reported root tree recursively. Reject a truncated or oversized
   manifest, missing parents, duplicate paths, links, submodules and unsupported
   file modes.
3. Request that commit's tar archive. Accept exactly one explicit HTTP 302 to
   `codeload.github.com`, with the configured repository and exact SHA in its
   path. Create a fresh download request without the API bearer credential.
4. Decode a single bounded gzip member and tar archive into regular files.
   Require Git archive's global PAX commit comment to match the requested SHA.
5. Compare the exact file set, modes, sizes and each Git blob object hash with
   the manifest. Reconstruct every directory's Git tree object, including the
   reported root, using Git's directory/file ordering.

The API contract is documented in GitHub's
[archive endpoint](https://docs.github.com/en/rest/repos/contents#download-a-repository-archive-tar)
and [tree endpoint](https://docs.github.com/en/rest/git/trees#get-a-tree).
Object hashes follow the [Git object format](https://git-scm.com/book/en/v2/Git-Internals-Git-Objects).
The existing Git SHA-1 object identity is a protocol compatibility requirement;
it is not a commit signature, build attestation or SLSA claim.

The commit-to-root-tree mapping trusts the authenticated GitHub API over
verified TLS. The package does not reconstruct the commit object or verify its
signature. The archive's PAX comment alone is not proof of content integrity.
Archive export exclusions, substitutions and expanded LFS content are rejected
when they differ from the reported Git tree. There is no silent partial checkout.

## Transport and archive limits

| Boundary | Limit |
| --- | --- |
| Whole fetch, including four requests and validation | 30 seconds |
| Connection / TLS handshake / response headers | 3 / 3 / 5 seconds |
| Response headers / archive redirect URL | 8 KiB / 4 KiB |
| Commit JSON / recursive tree JSON | 1 MiB / 8 MiB |
| Compressed archive / expanded tar | 32 MiB each |
| Workspace content / individual file | 16 MiB / 4 MiB |
| Files / filesystem nodes / path depth | 4,096 / 8,192 / 32 |

The client uses verified TLS 1.2 or later, fresh HTTP/1 connections, no ambient
proxy, automatic redirects, compression or retry. Errors are fixed and exclude
credentials, response bodies and signed archive ticket URLs. The API bearer is
never copied to the archive request. Archive tickets remain only in memory.
Private repositories require a separately scoped read credential; the real
validation here covers public Homedir only, not private-repository ticket flows.

Archive validation rejects traversal, Git metadata, links, devices, sparse or
unsupported nodes, duplicate/file-directory conflicts and special file modes.
It requires the actual two zero end blocks, allowing only the previous file's
expected padding before them and zero padding afterward. Concatenated gzip
members and dangling PAX/GNU metadata are rejected. File-local PAX attributes
are conservatively unsupported, so some legitimate archives will fail closed.

GitHub's regular-file modes 0664/0775 are normalized to Git's executable identity
as workspace modes 0644/0755. Empty directories are not represented by the
workspace bundle. Bounds are deliberately finite; repositories outside this
contract require an explicitly reviewed extension rather than larger implicit
budgets or omission of files.

## Validation

The offline suite uses actual HTTP/TLS sockets, hostile archives and an
independent `git write-tree`/`git ls-tree` vector containing binary and executable
files plus directory/file ordering cases:

```sh
cd runtime/k8s
go test -count=1 ./internal/sourcefetch ./internal/workspacebundle
```

An explicit optional read-only network test fetches public Homedir commit
`0622cdaeb59a11a27bd5f3d7e18183c401a5e412` and compares all 1,158 files with the
tree digest independently exported from its clean checkout:

```sh
AI_SDLC_TEST_GITHUB_SOURCE=1 go test -count=1 \
  -run TestActualPublicGitHubPinnedSource -v ./internal/sourcefetch
```

The validated workspace digest is
`a392d4d3be50cc89cb1cc324fb736952fe9e86b47e5d131912163d363c147277`.
Generic CI skips this external read unless requested. This is source acquisition
evidence, not an autonomous Homedir change, candidate verification or release.
The future controller must still establish current signed execution authority,
admission/work-item ownership, isolation, quotas and independent delivery gates.
