# Runtime upgrades and installation details

This page holds the detailed installation behaviour and the closed
`runtime-upgrade` transitions that the README previously described inline.
Each transition is a journaled, fenced operation with its own scope; the
linked procedure is authoritative.

One entrypoint installs verified Kubernetes clients, creates kind and applies
the versioned Helm chart. Each of the six runtime services has its own
container and Deployment; PostgreSQL uses persistent storage. A short-lived
migrator Job owns schema setup, while services receive separate database
credentials and signing identities. Images come
from [Quay](https://quay.io/repository/os-santiago/ai-sdlc) and are pinned by
digest in Git. Reapply the same command to reconcile desired state. To refresh
pins from a reviewed publication, use the [image upgrade procedure](image-lock-upgrades.md).
`./install.sh upgrade-plan` compares explicit published versions before mutation;
its [preflight report](image-lock-upgrades.md#plan-a-transition-before-changing-an-installation)
does not authorize upgrading an active installation. For an original pre-candidate
runtime, `runtime-upgrade pause` preserves history while stopping producers;
`runtime-upgrade backup` verifies a private copy by restoring it into an isolated
database. The bounded `runtime-upgrade diagnostic` transition applies the
reviewed diagnostic migration and worker image selection, then opens intake last.
For an installation on that diagnostic bridge, `runtime-upgrade tool-budget`
performs the reviewed 16-to-24 tool-step adjustment, including pause, a new
restore-verified backup and reactivation. Provider quotas remain unchanged.
After that update completes, `runtime-upgrade reapprove` can install separately
reviewed [finite approval windows](approval-window.md) for the
same test environment without renewing workflow requests or failed attempts.
After an expired, contained review failure, `runtime-upgrade reviewer-image`
can [replace only the reviewer image](reviewer-image-upgrade.md)
while retaining the candidate, verification and failed-review history.
See the same procedure for scope, prerequisites and recovery limits. The next
closed transition, `runtime-upgrade browser`, installs schema-35 browser evidence
through a separate backup, migration and activation journal. See its
[scope and reviewed trust input](browser-verifier.md#existing-installation-transition).
From that completed installation, `runtime-upgrade review-feedback` provides the
separate [schema-35-to-36 feedback transition](review-feedback-upgrade.md).
If its approval documents have expired, use the explicit
[browser approval renewal](browser-approval-window.md) before
starting that transition; existing workflow deadlines remain unchanged.
It preserves failed review history and installs future rejection feedback;
autonomous repair remains a separate capability.
The later [schema-36-to-37 repair transition](review-repair-upgrade.md)
enables one bounded repair and renews both approvals in the same operation.

For an existing installer-owned Konflux cluster, use the
[frozen runtime capability](frozen-konflux-runtime.md) and
`examples/saferus-konflux-frozen.local-install.json`. It preserves the platform identity and
keeps Saferus delivery disabled while integration is validated.

An explicit [closed-build capability](build-runtime-install.md)
connects signed merged-source dispatch, relay and the build controller on that
platform. Prepare its [persistent foundation](build-foundation.md)
through `install.sh build-foundation` before selecting the approved bundle. It requires a separately approved current profile and original storage
slots; installation does not authorize release or demonstrate the full E2E.

This first release installs **maintenance mode**, with delivery actions
disabled. It verifies infrastructure installation, not autonomous
issue-to-production delivery. No GitOps controller is installed; reconciliation
is explicit through Helm. See [installation and configuration](../installation.md)
for credentials, supported scope, limitations and recovery guidance.

Before installing the remaining delivery capabilities on an owned, frozen
Saferus Konflux environment, `./install.sh prepare --cluster <name> --images
<reviewed-images.lock.json>` verifies an empty current database and pauses all
delivery services through Helm. It preserves PostgreSQL and frozen authority;
ordinary `local` reapplication cannot silently resume it. The same refusal
holds while an original runtime is paused by `runtime-upgrade pause`:
reapplying `local` refuses until the recorded transition or `recover-original`
releases the `runtime-upgrade-pause.json` journal. See
[initial preparation](build-runtime-install.md#initial-empty-runtime-preparation)
for prerequisites and the distinction from upgrading a runtime with history.

For local GitHub client access, run `./install.sh credentials --github-auth gh-cli`
after installation and `gh auth login`. See the
[credential setup](../installation.md#optional-local-github-client) for its scope
and explicit refresh command.

The reviewed `shadow` profile adds real GitHub issue polling, signed admission
and durable blocked plans. It does not execute generated code or publish PRs:

```bash
./install.sh local --profile shadow --github-auth gh-cli
```

Only issues carrying `ai-sdlc:shadow`, labeled by the configured local account,
can enter that profile. See [shadow intake](../installation.md#shadow-intake)
for scope, verification and returning to maintenance.

The opt-in [candidate profile](candidate-runtime.md) adds signed
issue-to-candidate execution with an isolated SCC executor, a containerized Codex
gateway and terminal crash recovery. It requires reviewed matching images and
stops at an unvalidated candidate; it does not authorize PR publication or release.

The explicit [verification profile](independent-verification.md#install-the-independent-verifier)
adds a separate verifier service and four offline validation Pods. It requires
a matching published verifier image and stores independent behavior evidence;
publication, merge and promotion remain disabled.

With reviewed compatible images, verification can also enable an
[independent read-only SCC review](independent-review-installation.md)
using `"independentReview": true` in the installation JSON. The reviewer has a
separate offline namespace, model conversation and durable evidence journal.
Publication and deployment remain disabled.

The Homedir verification pilot can additionally opt into
[verified PR publication](publication-installation.md) with
`"publication": true`. Its release manager has a separate GitHub credential and
requires signed behavior/review evidence. Publication creates a bounded PR;
merge, release and production promotion remain separate gates.

With compatible images and verified native protection, the separate
[low-risk merge capability](merge-installation.md) can be enabled
with `"merge": true`. This does not enable release or production promotion.

The [Saferus reference adapter](../saferus-reference-project.md) provides a
separate, finite token-lifetime UI recipe with repository-bound publication and
merge. Its isolated validation is not an autonomous issue-to-production result.

The [Homedir enterprise pilot](../enterprise-pilot.md) uses a separate,
declaratively configured installation and real product validation. Start with
`./install.sh plan --config examples/homedir.local-install.json`; the example
grants shadow intake only. Its roadmap includes independent execution,
Konflux provenance gates and a measured Fullsend comparison.

For a separate local Konflux environment, use the same installer:

```bash
./install.sh supply-chain plan
./install.sh supply-chain local
./install.sh supply-chain status
```

This optional installation needs at least 8 GiB available memory and creates
its own kind cluster. It installs pinned upstream dependencies and a Helm-managed
Konflux operator, with no default users or UI host ports. See
[local Konflux setup](konflux-local.md) for prerequisites,
reapplication, evidence and the remaining build/provenance integration work.

Local installation also verifies NetworkPolicy enforcement with temporary
connectivity probes against the network plugin included in the pinned kind image;
see the [installation boundaries](../installation.md#installation-readiness-versus-autonomous-delivery).

## Supply-chain and registry profiles

For supply-chain component validation, see the [isolated Homedir/Konflux fixture](konflux-homedir-fixture.md). It records actual builds and signature checks without advancing live delivery.

For a new Saferus demonstration cluster, the [cold Helm registry profile](build-runtime-install.md#cold-installation-with-a-helm-owned-registry) adds native read-only provenance credentials and retained local registry storage. It preserves existing installations and does not activate autonomous delivery by itself.
