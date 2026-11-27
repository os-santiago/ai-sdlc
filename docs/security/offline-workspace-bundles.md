# Offline workspace bundles: hidden file-transfer prerequisite

An offline executor cannot clone source without additional network authority.
The `model-mailbox` image therefore includes a fixed `/workspace-transfer`
helper for importing bounded source bytes and exporting untrusted regular files.
Issue #465 adds this hidden prerequisite; no live Job controller, provider,
publishing route, role grant or delivery transition is enabled.

## Contract and limits

A JSON bundle has exactly `version`, `sourceSHA` and `files`. Version is
`ai-sdlc.workspace-bundle/v1`; sourceSHA is a lowercase 40-character commit ID.
Each file has exactly `path`, `mode` and `data` (canonical base64). Only file modes
420 (`0644`) and 493 (`0755`) are accepted. Object keys are case-sensitive and
unique, nulls and extra fields fail, and exactly one JSON document is permitted.
The parser checks encoded data size before allocating decoded bytes.

| Limit | Maximum |
| --- | ---: |
| Wire bytes | 192 MiB |
| Decoded file bytes in total | 128 MiB |
| Individual file | 4 MiB |
| Files | 16,384 |
| Files plus distinct directory prefixes / visited nodes | 32,768 |
| Path bytes / segment bytes / depth | 1,024 / 255 / 32 |
| Helper process, including blocked stdin/stdout/stderr | 60 seconds |

The bounds were raised from 32 MiB / 16 MiB / 4,096 / 8,192 for large
repositories; [Large repositories](../operations/large-repositories.md) lists
where size matters and what holding a tree costs. The encoder writes exactly
the bytes `json.Marshal` produced, so digests are unchanged.

Paths must be relative normalized POSIX paths with valid UTF-8, without control
characters, replacement runes, backslashes, `.`/`..`, empty segments or any
case-variant `.git` segment. Duplicate paths and file/directory conflicts fail.
The entire input tree, including implicit directories, is validated before any
filesystem mutation. Empty directories are omitted: this is a Git-style file
snapshot, not a filesystem backup.

## Fixed helper boundary

```text
/workspace-transfer import --pod-uid <assigned-uid> --source-sha <commit> --digest <sha256>
/workspace-transfer export --pod-uid <assigned-uid> --source-sha <commit>
```

Neither command accepts a path, URL, shell fragment or credentials. Both compare
the assigned UID with the read-only Downward API projection at
`/var/run/ai-sdlc/pod/uid`. Import streams bounded stdin: it holds the paths and
one decoded file at a time, never the whole document or tree, so its memory does
not grow with the repository. It refuses to start when `/workspaces/repository`
exists, and writes each file into the fixed staging directory
`/workspaces/.repository-import` with directory descriptors, no-follow opens,
exclusive file creation and permissions applied by descriptor. Only after the
whole document parsed, every bound and tree check passed, the exact wire SHA-256
matched and the document named the expected source commit is the staging
directory renamed to `repository`. A refused or partial import leaves at most
the staging directory: it is a failed, non-reusable workspace; discard the owned
Pod/volume instead of retrying over it. Success is reported only after all file
writes, syncs and the rename.

Export opens that fixed directory with no-follow traversal. It enumerates names
in batches, counts all visited nodes, and inspects each through its directory
descriptor. Visible links, special nodes, multiple hard links, oversized files
and special permission bits fail. This listing pass reads no content, so every
refusal reason is known before the first output byte. The files are then read
in path order and streamed to stdout one at a time: each is reopened component
by component without following links, and its identity, owner, size,
modification/change times and link count must equal the listing's before and
after reading. A file changed in between fails the export with a nonzero exit,
which voids the partial output.
Root `.git` is never opened or exported. Regular-file permissions are normalized
to Git's owner-executable bit. The helper does not configure providers or Git;
project configuration files may travel as untrusted data, and future executors
must prevent them from increasing authority. Error output contains no file
content or path names.

These checks do not make an active tree an atomic snapshot. A future controller
must independently verify that the assigned executor container and its writers
terminated, that PID namespaces were not shared, and that Pod/image/template
identity still matches. This hidden helper cannot establish those facts itself.

## Candidate authority and filesystem visibility

Every exported byte is untrusted. The supplied `sourceSHA` is context echoed by
the helper, **not proof that the candidate matches that commit**. The external
controller must retain trusted source identity, compare candidate paths/content/
executable bits outside the executor, enforce allowed scope, and run independent
verification before publication. Repository instructions and claimed test results
cannot grant authority. This change does not initialize Git or run repository code.

Real testing found that a FIFO created in the worker was visible there but absent
from the mailbox's view of the shared volume. The pinned gVisor implementation
falls back to an in-memory synthetic pipe when the gofer rejects its creation;
see [MknodAt in the pinned source](https://github.com/google/gvisor/blob/a0e6be6fb5e03d8ebea13592f36a097c7e9a41b9/pkg/sentry/fsimpl/gofer/filesystem.go).
The runtime's host-FIFO restriction remains unchanged. A synthetic private FIFO
is not an exported candidate file. Visible FIFOs are rejected by the helper;
the cross-container conformance test separately verifies a real FIFO in the
worker and exact unchanged regular-file output from the mailbox. It does not
claim that an invisible node was rejected or that the two views are full
filesystem backups.

## Reproducible validation

Run `go test ./internal/workspacebundle ./components/model-mailbox/workspace-cmd`
from `runtime/k8s`. Tests exercise exact bytes/modes, malformed JSON/base64,
paths/tree conflicts, implicit node counts, long segments, limits, symlinks,
hard links, FIFOs, special modes, empty-directory/depth bounds and blocked pipes.

Build the mailbox image, load it into an installer-owned kind cluster with the
verified offline runtime, and run from the repository root:

```sh
python3 tools/verify_workspace_bundle.py \
  --kubectl /path/to/verified/kubectl \
  --kubeconfig /path/to/installer/kubeconfig \
  --mailbox-image localhost/ai-sdlc-model-mailbox:conformance \
  --source-repository /path/to/clean-homedir-clone \
  --source-sha 0622cdaeb59a11a27bd5f3d7e18183c401a5e412
```

The fixture reads immutable Git objects without running repository code, creates
a disposable restricted gVisor Pod and tests invalid imports and hostile nodes.
The worker independently reads every imported path, byte and executable bit.
It creates, modifies, deletes and changes permissions only in a new synthetic
fixture directory; Homedir source files remain unchanged. After observing writer
termination, the fixture independently compares the exported candidate with the
exact expected tree, including those deterministic mutations.
Cleanup uses namespace UID preconditions. The report includes observed image IDs,
source and bundle identities, counts, limits tested and
`autonomousExecutionEnabled: false`. CI repeats this with the pinned public
Homedir source on kind/Docker. This is file-transfer evidence, not a generated
improvement, application test, release or issue-to-production E2E run.

The next boundary is the [fixed SCC executor launcher](offline-executor-launcher.md),
which verifies source identity in the worker view before starting repository tools.
