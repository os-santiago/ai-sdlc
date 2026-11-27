# Large repositories: workspace bounds, memory and storage

AI-SDLC moves a repository between the forge, its controllers and the sandboxed
pods as a [workspace bundle](../security/offline-workspace-bundles.md). Its
bounds decide which repositories the runtime can take on. The earlier bounds
(16 MiB of content, 4,096 files) refused real services such as
`quarkus-super-heroes`: a 27 MB archive of 36.8 MB in 1,287 files. This page
records the current bounds, every place where the size of a workspace matters,
and what holding one costs.

## Bounds

The bounds are compile-time constants of the codec, not installation settings:
every component, helper image and database check agrees on them.

| Bound | Before | Now | Where |
| --- | ---: | ---: | --- |
| Decoded content | 16 MiB | 128 MiB | `workspacebundle.MaxContentBytes` |
| Wire document (JSON, base64) | 32 MiB | 192 MiB | `workspacebundle.MaxWireBytes` |
| One file | 4 MiB | 4 MiB | `workspacebundle.MaxFileBytes` |
| Files | 4,096 | 16,384 | `workspacebundle.MaxFiles` |
| Files plus directories | 8,192 | 32,768 | `workspacebundle.MaxNodes` |
| Compressed source archive | 32 MiB | 96 MiB | `sourcefetch.MaxArchiveBytes` |
| Expanded tar stream | 32 MiB | 161 MiB | `sourcefetch.MaxExpandedArchiveBytes` |
| Export WebSocket messages | 8,192 | 49,152 | `podtransport.exportFrames` |
| Recorded source and export size | 33,554,432 | 201,326,592 | migration `0041_large_workspace_bounds.sql` |

The wire bound is the base64 of the content bound (4/3) plus about 1.3 KiB of
JSON per file at the file bound. The expanded tar bound is the content bound
plus, per node, a 512-byte header and under 512 bytes of padding, plus 1 MiB
for the global header, end blocks and record padding. Path rules (1,024 bytes,
255 per segment, depth 32), modes, the per-file bound and the candidate delta
bound (2 MiB, 64 changes) are unchanged.

The reference repository fits with room: `os-santiago/quarkus-super-heroes` at
`ae00455c01a7a50d89ae8f1db99483abd27abe25` fetched and verified from GitHub in
4.4 s as 1,287 files and 36,795,236 bytes (largest file 1,413,318 bytes), a
49,191,846-byte wire document, tree digest
`53130666262fd1148803fb1d88e0571d185aa62c85f4a429097948e919a46bc9`.

## Where size matters

| Step | Process | Holds | Before |
| --- | --- | --- | --- |
| Source fetch (`sourcefetch`) | worker, verifier, release manager | the decoded tree, one file and the paths: the archive is decompressed and parsed as it arrives | compressed archive + expanded tar + tree |
| Worker source cache | worker | one decoded tree; each read is a deep copy | one wire document, decoded per read |
| Execution preparation | worker | one wire document (two with a repair: original and parent) | always two |
| `ImportSource` | worker | a private copy of the document, inspected in place | two copies + a decoded tree |
| Executor import helper (`/workspace-transfer import`) | executor mailbox, repository-tests transfer sidecar | the paths and one file; files go to a staging directory | the document + a decoded tree |
| Executor export helper | executor mailbox | the paths and one file, streamed to stdout | the tree + an encoding growing to 2-3x |
| `ExportCandidate` | worker, verifier | the received messages, joined once; inspected in place | a buffer growing by doubling + a decoded tree |
| Candidate capture | worker | the export, its copy, the source and result trees, the applied check | unchanged |
| Repository-tests runs | verifier | the source tree, shared by the three run trees; one encoding while importing | three encodings + decoded copies |
| Tree checks before running code (launcher, ready marker) | executor worker, transfer sidecar | one file at a time, hashed | the whole tree |
| Review | verifier | source, candidate tree, its encoding, one export | the same, plus a decoded export |
| Publication | release manager | source and applied tree | unchanged |
| Database | PostgreSQL | sizes and digests only; the candidate delta (at most 2 MiB) is the only stored content | unchanged |

Measured with the Go heap held live (garbage collection target 5%):

| Operation, 41.2 MB / 2,000-file tree (55 MB on the wire) | Heap in use |
| --- | ---: |
| `Decode` (full decoded tree) | 56.7 MB |
| `Inspect` (validate and tree digest) | 16.1 MB |
| `ImportFrom` (streamed import) | 20.3 MB |
| `ExportTo` (streamed export) | 12.9 MB |
| `ImportFrom`, 16,383 paths of 1 KiB and one 4 MiB file | 40.0 MB |
| `ExportSummary` (tree check) of that tree | 26.5 MB |
| `sourcefetch` fetch of `quarkus-super-heroes` | 43.3 MB |

The streamed helpers' cost depends on the largest file and the number of
paths, not the repository size. `TestStreamingReadersDoNotCopyTheTree` keeps
the streamed operations under 32 MiB for the 2,000-file tree.

## Memory and storage cost

At the bounds, C = 128 MiB of content and W = 192 MiB on the wire; for
`quarkus-super-heroes`, C = 35 MiB and W = 47 MiB.

| Component | Change | Worst case live | Reference repository |
| --- | --- | ---: | ---: |
| Worker | unchanged limit (2 GiB); soft Go limit at 85% | about 2W + 3C during capture, 1.3 GiB with the cache | about 0.35 GiB |
| Verifier | memory limit 256 MiB to 1,536 MiB (request unchanged, 64 MiB); soft Go limit at 85% | about 830 MiB in a review | about 0.25 GiB |
| Release manager | default memory limit 128 MiB to 512 MiB (`components.releaseManager.resources`); soft Go limit at 85% | about 2C plus one file | about 75 MiB |
| Executor mailbox and transfer sidecar | unchanged (256 MiB and 64 MiB); the helper sets a 48 MiB soft Go limit | about 40 MiB | under 20 MiB |
| Executor workspace volume | memory-backed 64 MiB to disk-backed 512 MiB | | |
| Executor worker ephemeral storage | 128 MiB to 640 MiB (3,072 MiB with a toolchain); the request defaults to the limit, so the scheduler reserves 768 MiB of node disk per executor pod instead of 256 MiB | | |
| Repository-tests workspace volume | 256 MiB to 1 GiB (disk) | | |
| Repository-tests container ephemeral storage | grows by the same 768 MiB (requests and limits), 2.25 GiB more node disk reserved for the three concurrent runs | | |

The executor workspace moved to disk because a memory-backed volume is charged
to the pod's memory: at the bounds it would add 128 MiB plus the agent's
changes to the executor's memory limits and the executor namespace quota.
The repository-tests workspace was already disk-backed. Kubernetes evicts a
pod whose disk use, volumes included, exceeds the sum of its container
ephemeral-storage limits, so each pod's largest container limit covers its
volumes. No namespace quota changes: the executor, review and verifier quotas
count CPU and memory, and no container in those namespaces changes memory.

## Time bounds

Time bounds are unchanged. A 96 MiB archive within the 30-second fetch needs
about 3.2 MiB/s from the forge; a 192 MiB document within a 30-second import or
export exchange needs about 6.4 MiB/s through the Kubernetes API server. The
reference repository needs a seventh of that. A slower link fails closed at the
named fetch, import or export step, with no partial checkout.

## Security properties kept

- Every bound is still checked while parsing, before allocating a decoded
  body, and again on the decoded tree; only the numbers changed.
- Paths, modes, duplicate and file/directory conflicts, links, special nodes
  and the `.git` rules are unchanged, for archives, imports and exports.
- The import helper verifies the wire SHA-256 and source commit before the
  tree becomes `repository`: files are written to `.repository-import` with
  the same no-follow, exclusive creation, and the directory is renamed only
  after the whole document, the tree checks and the digest pass. A refused
  import leaves the workspace failed, as any partial import did.
- The export helper finds every refusal reason before writing; a file that
  changes between listing and reading (device, inode, mode, links, owner,
  size and change times) fails the export with a nonzero exit, which voids
  its output.
- The archive's gzip checksum, single member, end-of-archive blocks and zero
  padding are still verified, now on the stream.
- `Encode` is byte-identical to the previous `json.Marshal` encoding, so source
  and export digests of existing records keep their meaning.

## Upgrading

Migration `0041_large_workspace_bounds.sql` only widens two size checks and is
a relaxation migration: a configuration upgrade applies it. Rendering the chart
changes the verifier's memory limit and, unless the values file sets its own,
the release manager's. The executor and repository-tests pod shapes change
only in the volumes and disk limits above; their tests check that reverting
exactly those fields reproduces the previous pods.

Not verified here: a full issue-to-pull-request run of a repository of this
size in a cluster, and the executor's memory under gVisor with the
disk-backed workspace (the sandbox may cache file data it reads through its
gofer; unlike the earlier memory-backed volume, that cache is not the only
copy). The fetch, codec, import, export and tree checks are verified by tests
and by the reference repository fetch above.
