# Enterprise validation with Homedir

Tracking: [#453](https://github.com/os-santiago/ai-sdlc/issues/453).
The adoption hypothesis is repeatable, governed autonomous delivery with
measurable user outcomes and cost. Architecture and passing infrastructure
checks alone do not validate that hypothesis.

## Boundaries and initial evidence

The software target is [Homedir](https://github.com/os-santiago/homedir), a
Java 21/Quarkus community application with bilingual UI and Chromium tests.
The initial inspected revision is
`0622cdaeb59a11a27bd5f3d7e18183c401a5e412`. Use a clean clone: an existing
operator checkout may contain unrelated work. Never normalize or discard it.

The [installation configuration](../examples/homedir.local-install.json)
selects a dedicated kind cluster and the future `ai-sdlc-production` test
environment. It does not authorize the public Homedir VPS. Issues are the
only runtime work input. Bootstrap development can improve AI-SDLC; a
supervising session must not produce the runtime candidate, inject stage
commands, write live business state, or substitute a PR/merge/release.

Homedir's inspected [review policy](https://github.com/os-santiago/homedir/blob/0622cdaeb59a11a27bd5f3d7e18183c401a5e412/config/docs/governance/PR_REVIEW_POLICY.md)
requires human approvals by risk; bot reviews do not count. The observed
GitHub main rule requested zero approvals, which does not cancel the written
policy. Eligibility must account for both and pause when required evidence is
absent. Existing SCC automation and in-progress PRs remain separate; do not
apply their trigger labels or run competing controllers on one issue.

The current runtime supports signed issue admission and blocked shadow plans.
Worker execution, independent verification, authoritative publishing/merge,
release and production rollback are not an installed E2E path. A healthy
cluster or an accepted shadow issue must never be reported as delivered software.

## Iterations and graduation gates

| Iteration | Concrete outcome | Required real validation | Tracking |
| --- | --- | --- | --- |
| 1: onboarding and baseline | Reproducible Homedir scope and issue intake | Fresh kind install, repeated reconciliation, enforced network probes, real GitHub issue → signed admission → blocked plan; selected application tests with failures preserved | #454 |
| 2: isolated candidate | SCC/Codex generates a bounded candidate from an eligible issue | Live executor cannot reach control-plane credentials/API; controller-loss and new-Pod isolation tests; time/cost caps; deterministic independent tests | #422, #419 |
| 3: delivery | Candidate → PR → required evidence → merge → release | Current issue/policy/base/head revalidation, independent verifier, genuine GitHub responses, review policy, no manual stage replacement | #436, #437, #438 |
| 4: trusted artifact | Build once and promote by verified identity | Real Konflux build, Conforma policy and negative provenance cases; failed evidence prevents promotion | #455 |
| 5: user outcome | Same digest deployed to kind production-test | Browser journeys, bilingual accessibility/layout, error/latency budget, unhealthy revision rollback | #439, #440 |
| 6: adoption economics | Repeatable outcomes compared with Fullsend | Equivalent isolated corpus and budgets, all failures/interventions included, cost per accepted change | #456 |

Each iteration records source SHA, runtime image digests, configuration/policy
identity, issue revision, test command/tool identity, start/end, exit status,
artifact references and remaining gaps. Report task completion rate with the
eligible denominator, rejected work, retries, interventions, p50/p95 lead time,
escaped defects, UX regressions and total model/infrastructure/review cost.
Unknown cost or timing is unknown, never zero. First establish a small smoke
corpus; expand toward 50–100 representative tasks before drawing economic
conclusions. This is an evaluation design, not an enterprise certification.

The [offline executor runtime](security/offline-executor-runtime.md) implements
the first hidden containment prerequisite (#459) of iteration 2. It installs
and tests gVisor without enabling model transport or generative work.

## Fullsend: reuse decisions before reinvention

Initial source inspection pins
[`9dfefd453376cbd4ae6aef7398422ef7d31f2b77`](https://github.com/fullsend-ai/fullsend/tree/9dfefd453376cbd4ae6aef7398422ef7d31f2b77),
licensed Apache-2.0. No Fullsend code is vendored or executed by this change.

| Inspected surface | AI-SDLC decision to validate |
| --- | --- |
| `internal/runtime/registry.go`, runtime capability matrix | Explicit per-adapter guarantees and unavailable controls; retain SCC as generative adapter until comparative evidence supports a change |
| `docs/normative/sandbox-env/v1/` | Versioned environment/credential contracts and drift checks; prompts/hooks are not the containment boundary |
| `e2e/behaviour/`, forge abstraction | Reuse behavioral testing patterns and compare equivalent outcomes; preserve existing signed AI-SDLC authority contracts |
| CI-dispatched sandbox architecture | Do not import CI orchestration into AI-SDLC; its executable control plane remains Kubernetes |

Source inspection demonstrates available designs, not security assurance or
performance superiority. A reuse PR must pin upstream code, preserve applicable
license/NOTICE material, assess dependencies and prove behavior in our target
environment. The benchmark must not modify public production or run a second
controller against AI-SDLC's live pilot issue.

## Konflux and artifact promotion

Use Konflux for deterministic build, integration and release evidence, not
generative workflow orchestration. Its [onboarding model](https://konflux-ci.dev/docs/getting-started/)
uses Application/Component resources and IntegrationTestScenarios; a Snapshot
identifies the component set. ReleasePlan and ReleasePlanAdmission connect
release intent to trusted managed-namespace policy. [Conforma policy evaluation](https://konflux-ci.dev/docs/compliance/policy-evaluations/)
must gate release on signed provenance and the required integration results.

The planned adapter binds image digest, expected source SHA, builder identity
and policy revision. Missing/tampered evidence, unexpected source/builder,
wrong digest or failed mandatory tests must prevent promotion. Promote the
same built digest; do not rebuild after verification. Assess the actual builder
against [SLSA v1.2](https://slsa.dev/spec/v1.2/) before claiming a level.

Konflux is not installed by the current local profile. Its [local installer](https://konflux-ci.dev/konflux-ci/docs/installation/install-local/)
supports setup without integration (`SKIP_SECRETS=true`); the integrated GitHub
build path requires a GitHub App and registry integration. An existing `gh` client token
does not manufacture that App identity. Stage its pinned resources, capacity
and Secret references separately, then validate build and rejection paths
before enabling an AI-SDLC promotion capability. Argo CD is not required for
this initial explicit Helm reconciliation.

## First pilot observation (2026-11-15 UTC)

- Fresh Podman/kind installation from the declared Homedir configuration:
  six ready runtime services, PostgreSQL, completed migrations and successful
  positive/deny/allow NetworkPolicy probes. This is a point-in-time check,
  not continuous fail-closed containment for future executor Jobs.
- Real [Homedir issue #1576](https://github.com/os-santiago/homedir/issues/1576):
  intake generation 1 reached `needs-human`, with two signed deliveries and
  receipts, zero plans, execution requests, attempts and release gates.
  The conservative classifier matched the issue's exclusion of authorization
  changes. Keep the stop; assess false positives without weakening high-risk
  admission ([#457](https://github.com/os-santiago/ai-sdlc/issues/457)). The
  observer now supports every declared admission outcome.
- Selected application baseline: 22 JavaScript tests and 25 Java/Quarkus tests
  passed at the Homedir revision above. Java invocation:
  `mvn -B -ntp test -Dtest=I18nIntegrityTest,CommunityReputationNavExposureTest,CommunityBoardResourceTest`
  using Maven 3.9.11/Temurin 21. JavaScript invocation used Node 22 with
  `node --test tests/js/dom-injection-xss.test.js tests/js/notifications-center-render.test.js tests/js/utils-format-date.test.js`.
- Real Chromium against the local Quarkus development server: 9 tests passed,
  zero skipped, failed or flaky, with retries disabled. Run
  `npx playwright test homepage.spec.ts focus-indicators-wcag.spec.ts --workers=1 --retries=0 --reporter=json`
  from `tests/e2e` after `npm ci --ignore-scripts`, using Playwright 1.62.1.
  This validates existing public pages and focus indicators, not the pending
  long-name regression or an AI-SDLC-generated improvement. Selected run
  duration was 9.710 seconds; it is not a production performance benchmark.
- Failed setup attempts are retained: the first Java run could not resolve a
  worktree's external `.git` inside the container; a standalone clone fixed
  the harness. A later container start failed because its cache directory
  was absent; creating the task-owned directory resolved that setup error.
  Neither failure was a software regression or an autonomous candidate run.

Observe the same live issue without advancing it:

```bash
python3 tools/verify_shadow_intake.py --cluster ai-sdlc-homedir \
  --provider podman --repository os-santiago/homedir --issue 1576 \
  --expected needs-human
```

Sanitized local reports live under
`~/.local/state/ai-sdlc/ai-sdlc-homedir/`: `installation.json` includes the
resolved configuration and images; `shadow-verification.json` records the
observed outcome. Reapply using `local --config`; the separate
`verify_local_install.py` currently targets the default maintenance scope and
is not a generic Homedir/shadow verifier. These observations establish pilot
onboarding and safe stopping, not autonomous delivery or improved Homedir UX.
