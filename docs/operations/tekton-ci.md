# In-cluster Tekton CI and task catalog

The local kind installation can install Tekton Pipelines and a reviewed
catalog of deterministic CI tasks: check out one immutable commit, build and
test a Maven module offline with test services as sidecars, fill the shared
Maven cache from trusted inputs, and update an npm lockfile with scripts
disabled. The same tasks back pull-request CI, which can become a required
check, and the results ai-sdlc requests before or during generation
([architecture](../architecture/tekton-task-integration.md)).

What it does **not** establish yet: a GitHub check for a pull request, any
change to the merge gate, runtime wiring of the adapter, or delivery for a
repository that uses this CI. See [gate integration](#gate-integration-stage-3-design)
and the architecture stages.

## Install

Use the same entry point and cluster as the [local installation](../installation.md):

```bash
./install.sh plan  --tekton-ci            # offline: validates the desired state
./install.sh local --provider podman --tekton-ci
./install.sh status --tekton-ci           # fresh readiness; nonzero unless ready
```

`--tekton-ci` combines with any profile and is accepted only by `plan`, `local`
and `status` on the kind platform. The Konflux platform already runs Tekton
through its operator, and OpenShift provides OpenShift Pipelines; neither is
modified. Tekton is installed after the runtime is ready, so a Tekton failure
leaves the runtime intact and the owned Tekton state in place to reapply. The
installation report gains a `tektonCI` section only when the option is given.

### Desired state

Everything lives in `runtime/k8s/deploy/tekton-ci/`:

- `upstream.lock.json`: the Tekton Pipelines release (v1.15.5, the current
  long-term-support line), the release manifest URL, its SHA-256, size and a
  size bound, and the eight controller, webhook and helper images, each pinned
  by digest. The required namespaces, CRDs and Deployments form the inventory.
- `configuration.json`: reviewed overrides of upstream ConfigMaps. Only keys
  that already exist upstream can be set.
- `ci/`: the CI namespace, ServiceAccount, Maven cache claim, NetworkPolicies,
  the four catalog Tasks and three Pipelines.
- `projects/quarkus-super-heroes.json`: the module and npm-directory catalog
  the operator run tool uses. ai-sdlc components read the same information
  from the project manifest's `tekton` section instead.

### What the installer does

1. Downloads the release manifest once (bounded by `maxBytes`, two-minute
   timeout) into `${XDG_CACHE_HOME:-~/.cache}/ai-sdlc/tekton/`, and verifies
   size, SHA-256, that every Tekton image reference equals the locked set, and
   that every container image is pinned by digest. A cached copy is reused
   only when it verifies.
2. Refuses to adopt any existing namespace or cluster-scoped object it would
   apply (the Tekton and `ai-sdlc-ci` namespaces, CRDs, ClusterRoles and their
   bindings, webhook configurations, the CI admission policy) that does not
   carry this installation's `ai-sdlc.io/cluster-uid` annotation. A Tekton
   installed by anything else is never modified.
3. Converts the manifest with kubectl's own parser (the installer is
   standard-library Python), checks that every object stays in the locked
   namespaces, that Deployment images equal the lock, applies the reviewed
   configuration, marks every object with the cluster UID and the
   `ai-sdlc.io/tekton-ci: owned` label, and applies it with server-side apply
   under the `ai-sdlc-tekton-ci` field manager. Conflicts are never forced.
4. Waits for the CRDs to be established and the four Deployments to roll out,
   bounded by `--timeout`.
5. Creates the CI namespace, then server-side dry-runs the CI definitions until
   Tekton's webhook admits them (at most 30 attempts, 4 seconds apart; the
   webhook can serve before its certificate is trusted), then applies them.
6. Observes readiness: every required Deployment at its current generation
   with all replicas updated, ready and available; the hardened feature flags
   in place; the Pipeline present.

Re-running `local --tekton-ci` reconciles the same desired state. Omitting the
option later uninstalls nothing: deleting Tekton's CRDs deletes every run and
its history, so removal is a deliberate operator action
(`kubectl delete namespace ai-sdlc-ci` and the upstream release manifest).

### Tekton hardening

| Setting | Value | Why |
| --- | --- | --- |
| `set-security-context`, `set-security-context-read-only-root-filesystem` | `true` | Tekton's injected init and helper containers satisfy restricted Pod Security and run read-only. |
| `disable-inline-spec` | `pipeline,pipelinerun,taskrun` | Runs can reference only installed, reviewed Tasks and Pipelines. |
| remote resolvers (`git`, `http`, `hub`, `bundles`, `cluster`) | disabled | No run can fetch an unreviewed definition. |
| `enable-api-fields` | `stable` | No alpha or beta behavior. |
| `disable-creds-init` | `true` | No credential initialization; the CI carries no credentials. |
| `coschedule` | `disabled` | No affinity-assistant pods: they would hold CI pod quota (see the node limits) and deadlock concurrent runs. A task can still bind both the source and cache claims. Without an assistant, Tekton does not keep a run's pods on one node, so on a multi-node cluster the source and cache volumes must be reachable from any node (ReadWriteMany storage); node-local storage such as local-path works only on a single-node cluster like the local kind installation. |
| `send-cloudevents-for-runs` | `false` | No event sink is configured. |

## The catalog

Four Tasks and three Pipelines in namespace `ai-sdlc-ci`. Each pipeline is
`source-checkout` followed by one task and publishes `commit` and `summary`.

| Pipeline | Task | Network | Candidate code |
| --- | --- | --- | --- |
| `ai-sdlc-maven-build-test` | `maven-build-test` | none | yes (offline) |
| `ai-sdlc-maven-dependencies` | `maven-dependencies` | DNS + public HTTPS | trusted base-branch head only |
| `ai-sdlc-npm-lock-update` | `npm-lock-update` | DNS + public HTTPS | no (`--ignore-scripts`) |

**`source-checkout`** accepts `repository` (`owner/name`) and `revision`. The
revision must be a full lowercase 40-character SHA; branch names and short
SHAs are refused before any network access. It fetches exactly that commit
with depth 1, checks it out detached, verifies `HEAD` and publishes `commit`.
A pull-request head is fetchable by SHA even from a fork. Public repositories
only: no credential is mounted.

**`maven-build-test`** runs `mvn -B -ntp -o verify` in `module` with the pinned
Maven 3.9 / Temurin JDK 25 image. Its pods have no egress label, so the
namespace policy denies all network access. Dependencies come only from the
shared cache, mounted read-only and used as a Maven local-repository *tail*;
anything else Maven writes goes to a pod-local head that disappears with the
pod. A separate step publishes the bounded `summary`:

```json
{"module":"rest-villains","mavenExitCode":0,"reports":6,"tests":94,"failures":0,"errors":0,"skipped":0,
 "failingTests":[],"compilerErrors":[],"missingDependencies":false}
```

`failingTests` lists up to 10 `Class.method` names from Surefire/Failsafe
reports, `compilerErrors` up to 5 `[ERROR] File.java:[line,col]` lines with the
workspace prefix removed, and `missingDependencies` reports Maven's offline
"has not been downloaded" error. Each entry is printable ASCII cut to 100
characters before escaping. Tekton stores results JSON-escaped in each step's
termination message, and the kubelet gives each container min(4,096, 12,288 /
containers) bytes: 1,755 for the seven containers of a Maven task pod, 3,072
for `npm-lock-update`. Summaries are trimmed (fewer list entries, then none,
with `truncated: true`) until their escaped size fits that budget. A live
`dependency-audit` summary of 2,754 bytes was cut to 1,755 and its pipeline
failed with `CouldntGetPipelineResult` before this rule.

**The verdict is `mavenExitCode`, not the task status.** Tekton publishes no
pipeline results from a failed task, so the Maven step uses `onError: continue`
and the summary step succeeds whenever it wrote a summary. A failing build
therefore ends in a *succeeded* PipelineRun whose summary carries the failing
tests and compiler errors. A run passes only when the PipelineRun succeeded,
its `commit` equals the requested revision and the exit code is 0; the run tool
(`passed`) and the adapter apply that rule. `maven-args` is an array passed as separate argv entries, never
through a shell; parameters reach scripts only through the environment, and
scripts revalidate them.

**`maven-dependencies`** is the same task shape with the cache writable, network
allowed, and `-Dmaven.test.failure.ignore=true`: Maven resolves test providers
and packaging plugins only when those phases execute, and an offline build
needs all of them. Measured: a cache filled by a run that stopped at a test
failure lacked the jar plugin's dependencies, so the offline build compiled and
passed 72 unit tests and then failed packaging.

**`npm-lock-update`** runs `npm update <packages> --package-lock-only
--ignore-scripts --no-audit` (mode `update`) or `npm audit fix
--package-lock-only --ignore-scripts` (mode `audit-fix`) in `directory` with the
pinned Node 24 image. No install script runs and no `node_modules` is written.
npm works on copies of `package.json` and `package-lock.json` in a scratch
directory, so a project `.npmrc` (registry, auth, git settings) is never read;
user and global configuration point at an empty file, `git` is disabled and
the registry is always set explicitly (the public registry or the proxy). An
egress gate (in Node; the image has no curl) runs first. The summary also lists
the hosts the lockfile resolves from and any new ones (`newResolvedHosts`), and
the verdict is `npmExitCode`.
Packages are names only (at most 16; no URLs, paths, versions or shell syntax):
`npm update` rejects versions (`EUPDATEARGS`) and stays within the
`package.json` ranges. The summary lists up to 25 changed lockfile entries with
old and new versions (or `resolutionChanged` when only the resolved URL or
integrity changed), the diff size and its SHA-256; the full diff and lockfile are
written under `npm-lock-update/` in the run's source workspace.

The pipelines use the image's pinned Maven rather than the project's `./mvnw`,
so the build tool is reviewed and pinned instead of downloaded by a script in
the candidate tree.

### Test services

Services are Tekton sidecars that start only when the `services` parameter
names them, and otherwise idle. Tekton waits for their readiness probes before
the first step. Supported: `postgres` (PostgreSQL 17.12, the same pinned image
as the `verification.services` example) and `mongodb` (MongoDB 8, pinned; not
yet exercised by a module). Each listens on `127.0.0.1` only, runs as its own
non-root user with a read-only root filesystem and a size-limited `emptyDir`.
PostgreSQL uses trust authentication for user and database `ci`: there is no
credential, and the data exists only for the life of the pod. The Maven step
exports:

```text
QUARKUS_DEVSERVICES_ENABLED=false
QUARKUS_DATASOURCE_USERNAME=ci
QUARKUS_DATASOURCE_JDBC_URL=jdbc:postgresql://127.0.0.1:5432/ci
QUARKUS_DATASOURCE_REACTIVE_URL=postgresql://127.0.0.1:5432/ci
QUARKUS_MONGODB_CONNECTION_STRING=mongodb://127.0.0.1:27017   (with mongodb)
```

Dev Services and Testcontainers are off because the pod has no container
engine. Kafka (needed by event-statistics and rest-fights) and MariaDB
(grpc-locations) are pending.

### Dependency cache

Each repository has its own claim (8 GiB), `ai-sdlc-ci-maven-cache-` plus the
first 16 hex digits of the SHA-256 of `owner/name`. The installer creates one
per `--tekton-ci-repository` (repeatable; default: `--repository`), and runs
derive the name from their own repository, so a run can only bind its own
repository's cache, and a repository without one cannot run Maven pipelines.
`maven-dependencies` is its only writer, and the run tool allows it only when
the revision is the current head of the catalog's base branch (read with an
anonymous `git ls-remote`), i.e. reviewed, merged code. Every other run mounts
it read-only, so a candidate cannot poison it for later runs. A candidate that
adds a dependency fails offline with `missingDependencies: true` until the
dependency is in the cache.

On a multi-node cluster the claim needs `ReadOnlyMany`/`ReadWriteMany` storage
or node affinity; the local kind node is single-node.

### Sandbox

`maven-build-test`, the only task that runs candidate code, runs under the
`ai-sdlc-offline` RuntimeClass (gVisor, `network = "none"`) through the
PipelineRun's per-task pod template. Install it with `./install.sh sandbox`;
admission refuses a `maven-build-test` run whose `build-test` task does not
request it, and the run tool also checks that the node's per-scope task limit
was raised (otherwise: re-run `./install.sh sandbox`). Fetch tasks never use it: the class has no network, so an
npm run under it failed with `EAI_AGAIN`.

**Node task limit.** gVisor runs every container of a pod (sandbox, gofers and
systrap stubs) inside the systemd scope of the pod's first container, and
systemd caps each container scope at `DefaultTasksMax`, 15% of the node's pid
limit: 307 for a kind node under podman's default of 2048. A Tekton task pod
has two init containers plus one container per step and sidecar; with three or
more, the sandbox died with `failed to create new OS thread (have 57 already;
errno=11)` and the kubelet reported `creating root file system: input/output
error` or `the sandbox exited before the container started`. One- and
two-step tasks, emptyDir volumes and PVC workspaces all worked, which is how the
cause was isolated (runsc debug logs showed the panic).

`./install.sh sandbox` (and every `./install.sh local` that installs the
offline runtime) now sets two node limits:

- **Per-scope tasks:** a reviewed systemd drop-in
  (`runtime/k8s/deploy/local/offline-runtime/systemd-tasks.conf`,
  `DefaultTasksMax=768`) sized from the measured peak of a sandboxed Maven
  build-test (603 tasks) plus a 27% margin. It re-executes the node's systemd
  manager, verifies the value and records `scopeTasksMax` in its report. It
  only ever raises the default: a node without a pid limit (docker) keeps its
  larger default and gets no drop-in. It refuses a node whose pid limit is
  below twice the value, and a drop-in that ai-sdlc did not write. **It applies
  to every container scope created afterwards, sandboxed or not.**
- **Per-pod pids:** the kubelet `podPidsLimit` (768, `kubelet-pods.json`), in
  `deploy/local/kind.yaml` for new clusters and added to an existing node's
  kubelet configuration atomically: the installer stages the file, checks it is
  the original plus the reviewed value, moves it into place, restarts the
  kubelet and confirms the value through the node's `/configz`; on failure it
  restores the original and restarts the kubelet again. Pods created before
  the change keep running without the bound until they are recreated. A value
  that an earlier ai-sdlc revision set (marked, or listed under `previous`,
  such as the earlier 1024 in kind.yaml) is upgraded; any other is refused.
- **CI concurrency:** a ResourceQuota (`ai-sdlc-ci-pods`) allows two pods in
  `ai-sdlc-ci`, so at most two PipelineRuns make progress at once, each with
  one task pod; Tekton retries a task pod refused by the quota until a pod
  ends. Tekton's affinity assistant is disabled (`coschedule: disabled`):
  with it, each run held an assistant pod, and two concurrent runs took the
  whole quota while their task pods stayed pending, a deadlock observed in the
  disposable cluster. Tekton does not remove assistants that an earlier
  configuration started once the mode changes, so the installer deletes
  affinity-assistant StatefulSets whose PipelineRun is finished or gone
  (`releasedAssistants` in its report) and keeps those of runs still in
  progress.

The arithmetic for a podman kind node (2,048 pids): CI pods hold at most 2 x
768 = 1,536 pids (75%), the rest is left for the runtime and system pods, each
of which is also capped at 768. A runc Maven pod with tests peaked at 146
pids and the sandboxed build-test at 603, so legitimate runs fit with margin,
while a fork bomb in any one pod stops at 768.

Changing the value: edit the reviewed files and re-run `./install.sh
sandbox`; the installer replaces its own earlier drop-in and kubelet line.
Upgrading from an install without them works the same way, and until then the
run tool refuses sandboxed runs with "re-run ./install.sh sandbox". Removal:
delete `/etc/systemd/system.conf.d/ai-sdlc-offline-tasks.conf` on the node and
run `systemctl daemon-reexec`, and remove the marked `podPidsLimit` lines from
`/var/lib/kubelet/config.yaml` and restart the kubelet; sandboxed Tekton pods
then fail again.

Measured: `maven-build-test` rest-villains with the PostgreSQL sidecar under
gVisor passed 94/94 (72 unit, 22 integration) in 2 min 1 s of Maven, against
1 min 42 s with the default runtime.

### Maven proxy

The `verification.dependencies` work adds an in-cluster Maven and npm proxy
(Service `dependency-proxy` in namespace `ai-sdlc-dependencies`, port 80 to
pods labelled `app.kubernetes.io/component: dependency-proxy` on 8080; nginx
forwards only to one upstream host and allowed path prefixes). The CI side of
its contract is already installed: policy `ai-sdlc-ci-dependency-proxy` lets
pods labelled `ai-sdlc.io/ci-egress: dependency-proxy` reach only the proxy pods
(Maven 8080 at `/maven2`, npm 8081) and nothing else, not even DNS, so callers
pass the proxy Service's cluster IP; the proxy admits that label from
`ai-sdlc-ci`. No task carries the label yet. Once the proxy is installed:

1. Switch `maven-dependencies` and `npm-lock-update` from the `fetch` label to
   `dependency-proxy` (one line per Task). `source-checkout` keeps GitHub HTTPS.
2. Pass the proxy as `maven-mirror-url` (`--maven-mirror-url` in the run tool;
   the Maven step writes one `mirrorOf *` entry) or `registry-url`
   (`--registry-url`).

Until then dependencies come from Maven Central and the npm registry over
public HTTPS.

### Security boundaries and network exposure

Tests run candidate code. Maven plugins declared by the candidate POM also run
code, so the build and test phases are equally untrusted; they run only in
`maven-build-test`, which has no network. The controls are:

- **No credentials.** The `ai-sdlc-ci-runner` ServiceAccount has no Role and
  `automountServiceAccountToken: false`, as does the namespace's `default`
  account; the run's pod template repeats it. No Secret, `valueFrom` or
  `envFrom` appears in any Task (tests enforce this).
- **Admission.** The `ai-sdlc-ci-pipelineruns` ValidatingAdmissionPolicy admits
  a PipelineRun in `ai-sdlc-ci` only when it references a catalog pipeline,
  uses the runner account without a token (also in per-task overrides), and
  sets no `ai-sdlc.io/ci-egress` label on the run or a per-task override
  (labels propagate to pods, where the network policy reads them), and binds
  only a fresh `source` claim (no data source or pre-bound volume) and a
  repository cache, read-only except for `maven-dependencies`. Only cluster
  administrators may create `maven-dependencies` runs, which write a cache.
  Admission cannot read GitHub or compute the claim digest, so the rules that
  a cache write uses the base-branch head and the repository's own claim are
  enforced by the callers (the run tool and the adapter).
  Run fields are allowlisted: the spec may set only `pipelineRef`, `params`,
  `timeouts`, `taskRunTemplate`, `workspaces` and `taskRunSpecs`; pod templates
  only `automountServiceAccountToken`, `securityContext` and the
  `ai-sdlc-offline` runtime class; step and sidecar overrides only
  `computeResources`; the source claim only `accessModes` and `resources`. Env,
  volumes, DNS settings or host aliases in a pod template would otherwise let a
  run fake the address the egress gate probes or load a candidate file into a
  fetch task with `NODE_OPTIONS`. A second policy keeps a PipelineRun's egress label from
  changing on update (labels propagate to pods created later); cancellation
  stays allowed. **RBAC must never grant create, update or patch on TaskRuns,
  CustomRuns or Pods in `ai-sdlc-ci`**: the policies cover PipelineRuns, the
  Tekton controller creates TaskRuns and Pods itself, and a label patched onto
  a pod changes its egress at once.
- **Restricted pods.** The namespace enforces restricted Pod Security. Every
  step and sidecar is non-root, drops all capabilities, disallows privilege
  escalation, uses RuntimeDefault seccomp, a read-only root filesystem,
  size-limited `emptyDir` volumes and CPU/memory limits. No hostPath.
- **Pinned images.** Every Task image and every Tekton image is pinned by
  digest; the installer refuses unpinned images.
- **Network.** `ai-sdlc-ci-default-deny` denies all ingress and egress in the
  namespace. Pods of the fetch tasks (`source-checkout`, `maven-dependencies`,
  `npm-lock-update`, selected by the `ai-sdlc.io/ci-egress: fetch` label that
  Tekton copies from each Task to its pods) may use cluster DNS and TCP 443 to
  any address **outside** private, loopback, link-local, shared (100.64/10), benchmarking (198.18/15), IETF protocol (192.0.0/24)
  and multicast ranges. No pod can reach the Kubernetes API, node addresses,
  cluster Services, other pods or the host network. `maven-build-test` has no
  network at all.

**Remaining exposure, until the proxy exists:** `maven-dependencies` executes
the base branch's build plugins and tests with public HTTPS egress. That code
is reviewed and merged, but a compromised dependency could use the channel.
`npm-lock-update` runs no package code. With the proxy, both narrow to the
proxy only; path-level filtering belongs in the proxy.

- **Enforcement before candidate code.** On the pinned kind network-policy
  agent a new pod was observed to be unrestricted for its first seconds: probes
  at start reached the API Service and plain HTTP; the same pod, 20 seconds
  later, could not. The Maven task therefore starts with an `egress-gate` step
  that requires three consecutive denials of the Kubernetes API Service
  (`KUBERNETES_SERVICE_HOST`) within 90 seconds. Only a connect timeout or
  refusal counts as denial; success, a TLS answer, a resolution error or a
  missing address resets or fails the gate, and a failed gate stops the task
  before Maven. `source-checkout` and `npm-lock-update` run no repository or
  package code and have no gate.

Measured on a disposable kind cluster after enforcement started: a pod with the
egress label resolved names and reached `repo.maven.apache.org` and
`github.com` on 443, and timed out on the API Service (`10.96.0.1`) and plain
HTTP; a pod without the label could not resolve names or reach anything.

## Run it for quarkus-super-heroes

[os-santiago/quarkus-super-heroes](https://github.com/os-santiago/quarkus-super-heroes)
is a fork of quarkusio/quarkus-super-heroes: Java 25, Quarkus, one Maven project
per service directory. Its upstream workflow `simple-build-test.yml` runs
`./mvnw -B clean verify` per module with Docker, and is disabled in forks.

1. Install as above with `--tekton-ci`.
2. Fill the cache from the base-branch head (the tool reads the head itself and
   refuses any other revision):

   ```bash
   P=runtime/k8s/deploy/tekton-ci/projects/quarkus-super-heroes.json
   python3 tools/tekton_ci_run.py maven-dependencies --cluster ai-sdlc --project $P \
     --module rest-villains --wait 3600
   ```

3. Build and test a pull-request head offline:

   ```bash
   sha=$(gh pr view NUMBER -R os-santiago/quarkus-super-heroes --json headRefOid -q .headRefOid)
   python3 tools/tekton_ci_run.py maven-build-test --cluster ai-sdlc --project $P \
     --module rest-villains --revision "$sha" --wait 3600
   ```

4. Update the UI's npm lockfile for one package, or apply audit fixes:

   ```bash
   python3 tools/tekton_ci_run.py npm-lock-update --cluster ai-sdlc --project $P \
     --revision "$sha" --directory ui-super-heroes/src/main/webui --package axios --wait 1800
   python3 tools/tekton_ci_run.py npm-lock-update --cluster ai-sdlc --project $P \
     --revision "$sha" --directory ui-super-heroes/src/main/webui --mode audit-fix --wait 1800
   ```

The tool validates every input before cluster access, checks the cluster's
ownership record, creates a PipelineRun that references the installed
pipeline, and with `--wait` prints the outcome and exits nonzero on failure.
Logs: `kubectl -n ai-sdlc-ci logs <pipelinerun>-build-test-pod -c step-maven`.

### Module catalog

| Module | Status | Services | Notes |
| --- | --- | --- | --- |
| rest-villains | supported | postgres | Playwright UI tests excluded (no browser). |
| rest-heroes | unverified | postgres | Reactive client; run with `--allow-unverified`. |
| rest-narration | unverified | none | In-process WireMock Dev Services may be disabled by the Dev Services switch. |
| event-statistics | pending | kafka | Needs a Kafka sidecar. |
| rest-fights | pending | mongodb, kafka | Needs MongoDB and Kafka sidecars. |
| grpc-locations | pending | mariadb | Needs a MariaDB sidecar. |
| ui-super-heroes | pending | none | Quinoa downloads Node.js/npm; tests need a browser. |

The exclusions use `-Dtest=!UIResourceTests,!*IT` and
`-Dit.test=*IT,!UIResourceIT`. `-Dtest`/`-Dit.test` replace the plugins'
include patterns, so each expression keeps its own class pattern: an
exclusion-only `-Dtest` made Surefire run the `*IT` integration tests in the
unit phase, where they fail.

Measured on 2026-11-27 (disposable kind cluster, podman), commit
`ae00455c01a7a50d89ae8f1db99483abd27abe25`:

| Run | Result |
| --- | --- |
| `maven-dependencies` rest-villains | succeeded; 94 tests, 0 failures; cache filled |
| `maven-build-test` rest-villains, offline, read-only cache | succeeded; 6 reports, 94 tests (72 unit, 22 integration), 0 failures; 1 min 42 s of Maven |
| `npm-lock-update` ui-super-heroes, `--package axios` | succeeded; axios 1.21.0 to 1.21.1, follow-redirects 1.16.0 to 1.16.3; 3,230-byte diff |

## Gate integration (stage 3 design)

### What the merge gate accepts today

`internal/mergegate` reads the effective rulesets and the head's check runs and
commit statuses:

- Every required check (`merge.required` in the project manifest and every
  `required_status_checks` entry of every effective ruleset) is a
  `{context, appID}` pair with `appID > 0`. A ruleset entry without an
  `integration_id` makes the gate refuse (`ErrGate`).
- A required check is satisfied only by **check runs** named `context` whose
  `app.id` equals `appID`, all completed with `success`.
- Every commit status present on the head must be `success`, but a status can
  never satisfy a required check.

So a Tekton result counts only as a check run from a known GitHub App.

### Options

| | A. Commit status with a narrowly scoped token | B. GitHub App check run (recommended) |
| --- | --- | --- |
| Gate change | Required: a new `status` kind of required check matched by context and creator, and acceptance of a ruleset entry without `integration_id`. | None. `merge.required: [{"context": "tekton-ci/maven", "appID": APP_ID}]`. |
| Source binding | Weak. GitHub rulesets cannot bind a status posted with a personal token to its source; any holder of `statuses: write` (other automation, a workflow token) can post the context. The gate would have to trust the creator login. | Strong. GitHub enforces `integration_id` natively and the gate checks `app.id`. |
| Credential | Fine-grained token, one repository, `Commit statuses: write`. Long-lived, user-bound. | App private key; short-lived installation tokens; permissions `checks: write`, `pull_requests: read`, `contents: read`, installed on the one repository. |
| Operator setup | Create a token. | Create and install an App. |

**Recommendation: B.** It fits the gate unchanged, keeps the source binding that
the gate's design depends on, and uses short-lived tokens. A is listed for
completeness; it weakens the gate for a smaller setup cost.

### Triggering

GitHub webhooks cannot reach a local kind cluster. Options:

1. **Bounded poller (recommended).** A small `tekton-ci-reporter` Deployment in
   its own namespace lists open pull requests of the one configured repository
   every 60 seconds (one API call; installation tokens allow 5,000 per hour),
   and for each new head SHA creates one `ai-sdlc-maven-build-test` run per
   `post` entry of the project manifest's `tekton` section (deduplicated by the
   `ai-sdlc.io/revision` label and target annotation) and an `in_progress` check run with `head_sha` set. When all runs
   for a SHA finish it completes the check run with `success` or `failure` and a
   summary built only from the bounded `summary` results; logs never leave the
   cluster. A restart recomputes state from labels and check runs.
2. **Publication flow creates the PipelineRun.** The runtime already knows the
   exact head it published, so there is no polling delay. But the runtime would
   need RBAC on Tekton resources and a dependency on Tekton CRDs, pull requests
   opened by people would not be tested, and the check run still needs a
   reporter. More coupling for less coverage.

The poller is the simpler secure option: it is decoupled from the runtime, it
covers every pull request, and its only cluster permission is
`create/get/list/watch` on `pipelineruns` in `ai-sdlc-ci`.

Fork pull requests run untrusted code from outside the repository. The reporter
should test only heads whose repository equals the base repository, or carry a
maintainer label. The pipeline's own controls (no credentials, restricted
egress) still apply to every run.

### Operator steps (not automated)

1. Create a GitHub App with `Checks: read and write`, `Pull requests: read`,
   `Contents: read`, no webhook, and install it only on the target repository.
   Record its numeric App ID.
2. Store the App private key with a future
   `./install.sh credentials --tekton-ci-app-key FILE` in a Secret mounted only
   into the reporter. It is never printed or written elsewhere.
3. Add `{"context": "tekton-ci/maven", "appID": APP_ID}` to the project
   manifest's `merge.required` and the same check, with that `integration_id`,
   to the repository ruleset.

### Stage 3 implementation plan

1. `runtime/k8s/components/tekton-ci-reporter` (Go): App JWT, installation
   token, bounded poller, PipelineRun creation from the catalog, check-run
   create and complete, idempotent on restart. Unit tests with a GitHub
   fixture like `mergegate/github_fixture_test.go`.
2. Installer: `--tekton-ci-reporter` with the App ID and repository, the
   credential command above, the Role and NetworkPolicy (egress to
   `api.github.com` and the API server only).
3. Validation: a pull request on the fork gets `tekton-ci/maven` from the App,
   and the merge gate's collector reports it as satisfied.

## Validation

```bash
python3 -m unittest discover -s tools -p 'test_tekton_ci*.py' -v
```

The tests validate the lock, configuration and pins; refuse corrupt,
oversized or redirected release inputs and foreign ownership; check
server-side apply without forced conflicts and the admission retry bound;
render the Tekton YAML and check its references, security contexts, network
policy and parameter handling; run the clone, Maven and summary scripts
locally against unsafe inputs and synthetic reports; and check PipelineRun
rendering and the run tool's bounds.

Disposable cluster run, 2026-11-27 (kind 0.33, podman, Tekton v1.15.5):

- `./install.sh local --tekton-ci` on a fresh maintenance installation:
  installed, admitted on the first dry run, ready; four further applies
  (after desired-state changes) reconciled without conflicts, and
  `./install.sh status --tekton-ci` reported ready.
- All pods (checkout, build-test with sidecars, dependencies, npm, affinity
  assistant) were admitted under restricted Pod Security.
- Network probes and the egress gate as described
  [above](#security-boundaries-and-network-exposure); the gate passed after 3
  probes in each run.
- The catalog runs in the [module catalog](#module-catalog) table.
- `go test ./internal/tektontasks/` with `AI_SDLC_TEKTON_API` pointing at a
  `kubectl proxy` ran `maven-build-test` through the Go adapter.

The installer does not prune objects removed from the desired state; this
first version has no earlier one to migrate from.
