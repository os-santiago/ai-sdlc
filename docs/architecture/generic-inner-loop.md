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
| `verification` | Pinned toolchain image (`repo@sha256:`), argv test command, working directory, timeout, resources, `network: none`, exit code and optional JUnit report, repeat count, test globs, harness globs, reviewed toolchain environment, optional static analysis commands (`analysis`, omitted when absent) |
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

Each run (baseline, cross, candidate) is its own offline pod in the
`ai-sdlc-verifiers` namespace (single deny-all network policy, checked before
every effect) and the `ai-sdlc-offline` RuntimeClass (gVisor, `network=none`):

| Container | Image | Role |
|---|---|---|
| `install-runner` (init) | verifier | copies the static runner into a volume |
| `transfer` (native sidecar) | model mailbox | idles in `workspace-transfer hold`; the controller's exec target |
| `service-<name>` (native sidecar, optional) | pinned by the installation manifest | a backing service from `verification.services` (database, broker), reached on loopback |
| `tests` | toolchain pinned by the installation manifest | runs the runner, which runs the repository's command |

The candidate pod runs the command `repeat` times, each time on a fresh copy
of the verified tree.

- No service account token, no secrets, no service links, `restartPolicy: Never`.
- Non-root user, read-only root filesystem, all capabilities dropped,
  RuntimeDefault seccomp; `/tmp` is the size-limited scratch (HOME, TMPDIR).
- The tests container gets requests equal to the manifest's limits; a hard
  deadline covers the pull budget, ready wait, test timeout and margin.
- The repository command reaches the runner only through an immutable
  per-run settings ConfigMap, never the pod spec. The manifest may add a
  reviewed toolchain environment (`verification.env`, for example `PATH`).

**Backing services.** `verification.services` (at most 6) replaces Quarkus
Dev Services or Testcontainers, which need a container engine. Each service
is a native sidecar (an init container with `restartPolicy: Always`), started
after `transfer` and before `tests`. The kubelet stops it when the tests
container exits, so the pod still completes with the tests. Decisions:

- *Native sidecars, runner-side readiness.* The ordering and the clean stop
  come from native sidecars, which the pod already uses for `transfer`. The
  readiness check is not a kubelet probe. A kubelet TCP probe targets the
  pod IP, which the sandbox does not serve under `network=none`, and a
  probe would also add API defaults to the exact template comparison. Instead, the runner connects to
  `127.0.0.1:<port>` after verifying the tree and before any repository
  code runs, so the repository cannot open a port first.
- *Fresh state per pod.* Each run is a new pod with empty, size-limited
  `emptyDir` volumes, so baseline, cross and candidate never share service
  state. The candidate's repetitions share one set of services. Repetition
  1 always runs on fresh services, so the cross run and the first candidate
  run are unaffected. Later repetitions see what earlier ones left behind.
  Leftover state can make them disagree (`uncertain`, a possible false
  refusal). It can also make them agree when fresh services would not: a
  test may store a favourable result in repetition 1 and skip the flaky
  operation later. So shared services weaken the flakiness check of
  `repeat`. Fresh services per repetition would need one pod per
  repetition, a change to the effect ledger.
- *Fail closed.* A service that is not ready in time, exits or restarts
  fails the run as a transport failure, never as a decision. A service may
  be seen terminated only as a kubelet stop after the tests ended: it
  finished no earlier than the tests container, was not OOM-killed, and
  exited 0, 137 or 143. Otherwise its death could have made the tests fail,
  which the cross run would read as new failing tests on the old code
  (`observe-service`). The readiness port must open only once the service
  can serve.
- *Isolation.* Services inherit the pod's gVisor RuntimeClass, the missing
  token and service links, and RuntimeDefault seccomp. They run non-root
  (the namespace enforces Pod Security `restricted`) with every capability
  dropped, no privilege escalation and, unless the manifest opts out, a
  read-only root filesystem. They mount only their own volume. Images,
  arguments and environment come only from the installed manifest. `$` is
  refused so that Kubernetes expansion cannot change a reviewed value.
- *Budgets.* The pod deadline adds the longest `readySeconds` (at most
  180). Service requests equal their limits and are added to the verifier
  namespace quota for each of the three concurrent runs. The test timeout
  stays at 30–600 seconds. A manifest with services is refused
  (`ErrServiceBudget`) when the candidate run's deadline does not fit the
  signed command's 30-minute lifetime (`verification.MaxCommandAge`). That
  deadline is 480 s of pulls, import wait and margin, plus the longest
  `readySeconds`, `repeat` × `timeoutSeconds` and the analysis. Otherwise
  a run that could still finish would end as a failure. Manifests without
  services are not bounded this way, so their validity is unchanged.

- *Reusable definition.* `project.Service` holds no Kubernetes types.
  Only the repository-tests compiler turns it into a pod sidecar, so a
  Tekton task renderer can map the same manifest entry to Tekton `sidecars`.

**Sequence.** The controller:
1. imports the exact tree through the sidecar (verified workspace-bundle
   protocol), checking the receipt;
2. publishes a ready marker bound to the pod UID, source and tree.

The runner waits for that exact marker, verifies the tree digest again, waits
for the manifest's services (if any) on loopback, turns
non-dumpable (same-uid repository code cannot trace it or open its output),
runs the command, kills any leftover process (it is the pid namespace's init),
reads the report and prints one result line. No helper runs once the
repository's code may run. The controller accepts the result only from a
succeeded pod, from the same container instance, as exactly one line.

## Limits for adopters

- `repeat` is 1–3 and the test timeout 30–600 seconds.
- At most 6 backing services. Each one is non-root, uses a port of at least
  1024 and is ready within 180 seconds. A candidate's repetitions share them.
- The imported tree is bounded by the workspace-bundle limits (16 MiB of
  content, 4096 files) and has no `.git`: builds that need git metadata fail.
  Dependencies must be vendored or in the toolchain image (no network).
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
