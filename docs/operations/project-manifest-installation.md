# Installing a repository with a project manifest

A repository that is not a built-in reference (Homedir, Saferus) is adopted
through a reviewed project manifest (`ai-sdlc.project/v1`). The manifest is
installation configuration: it is never read from the target repository, an
issue or model output. See the [design](../architecture/generic-inner-loop.md)
for the rules it drives.

## 1. Write the manifest

Start from [`examples/ai-sdlc-sample.project.json`](../../examples/ai-sdlc-sample.project.json)
(Go) or [`examples/ai-sdlc-sample-python.project.json`](../../examples/ai-sdlc-sample-python.project.json)
(Python, for [ai-sdlc-sample-python](https://github.com/os-santiago/ai-sdlc-sample-python)):

| Section | What to set |
|---|---|
| `verification.image` | The toolchain image pinned by digest. Tests run offline, so dependencies must be vendored, in the image, or in a sealed dependency cache (`verification.dependencies`) |
| `verification.command` | The test command as an argument list, run from the repository root |
| `verification.result` | `exitCode: true`, plus an optional JUnit `junitPath` for per-test evidence. Without a report the decision is *weak* and the reviewer must judge it |
| `verification.testGlobs` | The files that count as tests |
| `verification.harnessGlobs` | Files a candidate may not change (build, dependency and test configuration, and the configuration of any `verification.analysis` tool, such as `.golangci.yml`, so a candidate cannot switch off its own analysis) |
| `verification.env` | Optional reviewed toolchain variables, for example `GOTOOLCHAIN=local` |
| `verification.dependencies` | Optional read-only dependency cache: `cachePath` (absolute; not under `/workspaces`, `/tmp`, `/runner`, `/etc`, `/proc`, `/sys`, `/dev`, `/run`, `/var/run`, `/var/tmp` or a system binary or library directory) and 1–32 `keyGlobs` naming the dependency-defining files (for Maven `**/pom.xml`, `.mvn/**`, `mvnw`). Key files are always harness files. Omitted when absent, so existing digests do not change. See [the dependency cache](dependency-cache.md) and [`examples/quarkus-super-heroes.project.json`](../../examples/quarkus-super-heroes.project.json) |
| `verification.analysis` | Optional static analysis: up to 4 argv `commands` and one `timeoutSeconds` (10–300, at most 600 in total), for example `{"commands": [["go", "vet", "./..."]], "timeoutSeconds": 120}`. They run offline in `verification.image`, on the baseline and on the candidate. A command that passes on the baseline and fails on the candidate refuses it (`refused-candidate-analysis`). See the [candidate baseline](../architecture/candidate-quality-baseline.md#static-analysis) |
| `publication` | The paths a candidate may change, and the file and size limits |
| `publication.commitIdentity` | Optional `name` and `email` of the author, committer and `Signed-off-by` of every published commit. Defaults to `ai-sdlc[bot] <noreply@ai-sdlc.invalid>`; omitted when absent, so existing digests do not change. Images older than this field refuse a manifest that sets it, so reinstall the manifest without it before rolling back (see [rollback](publication-writer.md#rollback)). See [commit identity](publication-writer.md#commit-identity) |
| `merge` | The required checks (context and GitHub App ID), strict checks, resolved threads, zero human approvals. `baseBranch` must be `main` |
| `generation.toolchain` | Optional. The toolchain root inside `verification.image` and the directories under it for the generator's `PATH`. See below |

The pod limits in the manifest set the verifier namespace quota for three
concurrent runs. The installer derives that quota.

Every candidate is also scanned for added credentials before verification
starts, whatever the manifest says (`refused-candidate-secret`). Test
fixtures that need a key or token-shaped value should generate it at run
time; see the [candidate baseline](../architecture/candidate-quality-baseline.md#secret-scanning).

### Optional: a toolchain for the generator

A top-level `generation.toolchain` section declares a self-contained
toolchain inside the pinned `verification.image`, so the generator can run
the repository's tests before it exports a candidate:

```json
"generation": {
  "toolchain": {"root": "/usr/local/go", "path": ["bin"]}
}
```

- `root` is an absolute, clean directory path of at most 256 bytes. It cannot
  be `/` or lie under `/workspaces`, `/tmp`, `/runner`, `/proc`, `/sys`,
  `/dev`, `/run`, `/var/run`, `/var/tmp` or `/etc`.
- `path` lists one to eight distinct, clean, relative directories under `root`
  (no `..`, no `.git`), in `PATH` order.
- The section requires `verification`: the toolchain comes from the same
  pinned image as the tests.
- A manifest without the section keeps its digest. Adding it changes the
  digest, so an existing installation must be reconfigured.

The runtime copies `root` with a bounded copier (`repository-tests
copy-toolchain`) into a volume that the generation worker mounts read-only.
The copier enforces these rules:
- It never follows symbolic links. It copies only relative links that stay
  inside the root.
- It strips setuid, setgid and sticky bits, and makes the copy read-only.
- It caps the copy at 50,000 entries, 1 GiB and a depth of 64.
- It refuses executables that are dynamically linked (an ELF file with an
  interpreter), so a toolchain built for another libc fails at copy time.
  This is a compatibility check, not a security boundary.
- The destination must not exist yet: it is a subdirectory of the volume,
  written atomically.

Only self-contained toolchains, such as Go's, qualify. The official Python
images do not: their interpreter is dynamically linked against glibc, and a
`path` directory may not contain `python3`. The Python example therefore
omits `generation.toolchain`; the generator gets the offline guidance and
the verifier still runs the repository's `unittest` suite. The offline
guidance names the manifest's `verification.command` and directory (taken
only from the reviewed manifest, never from the issue), and asks for tests in
the same framework, imports and conventions as the repository's existing
tests with no new dependency, because verification has only the image's
packages: a test that imports `pytest` fails in the Python image. Since the
generator cannot run the tests, the guidance also asks it to check every
expected value by hand and prefer small inputs with obvious results: in Python
batteries 38–40 most remaining refusals were correct code with a miscounted
expected value, most of them on one issue
([evidence](../evidence/2026-11-26-claude-cli-agent-experiment.md)). A generation toolchain (#831) or a verification repair (#837)
would catch these mechanically.

The Python example runs the suite as `python3 -I -B -m unittest discover -s . -t .`.
Isolated mode (`-I`) keeps the candidate's working tree off `sys.path` until
discovery starts, so a candidate's `unittest.py` or `sitecustomize.py` cannot
replace the test runner, and it ignores `PYTHON*` variables; `-B` keeps the
no-bytecode behavior. The example also denies root-level `*.py` files to candidates.
Isolation protects only the verifier: a repository CI that runs
`python -m unittest` without `-I` would import a root `unittest.py`, and its
required check would pass without running tests on every later pull request.
Use `python -I -m unittest` in the repository's CI too. A package's `load_tests` hook can still change which
tests run, as Go's `init` functions can in the Go example. Package files are
product code, so they cannot be listed in `harnessGlobs`; the independent
review is the control for that case.

An issue that needs a new top-level package is refused with `no-failing-test`.
The check that a new test fails on the old code runs the base tree plus the
candidate's test files only. Without the new package's `__init__.py`, discovery
skips the directory and the run passes. Issues inside existing packages are
not affected.

#### What the generator gets

With the section, each generation (and its repair) runs in an executor pod
with two init containers before the worker and the mailbox start:

1. `install-runner` (the mailbox image) installs the static copier into a
   small disk volume.
2. `copy-toolchain` (the pinned `verification.image`, the digest the verifier
   uses) copies `root` into a 1,280 MiB disk volume. The volume is larger
   than the copier's 1 GiB byte cap to leave room for the file-system blocks
   of up to 50,000 entries.

The worker then runs with:
- the toolchain mounted read-only at `/toolchain/root`, and `toolchain.path`
  (under that directory) before the worker image's directories on `PATH`;
- a 1 GiB disk scratch volume at `/scratch` for `TMPDIR` and
  `XDG_CACHE_HOME`, which holds the Go build cache;
- only the toolchain variables of `verification.env`: a strict allowlist of
  Go (`GO…`, for example `GOFLAGS` or `GOTOOLCHAIN`), cgo (`CGO_…`) and Rust
  (`CARGO_…`, `RUST…`) names. SCC builds its children's environment from its
  own, so these variables also reach SCC's Node.js process; nothing that
  configures Node.js, OpenSSL, the C library, a shell, Python, Git, npm or the
  launcher is passed. A value that names `/workspaces` or `/scratch` is
  refused at installation: the migrator names the error. The same names are
  allowlisted for `run_shell`. Each `:`-separated entry at or under `root`
  is relocated to the copy (for example `GOROOT=/usr/local/go` becomes
  `/toolchain/root`); a value that names `root` in any other way (for example
  inside a flag) is refused at installation. Other variables stay
  verification-only;
- the test command with an absolute executable under `root` (for example
  `/usr/local/go/bin/go`) relocated the same way, in the command and the
  prompt. A relative command such as `go test ./...` is unchanged, and the
  verifier always runs the manifest's own command and environment;
- 512 MiB more memory (a 1,280 MiB limit) and a 2,560 MiB ephemeral-storage
  limit, which covers the three disk volumes (1,280 + 1,024 + 128 MiB) and
  the worker's own 128 MiB, so the kubelet evicts the pod predictably if they
  fill. Kubernetes sets each request to its limit, so the scheduler reserves
  per pod 2,560 MiB (worker) + 128 MiB (mailbox) of ephemeral storage, and
  128 MiB for each init container while it runs. The executor quota does not
  bound ephemeral storage;
- a prompt that names the manifest's test command and its directory, and
  tells the generator to run it with `run_shell` and fix its change until it
  passes before it finishes.

The worker stops before running SCC if a declared `path` directory contains
`node`, `git`, `scc`, `sh`, `bash`, `env`, `bwrap`, `busybox`, `sed` or
`python3`: those would replace the worker's own programs on `PATH`.

The chart sizes the executor namespace for this pod from the same
`project.manifestJSON` the runtime mounts: `executor-bounds` grows
`limits.memory` from 4 GiB to 6 GiB (four pods with 512 MiB more each)
whenever the manifest declares `generation.toolchain`, so an upgrade can never
run a toolchain pod under the smaller quota. Requests do not change. A
RuntimeClass `podOverhead` is added on top, as before. The readiness budget,
the lifecycle claim, the pod deadline and the signed command lifetime grow
by 180 s for the image pull and the copy (see
[the timing envelope](candidate-runtime.md#timing-envelope)).

#### Stops

A toolchain the copier refuses fails the pod at init, before any model call.
The worker log names the stop:

| Stop | Cause |
| --- | --- |
| `readiness-toolchain-dynamic` | An executable is dynamically linked (for example a glibc toolchain) |
| `readiness-toolchain-link` | A symbolic link may leave the root |
| `readiness-toolchain-limit` | More than 50,000 entries, 1 GiB or depth 64 |
| `readiness-toolchain-source` | The root is missing, unreadable, behind a symbolic link, or contains a device, FIFO or socket |
| `readiness-toolchain-usage` | The root or destination is refused |
| `readiness-toolchain-destination` | The volume could not be written |
| `readiness-toolchain-memory` | The copy ran out of memory |
| `readiness-toolchain-install` | The copier could not be installed |
| `readiness-toolchain-failed` | Any other copy failure |

The runtime does not fall back to the offline guidance: fix the manifest and
reinstall.

The runtime accepts only the canonical manifest bytes the installer writes.
An installed manifest in any other form, such as keys in another letter case,
which Go's decoder would accept, stops the runtime with "project manifest is not in
canonical form". The chart likewise refuses to render a `project.manifestJSON`
that is not a JSON manifest object.

#### Rotation

Adding, changing or removing `generation.toolchain` changes the manifest
digest, which the installation policy binds. The installer refuses a
different manifest for an installed policy, so changing the toolchain of an
installed repository requires a reinstall. A new `verification.image` digest
also changes the generator's toolchain.

## 2. Install

```bash
./install.sh local --cluster ai-sdlc-sample \
  --repository OWNER/REPO --environment ai-sdlc-production \
  --profile verification --independent-review --publication --merge \
  --github-auth gh-cli --codex-auth-file PATH \
  --project-manifest path/to/manifest.json
```

In an installation config file, the same manifest is the `projectManifest`
key, resolved relative to the config file.

The installer validates the manifest's repository (lowercase, not a
reference) and merge section before any remote effect, then:
1. compiles the manifest with the pinned migrator;
2. binds its digest into the authority policy (`projectSHA256`);
3. mounts it read-only in every component;
4. creates the additive native merge rule from the `merge` section.

**First run:** the merge rule's ID is known only after it is created. If the
manifest's `merge.rulesetID` differs, the installer stops and prints the ID.
Set `merge.rulesetID` to that value and run the same command again.

An installation keeps the manifest it was installed with. Supplying a different
manifest is refused, and so is omitting it; rotation is a reviewed
reconfiguration.

## 3. Deliver

File an issue in the repository. No marker is needed: the installed manifest
selects verification with the repository's own tests (`repository-tests/v1`).
