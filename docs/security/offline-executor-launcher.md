# Offline SCC executor launcher

Issue #467 adds a hidden execution prerequisite. It does not enable live issue
execution, issue-to-production delivery, provider calls or new Kubernetes roles.
The ordinary worker Deployment still starts `/component`. A future reviewed
controller must create, authorize and observe the separate executor workload.

## Fixed inputs and workload

`internal/executor.Compile` is a pure manifest compiler. It accepts a namespace,
two immutable image references, an exact assignment, its reviewed profile and a
pinned A-Dev profile. It returns an immutable ConfigMap and a fixed Pod. It has no
Kubernetes client, provider client, credentials or admission authority.

The assignment binds the work item, attempt, repository, source commit, canonical
source-tree digest, profile digest, authority revision, issue data, acceptance
criteria and expiry. Parsing an assignment does not authorize execution. The
future controller must bind those inputs and the actual Pod UID in durable state
before starting any live work. A ConfigMap hash is an integrity binding, not a
signature or proof of origin.

The Pod uses `ai-sdlc-offline`, non-root UID/GID 1000, dropped capabilities,
read-only container roots, no service-account token, no host namespaces and no
shared process namespace. Worker and mailbox images must use `@sha256:` refs.
Only fixed, bounded memory-backed workspace and bridge volumes are writable.
The worker cannot mount the bridge files. Assignment and Pod-identity projections
are read-only. No arbitrary commands, mounts, environment or endpoints are
accepted from an issue or repository. The Pod deadline is 1,020 seconds and its
restart policy is `Never`; a replacement Pod requires a separately authorized
attempt, not an automatic replay.

## Import, start, execute, terminate, export

1. A trusted controller imports a bounded regular-file source bundle through
   `/workspace-transfer`, checking the actual Pod UID and exact wire digest.
2. After import succeeds, `/executor-control start` exclusively publishes a
   marker binding Pod UID, assignment digest and source-tree digest. It uses
   no-follow directory-relative access and non-replacing atomic publication.
   Repeated publication fails. The marker is synchronization, not authorization.
3. The PID-1 `/executor` waits at most 60 seconds. It verifies the bound assignment,
   profile, A-Dev policy and deterministic SCC configuration. It rechecks expiry
   after the marker, verifies the tree in its **own filesystem view**, and rejects
   preexisting root `.git` or a previous outside-worktree Git baseline.
4. It initializes a fresh Git baseline with an empty image-owned template and
   hooks/ambient Git configuration disabled. `git add --force --all` includes
   source files that `.gitignore` would otherwise hide. Git metadata lives at
   `/workspaces/git`, outside the regular-file candidate export. This local
   synthetic commit is not the original source commit or release provenance.
5. SCC receives fixed arguments, a complete explicit environment, the reviewed
   prompt/profile and a Pod-loopback model endpoint. Its wall-clock execution is
   bounded by both assignment expiry and profile duration. Combined subprocess
   stdout/stderr is capped at 256 KiB; exceeding it fails the attempt. Subprocess
   groups are killed on cancellation and after the direct child exits.
6. The launcher exits immediately after SCC. **An external observer must verify
   that the worker container with the assigned Pod UID terminated before export.**
   Child exit, summary files and start markers cannot prove this. A detached child
   may have its own process group; container termination is the final boundary.
7. The helper exports regular-file candidates only after that external check.
   The controller must independently compare them with trusted source and apply
   review, test, security, performance and release gates. Exit zero alone never
   means the candidate is accepted or ready for release.

The canonical tree digest includes the source commit and every sorted UTF-8 path,
Git executable mode, byte length and SHA-256 content digest. Its versioned binary
encoding is implemented in `workspacebundle.Bundle.TreeDigest`; an independently
computed fixed vector checks interoperability. It does not rely on JSON ordering,
Git ignore rules, a worker-provided manifest or a worker-created commit.

## Repository configuration and model bounds

The pinned SCC supports project configuration. That configuration remains
untrusted. `SC_MODEL`, `SC_BASE_URL` and a read-only global profile select the
intended model and endpoint, but project configuration can still alter options
such as `maxTokens` and `stream`, including through `activeProfile`.

The compiler therefore always starts a **restricted mailbox** with the reviewed
model and per-request token cap. Before publishing a request, it rejects a model
mismatch, streaming, missing/noninteger/nonpositive/over-cap `max_tokens`,
alternative top-level fields and duplicate JSON keys. It preserves accepted
request bytes. Invalid project settings cause a bounded failure; they are not
silently rewritten into a successful attempt.

This is a local request constraint, not a token accounting system, financial
budget or durable quota. The external provider broker must independently bind
and enforce the reviewed profile, reserve durable budgets and reject uncertain
replays. The hidden model-call ledger and this launcher are not yet wired into
that broker. A model's output and tool actions remain untrusted regardless of
prompt delimiters or claimed compliance.

## Validation and current limitations

`tools/verify_executor.py` compiles the real template, loads a pinned Homedir
snapshot into a disposable gVisor Pod, runs the actual pinned SCC, supplies
synthetic model replies and compares the exported candidate externally after
worker termination. The normal case uses SCC's real `write_file` tool and
preserves all original Homedir source files. Negative cases exercise direct and
`activeProfile` token overrides and streaming overrides. The persistent-child
case spawns a detached shell through SCC's real tool, proves that it started,
then checks that its delayed write cannot survive worker container termination.
Additional negative cases tamper with the imported tree or the projected profile
and A-Dev policy after digest binding; the actual launcher rejects them with no
published model requests. A timeout case withholds the reply to a real SCC
request and requires launcher exit 124 within a separate external deadline before
export. Fixed publication logs let the fixture compare all accepted request
sequences with its external observations without logging request contents.

Go tests cover assignment/profile ambiguity, digest mismatches, expiry, source
mutation, existing Git metadata, ignored-file baseline inclusion, exclusive
start binding, subprocess output/deadline bounds and fixed workload isolation.
The container fixture is quality CI; it does not orchestrate live AI-SDLC work.
Fixture issue numbers and candidate files are synthetic and are never published
as product improvements or releases.

The worker image has Node and Git, not every target project's toolchain or
network-fetched dependencies.

## Optional generation toolchain (issue #745)

A repository whose reviewed manifest declares `generation.toolchain` gets its
own self-contained toolchain in generation. The assignment then carries the
pinned verification image, the toolchain root and `PATH` directories, the
verification command and the filtered verification environment. All of it
comes from the installed manifest, whose digest the authority policy binds,
never from the issue, the repository or the model. The assignment and its
workload are byte-identical to the original when the section is absent.

The trust boundary does not change:
- Both init containers run with the worker's security context: UID 1000,
  dropped capabilities, read-only root, no privilege escalation, no
  service-account token, inside the same sandbox RuntimeClass and with no
  network. The toolchain image only supplies files; the copy is bounded,
  never follows symbolic links and strips setuid, setgid and sticky bits.
- The worker mounts the copy read-only. The only new writable volume is the
  disk scratch volume for temporary files and build caches; it is not part of
  the exported candidate, which is still the regular files of the workspace.
- `run_shell` children get the toolchain directories first on `PATH` and the
  allowlisted toolchain variables. A variable can never replace SCC, Node.js,
  Git, npm or launcher settings. A toolchain that ships `node`, `git`, `scc`,
  `sh` or `env` in a `PATH` directory stops the run before SCC starts.
- Running the tests in generation is guidance for the model, not evidence:
  independent offline verification still runs the same command on a fresh
  copy and decides.

The trusted controller accepts exactly the compiled init sequence. Readiness
and every later observation require the compiled init containers, in order,
running their pinned images, never restarted, exited 0 and with unchanged
container IDs. Any other init status refuses the pod. A failed copy is named
with a bounded `readiness-toolchain-*` stop. This fixture does not prove Java/Maven validation,
provider-backed generation, live admission, durable job supervision, candidate
review, Konflux/SLSA promotion or deployment. Those require separate reviewed
components and real evidence before autonomous delivery is enabled.
