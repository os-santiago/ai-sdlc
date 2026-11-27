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

**Sandbox.** `maven-build-test` runs under the `ai-sdlc-offline` gVisor
RuntimeClass through the PipelineRun's per-task pod template; fetch tasks
never use it, because that class sets `network = "none"`. Tekton task pods
first failed under gVisor because systemd capped the sandbox's scope at 307
tasks; the offline runtime installer now raises the node's `DefaultTasksMax`
to 1,024 (measured peak 603) and bounds every pod with a kubelet
`podPidsLimit`, admission requires the sandbox for build-test, and the
sandboxed build passed 94/94 (stage 2a). The
network-less policy, `mvn -o` and restricted Pod Security apply as well.

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

**During generation (stage 3).** The generator calls a declared tool, for
example `build_test(target)` or `npm_lock_update(target, packages)`. The
candidate tree reaches the run without a push: the adapter writes the
candidate's bounded diff against the base revision (the existing workspace
bundle limits) into a ConfigMap bound as a read-only workspace, and an
`apply-candidate` step in the pipeline applies it after `source-checkout` and
verifies the resulting tree digest. Only the bounded summary returns to the
generator. This gives Java, Node and other toolchains to the generator without
an in-sandbox toolchain (issue #831). For `npm_lock_update` the lockfile diff
comes back through the same channel in the opposite direction (bounded by the
diff size the summary reports).

**Post-generation (stage 4).** For each `post` entry a PipelineRun tests the
pull-request head and is reported as a GitHub App check run named
`post.check` (for example `tekton-ci/maven`), which the merge gate already
accepts as `{context, appID}`; see the
[gate integration design](../operations/tekton-ci.md#gate-integration-stage-3-design).

## Stages

1. **In review, two stacked pull requests:** (a) pinned Tekton install behind
   `--tekton-ci`, catalog tasks `source-checkout`, `maven-build-test`,
   `maven-dependencies`, `npm-lock-update`, three pipelines and the operator
   run tool; (b) the manifest section and the adapter skeleton with tests.
   Validated against os-santiago/quarkus-super-heroes in a disposable kind
   cluster.
2. Pre-generation wiring in the runtime (RBAC for the adapter identity,
   results into reviewed context), gVisor for Tekton pods (2a, in review),
   proxy-only egress once the proxy merges, `dependency-audit`.
3. Generator tools: candidate diff transfer, `apply-candidate` step, tool
   schema in the model gateway, budgets.
4. Post-generation check reporter (GitHub App check run) and merge-gate
   evidence.
