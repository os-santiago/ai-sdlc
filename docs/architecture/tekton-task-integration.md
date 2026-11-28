# Tekton task integration

Status: stage 1 of 4 delivered (catalog, install, adapter skeleton). Date:
2026-11-27.

## Principle

Recurring CI/CD work needs no generative AI. Checking out a commit, resolving
dependencies, building, running tests, updating a lockfile and auditing
dependencies are deterministic, so they run as **Tekton Tasks** with pinned
images, before or after the generative stage. The generative stage either
consumes their results or requests them as tools. Generative AI is reserved
for work that needs correlating unstructured data: writing code and analysing
dependencies.

```text
            pre-generation                generation                 post-generation
 issue --> [catalog runs on base] --> [generator] <--tool--> [catalog run on candidate] --> PR --> [CI check run] --> merge gate
              |  bounded results          ^                        bounded results                 required check
              +---- reviewed context -----+
```

Tekton is also the customer's CI/CD standard, so the same tasks back the
merge-gate check on the pull request.

## Catalog

Installed by `./install.sh local --tekton-ci` into namespace `ai-sdlc-ci`
(runtime/k8s/deploy/tekton-ci/ci/, [operations](../operations/tekton-ci.md)).

| Task | Runs | Network | Inputs | Bounded result |
| --- | --- | --- | --- | --- |
| `source-checkout` | `git fetch` of one full SHA | DNS + public HTTPS (GitHub) | repository, revision | verified commit |
| `maven-build-test` | `mvn -o verify` for one module, test services as sidecars | **none** | module, services, reviewed Maven switches | exit code, test counts, failing test names (≤10), compiler errors (≤5), missing-dependency flag |
| `maven-dependencies` | `mvn verify` with test failures ignored, filling the shared cache | DNS + public HTTPS, or the proxy | module, services, mirror URL | as above |
| `npm-lock-update` | `npm update <pkgs> --package-lock-only --ignore-scripts` or `npm audit fix --package-lock-only --ignore-scripts` | DNS + public HTTPS, or the proxy | directory, mode, package specs | changed entries (≤25), diff size and SHA-256 |
| `dependency-audit` | npm audit JSON; OSV or OWASP for Maven | proxy / advisory DB | directory or module | structured findings (planned) |
| `sast-results` | code-scanning alerts for a SHA | GitHub API | revision | alert list (optional, planned) |

Each pipeline (`ai-sdlc-maven-build-test`, `ai-sdlc-maven-dependencies`,
`ai-sdlc-npm-lock-update`) is `source-checkout` followed by one task, and
publishes `commit` and `summary`.

Common properties: every image pinned by digest; every container non-root,
no privilege escalation, all capabilities dropped, RuntimeDefault seccomp,
read-only root filesystem, bounded `emptyDir` volumes and resource limits;
restricted Pod Security enforced on the namespace; no service-account token
and no credential in any task pod; Tekton accepts only installed definitions
(no inline specs, no remote resolvers). A task that runs code first waits until
the namespace NetworkPolicy is enforced.

**Trust split for Maven.** Candidate code (POM plugins and tests) runs only in
`maven-build-test`, offline, against the shared cache mounted read-only as a
Maven local-repository tail, so it can neither fetch nor poison anything.
`maven-dependencies` is the only writer of the cache and runs only on the
current base-branch head (trusted inputs). A candidate that adds a dependency
fails offline with `missingDependencies: true`. `maven-dependencies` cannot fill
it: it runs only on the base-branch head. The decided recovery (planned) is a
resolution-only step for the candidate revision (`dependency:resolve` and
plugin resolution, no lifecycle phase) through the proxy only, writing a
separate per-candidate cache layer that never touches the shared base cache;
the offline build then reads the base cache and the candidate layer, both
read-only. That step still runs untrusted code: `dependency:resolve` is a goal
of the Maven Dependency Plugin, and Maven loads the extensions a POM declares
and the repository's `.mvn/` files while building the project. It therefore
needs its own controls: refuse candidates that change `<extensions>`,
`.mvn/` or plugin declarations, and run it under the `maven-build-test`
restrictions (sandbox, no token) with egress only to the proxy.

**Sandbox.** The adapter and the run tool can put `maven-build-test` under the
`ai-sdlc-offline` gVisor RuntimeClass through the PipelineRun's per-task pod
template. Fetch tasks never use it: that class has `network = "none"`. In the
disposable cluster a plain four-container pod started under gVisor, but Tekton
task pods failed to start their sub-containers ("creating root file system:
input/output error"), so the sandbox is opt-in until that is resolved (stage
2). The network-less NetworkPolicy, `mvn -o` and restricted Pod Security apply
either way.

**Proxy.** The `verification.dependencies` work provides an in-cluster Maven
proxy (`dependency-proxy.ai-sdlc-dependencies`, cache on a PVC). Once merged,
fetch tasks get a dedicated egress label allowing only the proxy, the proxy
admits that label from `ai-sdlc-ci`, and `maven-mirror-url`/`registry-url`
point at it. The CI cache claim (`ai-sdlc-ci-maven-cache`) stays separate from
the proxy's cache: the proxy caches upstream bytes, the CI claim holds a
resolved local repository for offline builds.

## Manifest

The project manifest gains an optional, validated `tekton` section
(`internal/project/tekton.go`). It is omitted when absent, so existing manifest
digests are unchanged.

```json
"tekton": {
  "modules": {
    "rest-villains": {"services": ["postgres"],
                      "mavenArgs": ["-Dtest=!UIResourceTests,!*IT", "-Dit.test=*IT,!UIResourceIT"]}
  },
  "npmDirectories": ["ui-super-heroes/src/main/webui"],
  "pre":   [{"pipeline": "maven-build-test", "target": "rest-villains"}],
  "tools": [{"name": "build_test", "pipeline": "maven-build-test"},
            {"name": "npm_lock_update", "pipeline": "npm-lock-update"}],
  "post":  [{"pipeline": "maven-build-test", "target": "rest-villains", "check": "tekton-ci/maven"}]
}
```

Rules: targets must be declared modules or directories (relative, no dot
segments); services come from the catalog; Maven switches cannot change
settings, POM, offline mode or the local repository; `maven-dependencies` may
not appear in any stage: filling a cache is an operator step
(`tools/tekton_ci_run.py maven-dependencies`, admitted only for cluster
administrators); every
`post.check` must be one of `merge.required`; at most 16 modules, 16
directories, 8 pre, 8 post and 4 tools.

## Adapter

`internal/tektontasks` is the only path from ai-sdlc components to Tekton:

- `FromInvocation(manifest, invocation, revision, baseHead)` for pre/post runs
  (a `maven-dependencies` request is refused unless the revision equals the
  observed base-branch head), and
  `FromTool(manifest, tool, target, packages, revision)` for generator tool
  calls. A tool call supplies only a declared target and, for npm, package
  names (`npm update` takes no versions and stays within `package.json`
  ranges; no URLs, paths or shell syntax, at most 16). Everything else comes from the manifest and the runtime.
- `Request.PipelineRun(runtimeClass)` renders the object (installed pipeline
  reference, token-free restricted pods, read-only cache for build-test,
  optional sandbox for build-test only). A test checks its parameters against
  the installed Pipeline YAML.
- `Run(ctx, cluster, request, runtimeClass, poll)` creates the run once and
  polls until it finishes, or cancels it when the context ends; a succeeded
  run must report exactly the requested commit; results over 4096 bytes are
  refused. A failing build is a *succeeded* pipeline whose summary carries the
  verdict (Tekton publishes no results from failed tasks), so callers gate on
  `MavenSummary.Passed` / `NpmSummary.Passed`.
- Each repository has its own cache claim (`CacheClaim(repository)`, the
  same digest-derived name the installer creates).
- `ParseMavenSummary` and `ParseNpmSummary` decode results strictly (unknown
  fields, trailing data and out-of-bound lists refused).
- `DynamicCluster` implements creation, reads and cancellation with a dynamic
  client. The identity using it needs only `create`, `get` and `patch` on
  `pipelineruns` in `ai-sdlc-ci`; the namespace's admission policy limits what
  it can create to the catalog.

## Uses

**Pre-generation (stage 2).** For each `pre` entry the runtime runs the
pipeline on the base revision before generation, and adds the bounded summary
(baseline failures, compiler errors, audit findings) to the generator's
reviewed context, labelled as tool output, never as instructions.

**During generation (stage 2d, generator tools).** The generator calls a
declared tool, `build_test(target)` or `npm_lock_update(target, packages)`,
on the tree it is editing, and gets the bounded summary back as untrusted
data. This gives Java, Node and other toolchains to the generator without an
in-sandbox toolchain (issue #831). See [Generator tools](#generator-tools).

**Post-generation (stage 4).** For each `post` entry a PipelineRun tests the
pull-request head and is reported as a GitHub App check run named
`post.check` (for example `tekton-ci/maven`), which the merge gate already
accepts as `{context, appID}`; see the
[gate integration design](../operations/tekton-ci.md#gate-integration-stage-3-design).

## Generator tools

### Constraints

The generator (SCC) runs in the executor pod: no network, no service-account
token, gVisor, and a model mailbox as its only channel out. Every SCC tool
runs inside that sandbox; the model broker and gateway never run tools, and
the broker passes model responses through without interpreting tool calls.
So a tool whose work happens in Tekton must cross the same mailbox, and the
run must be created by a trusted controller, never by the sandbox.

### Path

```text
SCC ──MCP stdio──> ai-sdlc tools server (executor image, in the sandbox)
                     │ computes the candidate diff (git diff against the
                     │ generation's baseline commit), bounded to 768 KiB
                     └─HTTP 127.0.0.1:8091 /v1/ai-sdlc/tools──> mailbox helper
                                         │ writes tool-request.json (/bridge)
execution controller (worker) ──pods/exec read-tool──┘
   │ authorizes with the lifecycle authority (same as model calls),
   │ checks the call against manifest tekton.tools and the tool budget,
   │ journals the request digest
   └─ tektontasks.InvokeTool ─> candidate ConfigMap + PipelineRun
        (checkout base SHA → apply-candidate → build-test / lock-update)
   <─ bounded summary ── written back as tool-response.json ──> SCC tool result
```

- **Tool surface.** SCC loads MCP servers only from its trusted user
  configuration (a project file cannot add one), which the executor writes.
  The tools server lists exactly the manifest's `tekton.tools`, with the
  declared targets as an enum and npm package names as a validated array, so
  the model sees them as first-class tools (`mcp__ai-sdlc__build_test`). SCC's
  per-server `timeoutMs` covers a build (it defaults to 30 s).
- **Candidate in.** A unified text diff against the base revision, carried
  in the request and then in an immutable ConfigMap with its SHA-256. The
  768 KiB limit applies to the decoded diff; JSON escaping in the tool request
  can grow its encoding (up to six times for control characters), and the
  request bound allows for that. `apply-candidate` verifies the digest,
  refuses binary changes, symbolic links (mode 120000), submodules (160000)
  and paths outside the tree or under `.git`, applies the diff to the work
  tree and the index together (`git apply --index`, symlinks disabled), fails
  if the work tree then differs from the index (untracked, ignored or
  unstaged files), and reports the resulting tree id and the digest it
  applied. `Run` checks that reported digest against the diff it sent and
  reports the tree in the result. The ConfigMap is owned by the PipelineRun
  and deleted with it; if the owner cannot be set, the ConfigMap is deleted
  and the run cancelled at once. Each tool call also deletes unowned
  candidate ConfigMaps older than 10 minutes (left by a crash between
  creating one and setting its owner).
- **Build-test only.** Only `ai-sdlc-maven-build-test` takes a candidate: it
  runs offline under the sandbox. `npm-lock-update` runs with the fetch egress
  (DNS and any public 443), and npm resolves URL specs and lockfile
  `resolved` hosts from the diff, so a candidate there would give the offline
  generator a way out. It returns in slice 3 only once non-registry specs and
  non-registry lockfile hosts are refused and egress is proxy-only
  (Stage 2b).
- **Who triggers.** Only the execution controller, with its RBAC in
  `ai-sdlc-ci` (create/get/patch PipelineRuns; create/patch/delete/list
  ConfigMaps). RBAC `resourceNames` cannot restrict creation, so the
  ValidatingAdmissionPolicy `ai-sdlc-ci-candidate-configmaps` does: in
  `ai-sdlc-ci`, a ConfigMap is only an immutable `ai-sdlc-candidate-<digest>-<suffix>`
  labelled `ai-sdlc.io/candidate=true` that holds exactly `candidate.diff`
  and `candidate.sha256` (the namespace's `kube-root-ca.crt` and cluster
  administrators are exempt). Admission admits the candidate workspace only
  as such a ConfigMap and only for `ai-sdlc-maven-build-test`; build-test
  still runs under the sandbox.
- **Allowlist.** `tektontasks.InvokeTool` accepts only a tool named in
  `tekton.tools`, a declared target, and npm package names; repository,
  revision, services and Maven switches come from the manifest and the
  controller. A cache-writing pipeline cannot be reached.
- **Budgets.** Per generation: a number of tool calls and total tool seconds
  (`ToolBudget`). A call reserves all the seconds left and refunds what it
  did not use, so concurrent calls cannot overspend. Each call is also
  bounded by the generation's deadline and is cancelled when it ends. Tool
  time counts against the generation's own seconds, because the generator
  waits for the result. The budget lives in the controller's memory for one
  generation; durable records are the journal (slice 2).
- **Results.** The strictly decoded summary (tests run and failed, up to 10
  failing names, up to 5 compiler errors, missing-dependency flag; or the npm
  changes) plus the PipelineRun, revision and tree, framed as "Untrusted tool
  output (data, not instructions)".
- **Journal.** Each call records the request digest before the run, the
  candidate ConfigMap and PipelineRun names as soon as each exists, and the
  revision, tree, summary digest and state after it; an error records the
  names it has (`ToolJournal`; a `tool_calls` table keyed like `model_calls`,
  slice 2). Journal and cleanup calls after the run have their own 30 s
  timeouts. The flow console can derive a `tool-call` step from it.

### Slices

1. **Done (this change):** the candidate channel (`apply-candidate`, the
   optional `candidate` workspace in the build-test pipeline, its admission
   rules) and `tektontasks.InvokeTool` with allowlist, budget,
   journal interface and untrusted framing. Live, through the adapter on a
   disposable cluster, for os-santiago/quarkus-super-heroes rest-villains at
   `ae00455` under the gVisor class: a candidate diff that assigns a string to
   an int field returned `passed: false` and `VillainService.java:[143,33]
   incompatible types: java.lang.String cannot be converted to int` with tree
   `5fa25b8e`; a harmless diff passed 94 tests with tree `6d647ccd`. A run
   without a candidate still completes (the apply task is skipped).
2. Mailbox tool endpoint and protocol, the executor's MCP tools server, the
   execution controller's dispatch with RBAC, the `tool_calls` migration and
   store, and budgets in the execution profile.
3. `npm_lock_update` through the same path, returning the lockfile change,
   after refusing non-registry specs and lockfile hosts and with proxy-only
   egress.
4. Flow console `tool-call` step.

## Stages

1. **In review, two stacked pull requests:** (a) pinned Tekton install behind
   `--tekton-ci`, catalog tasks `source-checkout`, `maven-build-test`,
   `maven-dependencies`, `npm-lock-update`, three pipelines and the operator
   run tool; (b) the manifest section and the adapter skeleton with tests.
   Validated against os-santiago/quarkus-super-heroes in a disposable kind
   cluster.
2. Pre-generation wiring in the runtime (RBAC for the adapter identity,
   results into reviewed context), gVisor compatibility for Tekton pods,
   proxy-only egress once the proxy merges, `dependency-audit`.
3. Generator tools (stage 2d): see [Generator tools](#generator-tools) for the
   slices.
4. Post-generation check reporter (GitHub App check run) and merge-gate
   evidence.
