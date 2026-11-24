# Installing a repository with a project manifest

A repository that is not a built-in reference (Homedir, Saferus) is adopted
through a reviewed project manifest (`ai-sdlc.project/v1`). The manifest is
installation configuration: it is never read from the target repository, an
issue or model output. See the [design](../architecture/generic-inner-loop.md)
for the rules it drives.

## 1. Write the manifest

Start from [`examples/ai-sdlc-sample.project.json`](../../examples/ai-sdlc-sample.project.json):

| Section | What to set |
|---|---|
| `verification.image` | The toolchain image pinned by digest. Tests run offline, so dependencies must be vendored or in the image |
| `verification.command` | The test command as an argument list, run from the repository root |
| `verification.result` | `exitCode: true`, plus an optional JUnit `junitPath` for per-test evidence. Without a report the decision is *weak* and the reviewer must judge it |
| `verification.testGlobs` | The files that count as tests |
| `verification.harnessGlobs` | Files a candidate may not change (build, dependency and test configuration) |
| `verification.env` | Optional reviewed toolchain variables, for example `GOTOOLCHAIN=local` |
| `publication` | The paths a candidate may change, and the file and size limits |
| `merge` | The required checks (context and GitHub App ID), strict checks, resolved threads, zero human approvals. `baseBranch` must be `main` |
| `generation.toolchain` | Optional. The toolchain root inside `verification.image` and the directories under it for the generator's `PATH`. See below |

The pod limits in the manifest set the verifier namespace quota for three
concurrent runs. The installer derives that quota.

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

Only self-contained toolchains, such as Go's, qualify.

> **Status:** the manifest section and the copier exist; the executor pod does
> not use them yet. See issue #745.

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
