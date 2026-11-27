# Generic inner loop (M1 design)

Status: in progress. This design lets a repository that is not a built-in
reference go from an authorized issue to a merged, verified and independently
reviewed pull request **without code changes to ai-sdlc**. It is the core of
the v1 [roadmap](../roadmap.md).

## Project manifest

A per-repository declaration, `ai-sdlc.project/v1` (`runtime/k8s/internal/project`).
It is installation configuration, reviewed like the authority policy. It is
never read from the target repository, an issue or model output.

| Section | Content |
|---|---|
| `repository`, `baseBranch` | Lowercase GitHub repository and target branch |
| `verification` | Pinned toolchain image (`repo@sha256:`), argv test command, working directory, timeout, resources, `network: none`, exit code and optional JUnit report, repeat count, test globs, harness globs, reviewed toolchain environment, optional static analysis commands (`analysis`, omitted when absent), optional read-only dependency cache (`dependencies`, omitted when absent) |
| `publication` | Allow and deny path globs, file and byte limits, and the optional `commitIdentity` of published commits (default `ai-sdlc[bot] <noreply@ai-sdlc.invalid>`; omitted when absent, so existing digests do not change) |
| `merge` | Ruleset, required checks, strict checks, resolved threads, human approvals |
| `recipes` | Optional reviewed verification plug-ins (the per-issue oracles of the references) |
| `generation` | Optional. `toolchain.root` names a self-contained toolchain inside the verification image and `toolchain.path` the directories under it for the generation worker's `PATH`. Requires `verification`; omitted when absent, so existing digests do not change. The executor copies it in init containers and the generator runs the verification command before export |

**Parsing:**
- strict: unknown fields and duplicate keys are refused, object keys must be
  ASCII, and the merge booleans and `humanApprovals` must be explicit;
- capped at 16 KiB;
- one canonical form, whose sha256 is the manifest digest.

**Globs** match clean, relative paths in a conservative character set. Deny
wins. A path no allow pattern matches is denied. `.github/**`, `.gitmodules`,
`.gitattributes` and `CODEOWNERS` (at any depth) are always denied. Matching is polynomial.

**Binding** (slices 2 and 3, in review in #674 and #675; not yet on `main`).
The authority policy carries `projectSHA256` (omitted when
empty, so existing revisions are unchanged). Every component resolves the
manifest from the same authenticated policy on each load, and records it only
when the whole load succeeds:
- a bound digest selects only an installed manifest with that digest and
  repository;
- reference repositories always use their built-in manifests;
- a mounted manifest that the policy does not bind is refused.

## Verification with the repository's own tests (`repository-tests/v1`)

Status: decision rules and runner in review (slices 4 and 5); the sandbox
and controller follow (slice 6).

The sandboxed verifier runs the manifest's command on three trees:

| Run | Tree | Purpose |
|---|---|---|
| baseline | base revision | which tests passed before |
| cross | base revision plus only the candidate's test files | the new tests must fail on the old code |
| candidate | candidate revision, repeated | the change passes, consistently |

The runner gives the command only fixed variables (PATH, HOME, TMPDIR,
LC_ALL, TZ, CI) plus the manifest's reviewed `env`, and discards untrusted
output. It
reads only a regular report file inside the workspace and emits one bounded
result: an exit code and per-test status keyed by the sha256 of the test
identity.

**Decision rules.** Each refusal has one reason, checked in this order:

| Reason | Meaning |
|---|---|
| `incomplete-evidence` | The number of candidate runs differs from `repeat` |
| `invalid-delta` | A changed path is invalid or contradictory |
| `harness-changed` | A path matching the manifest's `harnessGlobs` (build, dependency, test configuration, helper or data files) was changed |
| `test-deleted` | A test file was deleted or renamed |
| `no-test-change` | No test file was changed |
| `uncertain` | Candidate runs disagree |
| `candidate-failed` | The candidate fails |
| `no-cross-report` | With a report, the cross or candidate run produced none |
| `regression` | With a report, a test that passed on the baseline no longer passes |
| `baseline-failing` | Without a report, the baseline fails |
| `no-failing-test` | No test fails explicitly on the base code and passes on the candidate |
| `candidate-analysis` | With the optional `verification.analysis`, an analysis command that passes on the baseline fails on the candidate |

Before any run, the verifier also refuses a candidate whose added lines hold
a high-confidence credential (`refused-candidate-secret`). That check and the
optional static analysis form the
[candidate quality and security baseline](candidate-quality-baseline.md).

A decision without a per-test report is marked **weak**, so the independent
reviewer judges whether the tests exercise the requested behaviour. The
summary (exit codes, counts, at most 64 sorted changed test files) has a
digest; binding it into the verification proof and giving it to the reviewer
is the last part of slice 6.

**Known limits, for the reviewer.** A test file can weaken assertions or define
its own entry point (for example `TestMain`). Rules cannot detect this; the
read-only reviewer receives the changed test files for that judgement.

## Sandbox

Status: in review (slice 6).

Each run (baseline, cross, candidate 1..n) is its own offline pod in the
`ai-sdlc-verifiers` namespace (single deny-all network policy, checked before
every effect) and the `ai-sdlc-offline` RuntimeClass (gVisor, `network=none`):

| Container | Image | Role |
|---|---|---|
| `install-runner` (init) | verifier | copies the static runner into a volume |
| `transfer` (native sidecar) | model mailbox | idles in `workspace-transfer hold`; the controller's exec target |
| `tests` | toolchain pinned by the installation manifest | runs the runner, which runs the repository's command |

- No service account token, no secrets, no service links, `restartPolicy: Never`.
- Non-root user, read-only root filesystem, all capabilities dropped,
  RuntimeDefault seccomp; `/tmp` is the size-limited scratch (HOME, TMPDIR).
- The tests container gets requests equal to the manifest's limits; a hard
  deadline covers the pull budget, ready wait, test timeout and margin.
- The repository command reaches the runner only through an immutable
  per-run settings ConfigMap, never the pod spec. The manifest may add a
  reviewed toolchain environment (`verification.env`, for example `PATH`).

**Sequence.** The controller:
1. imports the exact tree through the sidecar (verified workspace-bundle
   protocol), checking the receipt;
2. publishes a ready marker bound to the pod UID, source and tree.

The runner waits for that exact marker, verifies the tree digest again, turns
non-dumpable (same-uid repository code cannot trace it or open its output),
runs the command, kills any leftover process (it is the pid namespace's init),
reads the report and prints one result line. No helper runs once the
repository's code may run. The controller accepts the result only from a
succeeded pod, from the same container instance, as exactly one line.

## Limits for adopters

- `repeat` is 1–3 and the test timeout 30–600 seconds.
- The imported tree is bounded by the workspace-bundle limits (16 MiB of
  content, 4096 files) and has no `.git`: builds that need git metadata fail.
  Dependencies must be vendored, in the toolchain image, or in a sealed
  dependency cache that every run mounts read-only (no network; see
  [the dependency cache](../operations/dependency-cache.md)).
- The workspace volume holds the tree and any build output (256 MiB).
- At most 512 test cases, a 4 MiB report and 16 levels of suite nesting.
- The verifier namespace's resource quota bounds what a run can request; the
  installer must size it for the manifest (slice 7).
- The pod relies on Kubernetes native sidecars and the `ai-sdlc-offline`
  RuntimeClass, which the installer provides (slice 7).
- A failed pod or an unreadable result is a transport failure, not a
  decision; nothing is retried automatically.

## Slices

1. Manifest type, parsing, globs, built-ins (#672)
2. Policy binding, migrator compile step, chart value (#674)
3. Manifest-driven registry, publication and merge policy (#675)
4. Decision rules (#676)
5. Runner and JUnit parser (#677)
6. Sandboxed three-tree workload:
   - pod-side marker, hold and runner install (#679)
   - pod compiler (#680)
   - trees and pod transport (#681)
   - controller wiring, proof v2, reviewer input
7. Installer support and a local kind run on a sample repository
