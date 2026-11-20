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
| `verification` | Pinned toolchain image (`repo@sha256:`), argv test command, working directory, timeout, resources, `network: none`, exit code and optional JUnit report, repeat count, test globs, harness globs |
| `publication` | Allow and deny path globs, file and byte limits |
| `merge` | Ruleset, required checks, strict checks, resolved threads, human approvals |
| `recipes` | Optional reviewed verification plug-ins (the per-issue oracles of the references) |

**Parsing:**
- strict: unknown fields and duplicate keys are refused, object keys must be
  ASCII, and the merge booleans must be explicit;
- capped at 16 KiB;
- one canonical form, whose sha256 is the manifest digest.

**Globs** match clean, relative paths in a conservative character set. Deny
wins. A path no allow pattern matches is denied. `.github/**`, `.gitmodules`,
`.gitattributes` and `CODEOWNERS` are always denied. Matching is polynomial.

**Binding.** The authority policy carries `projectSHA256` (omitted when
empty, so existing revisions are unchanged). Every component resolves the
manifest from the same authenticated policy on each load, and records it only
when the whole load succeeds:
- a bound digest selects only an installed manifest with that digest and
  repository;
- reference repositories always use their built-in manifests;
- a mounted manifest that the policy does not bind is refused.

## Verification with the repository's own tests (`repository-tests/v1`)

The sandboxed verifier runs the manifest's command on three trees:

| Run | Tree | Purpose |
|---|---|---|
| baseline | base revision | which tests passed before |
| cross | base revision plus only the candidate's test files | the new tests must fail on the old code |
| candidate | candidate revision, repeated | the change passes, consistently |

The runner sees a fixed, empty environment and discards untrusted output. It
reads only a regular report file inside the workspace and emits one bounded
result: an exit code and per-test status keyed by the sha256 of the test
identity.

**Decision rules.** Each refusal has one reason:

| Reason | Meaning |
|---|---|
| `incomplete-evidence` | The number of runs is wrong |
| `invalid-delta` | A changed path is invalid or contradictory |
| `harness-changed` | Build, dependency, test configuration, helper or data files were changed |
| `test-deleted` | A test file was deleted or renamed |
| `no-test-change` | No test file was changed |
| `uncertain` | Candidate runs disagree |
| `candidate-failed` | The candidate fails |
| `no-cross-report` | With a report, the cross run produced none |
| `regression` | A test that passed on the baseline no longer passes |
| `no-failing-test` | No test fails explicitly on the base code and passes on the candidate |
| `baseline-failing` | Without a report, the baseline fails |

A decision without a per-test report is marked **weak**, so the independent
reviewer judges whether the tests exercise the requested behaviour. The
summary (exit codes, counts, sorted changed test files) is bound into the
verification proof and given to the reviewer.

**Known limits, for the reviewer.** A test file can weaken assertions or define
its own entry point (for example `TestMain`). Rules cannot detect this; the
read-only reviewer receives the changed test files for that judgement.

## Sandbox

Each run is an offline pod in the `ai-sdlc-offline` RuntimeClass (gVisor,
`network=none`), with:
- no service account token and no secrets;
- a non-root user and a read-only root filesystem;
- memory-backed scratch directories with size limits;
- fixed resources and a hard deadline.

The source tree is imported into the pod by the verified workspace-bundle
protocol. The toolchain image is chosen by the installation manifest, never by
the repository.

## Slices

1. Manifest type, parsing, globs, built-ins (#672)
2. Policy binding, migrator compile step, chart value (#674)
3. Manifest-driven registry, publication and merge policy (#675)
4. Decision rules (#676)
5. Runner and JUnit parser (#677)
6. Sandboxed three-tree workload and controller wiring, proof v2, reviewer input
7. Installer support and a local kind run on a sample repository
