# Reproducible local runtime validation

From an AI-SDLC checkout:

```bash
python3 tools/validate_runtime.py
```

Prerequisites are Python 3.10+, Git, and a working Docker or Podman daemon.
The current tested container profile is Linux/amd64. Other host architectures
need compatible emulation and are not claimed validated. The command selects
Podman before Docker when both are installed; choose explicitly with
`--provider docker` or `--provider podman`.

The committed `tools/runtime-validation.lock.json` fixes Go and PostgreSQL
images by digest. The runner installs Git from the image's Alpine repositories;
initial image/module/package downloads need network access. This is reproducible
validation orchestration, not a hermetic build or signed supply-chain provenance.

## What the command establishes

A unique run owns two labeled containers. PostgreSQL listens only on loopback in
its container network namespace; the Go runner joins that namespace. No host
port is published and no existing database is used. Database files live in a
512 MiB tmpfs mount; no anonymous data volume remains after removal. Readiness
requires a real SQL query over TCP, after the image's initialization server exits. The entire checkout is
mounted read-only at its original absolute path, including portable contract
fixtures. Git worktrees also mount their common Git metadata read-only, so commit
information remains available. The ephemeral runner trusts only this checkout
path through `safe.directory`; it does not disable ownership checks globally or
change host Git configuration. Shared SELinux mount
labels support concurrent validators; do not simultaneously remount the same
checkout or caches with private `:Z` labels from another tool.

After bounded database readiness, the runner performs Go build, vet and
`go test -json -count=1 -p 1 ./...`. Serial packages avoid fixture role conflicts.
The report requires actual passing persistence tests for migration, intake,
execution claims and model reservations. A skipped database test cannot satisfy
that requirement. Optional Kubernetes/provider integration tests still need
their dedicated fixtures; all skips are listed. Success does not establish
live issue processing, isolated Pod conformance, a release or E2E delivery.

## Reports and deadlines

By default, each run gets a new private directory under
`$XDG_STATE_HOME/ai-sdlc/validation` (or `~/.local/state/ai-sdlc/validation`).
`report.json` records the commit, dirty-worktree flag, image pins, stage results
and timings, tests and cleanup. Logs sit beside it. A dirty commit identifier
does not uniquely identify uncommitted bytes; validate a clean reviewed commit
for release evidence. Keep these developer reports out of source control.

```bash
python3 tools/validate_runtime.py --provider podman --timeout 3300 --output /tmp/my-new-validation
```

The output directory must not already exist. The overall work deadline defaults
to 3300 seconds and accepts 60–3600. Each Go package has a 30-minute deadline,
matching the contract job, within a 45-minute test stage; the local-validation CI
job allows 65 minutes including setup and cleanup. The execution package's
real expiry and recovery waits approach 25 minutes on hosted runners.
Cleanup has a separate bounded window. A
failed command exits nonzero, records attempted stages and removes only objects
whose ownership label matches the run, by observed container ID. Cleanup errors
also make the result fail and identify remaining names for inspection.

Temporary database credentials are generated per run and passed by named
environment variables to the container CLI. They are never written to host
files or command arguments. The fixture containers retain their own environment
until removed. SIGKILL, daemon loss or a host crash can prevent cleanup; inspect the report/run directory and exact
ownership label before removing a leftover container. Do not run broad container
prune commands as recovery. Source tests execute trusted developer code and can
read fixture credentials; this tool is not a hostile-code sandbox.

Module and build caches remain under `$XDG_CACHE_HOME/ai-sdlc/validation/go`
(or `~/.cache/ai-sdlc/validation/go`). On a failure, inspect the first failed
stage's log, fix the underlying dependency or test, then start a new run. A
retry produces separate evidence rather than overwriting the failure.

## Native provenance registry

Use `python3 tools/validate_provenance_registry.py --output /tmp/new-registry-evidence`
for the separate Linux/Podman native registry test. It verifies the committed
pull-only ACL, actual TLS/signatures and bounded Go verifier against disposable
Zot with synthetic history. See the [scope and cleanup contract](../architecture/operational-provenance-boundary.md#native-registry-access-conformance).
It does not install trust or activate the operational pipeline.

The full PostgreSQL suite has a 20-minute per-package ceiling and a 25-minute test-stage ceiling, both constrained by the overall deadline. This accommodates the serial journal fault tests on hosted runners; it does not retry or skip failed tests.
