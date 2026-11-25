# Helm chart installation without the installer

Release plan item B6 makes the `ai-sdlc-k8s-runtime` chart self-contained,
published and signed, so that a cluster administrator can install it without
the repository's local tooling (principle P5, audit gaps 5.2, 5.3 and 5.5;
issues #430, #431, #432, #434). This page records where that work stands:

1. what the chart still needs that only `install.sh` produces (the gap
   analysis that drives the next B6 changes);
2. how the chart is published and signed, and how an adopter verifies it;
3. the owner action the first publication needs.

The Secrets the chart consumes are specified separately in the
[Helm chart Secret contract](helm-chart-secret-contract.md).

Status on 2026-11-24: the chart is packaged, linted and rendered in CI on
every change. Publication and signing run only on pushes to `main`, and no
chart has been published yet. A chart-only installation is **not yet
possible** for any profile without the installer's identity bootstrap
(item C1 below).

## Gap analysis

`tools/install.py` and the modules it calls compute Helm values, create
Secrets, run bootstrap commands from the pinned migrator image and run
preflight checks before `helm upgrade --install`. Each item below is
classified as:

- **(a) chart-expressible values:** an administrator can already supply the
  value in a values file; the gap is documentation, a default, or deriving it
  inside the chart.
- **(b) documented Secret contract:** private material the administrator
  supplies, or generates once and adopts. Specified in the
  [Secret contract](helm-chart-secret-contract.md).
- **(c) chart hook or Job:** needs in-cluster computation the chart does not
  do yet.
- **(d) installer-only for now:** stays outside the chart in B6, either
  because it is a platform responsibility (P5: AI-SDLC never installs it) or
  because it belongs to a later item (operator R1, admission R5).

### Generated configuration documents and their digests

| # | Value(s) | What the installer does today | Class | Next step |
|---|---|---|---|---|
| V1 | `image.repository`, `image.tag`, `components.*.digest`, `migrator.digest`, `verifier.digest` | Reads `images.lock.json` and `images.evidence.json`; `install_images.py` resolves them and verifies each image's cosign signature before writing the lock | (a) values; signature verification (d) | Ship the signed image digests of the chart's revision with the published chart, so its defaults stop being `tag: unpublished` |
| V2 | `authority.policyJSON`, `authority.installationUID` | Runs `bootstrap-identities` in the pinned migrator image, merges the result with an existing policy, checks it with `validate-bootstrap`, and sets the installation UID to the `kube-system` namespace UID | (c) | Bootstrap Job (C1) that creates the identity Secrets and the policy together |
| V3 | `intake.configJSON` | `runtime/k8s/deploy/local/intake-policy.json` plus the repository and the GitHub client's `actorID` | (a) | Document the document's schema; `actorID` is public |
| V4 | `candidate.configJSON`, `review.configJSON` | Built from the worker and model-mailbox digests in the image evidence, the A-Dev profile digest, step, time and token budgets, and the RuntimeClass (`candidate_install.desired_values`) | (a), derived | Render both documents in the chart from digests and budget values, so administrators do not hand-assemble them |
| V5 | `modelGateway.*` | Validated from a model-upstream document (`--model-upstream`) | (a) | Document the values directly |
| V6 | `sandbox.runtimeClassName`, `sandbox.podOverhead` | The class name is an input; on OpenShift the overhead is read from the RuntimeClass | (a) | None: the worker and verifier already refuse a missing class or an overhead above the headroom |
| V7 | `candidate.apiServerCIDRs` | Read from the cluster's API server endpoints | (a) supplied value; discovery (d) | Document how to read them with `kubectl` |
| V8 | `candidate.credentialRevision`, `publication.credentialRevision` | The model and publication Secrets' `resourceVersion`, so that a credential refresh rolls the Pods | (a) optional | Document as an optional rollout trigger |
| V9 | `project.manifestJSON`, `project.verifierQuota`, `projectSHA256` in the policy | Compiled by the pinned migrator image and bound into the authority policy | (c) | Part of the bootstrap Job (C1) |
| V10 | `merge.configJSON` | Reconciled from the repository's GitHub ruleset | (d) | Stays with the installer until the operator (R1) |
| V11 | `build.*`, `provenance.*`, `release.*` | Reviewed Saferus fixtures bound to cluster and object UIDs | (d) | Out of scope for the generic chart path |
| V12 | `openshift.enabled`, `contract.inline`, `runtime.mode`, `namespace.*` | Set from the platform and profile files in `runtime/k8s/deploy/local` | (a) | Publish the profile values next to the chart |

### Secrets

| # | Secrets | What the installer does today | Class |
|---|---|---|---|
| S1 | `ai-sdlc-postgres-app` | Generates a password and a `uri` for the bundled PostgreSQL; CloudNativePG generates its own | (b) generated once and adopted; supplied for an external database |
| S2 | `ai-sdlc-database-roles`, `ai-sdlc-database-{role}` | Generates one distinct password per role, the matching `uri`, and the migrator's `passwords.json` | (b) now; (c) later, a generate-once hook |
| S3 | `ai-sdlc-identity-{role}` | Generated by `bootstrap-identities` together with the authority policy (V2) | (c) |
| S4 | `ai-sdlc-github-client`, `ai-sdlc-publication-client`, `ai-sdlc-codex-auth`, the model upstream credential, `ai-sdlc-provenance-registry` | Imports an authenticated `gh` token, an agent CLI auth file or a registry credential | (b) supplied |
| S5 | `ai-sdlc-model-client`, `ai-sdlc-review-model-client` | Generates a random token | (b) now; (c) later, a generate-once hook |
| S6 | `ai-sdlc-source-reader` (optional) | Never creates it; references an existing, owned, valid one by setting `verifier.sourceCredential` ([source availability](source-availability.md#authenticated-verification-and-review-reads)) | (b) supplied |

### Migrations and bootstrap Jobs

| # | Step | Today | Class | Next step |
|---|---|---|---|---|
| C1 | Identity and policy bootstrap | `install.py` runs the migrator image on the operator's host with Docker or Podman (`--network=none`), then creates Secrets with `kubectl` | (c) | An in-cluster Job with a narrowly scoped ServiceAccount; it creates missing Secrets only and never rotates an existing identity |
| C2 | Schema migration | Already a chart Job (`<release>-migrate-<revision>`), run with `--wait-for-jobs` | (a) already in the chart | Consider a `pre-upgrade` hook so that Deployments never start on an old schema without `--wait-for-jobs` |
| C3 | Offline sandbox runtime and RuntimeClass on kind | `offline_runtime.install` on the kind node | (d) platform | None: the platform provides the RuntimeClass |
| C4 | Release namespace | The installer applies `runtime/k8s/deploy/local/namespace.yaml` (restricted Pod Security) | (a) | `namespace.create=true` renders the same Pod Security labels |
| C5 | CloudNativePG | The chart default `postgres.cloudNativePG.enabled=true` needs the CloudNativePG operator and CRDs; the local installer uses `postgres.local` instead | (d) platform | External PostgreSQL with TLS required (gap 5.5): the component `uri` host and `sslmode` must become configurable |

### Image locks and preflight

| # | Check | Today | Class |
|---|---|---|---|
| P1 | Image signature verification | `install_images.py` verifies each digest's signature before it writes a lock; installation from an existing lock does not reverify | (d) until the signature admission policy (R5) |
| P2 | Host prerequisites, container engine, kind cluster ownership | `install.py` | (d) not applicable to a chart installation |
| P3 | RuntimeClass presence and unchanged tool budget | `candidate_install.require_runtime_class` and `require_unchanged_tool_budget` | (d) for the budget; the class check already runs in-cluster |
| P4 | OpenShift preflight (SCC, Kata, overhead) | `openshift_install.preflight` | (d) |
| P5 | Storage and kube-system readiness | `kubectl wait` before installation | (d) |
| P6 | Network policy verification after installation | `network_probe.verify` | (c) candidate for a `helm test` hook |
| P7 | Ownership and adoption (`ai-sdlc.io/cluster-uid`) | The installer refuses Secrets and ConfigMaps without the cluster UID annotation | (d) |

### What the next B6 changes do

1. Pin the published chart's default images to the signed digests of the
   same revision (V1), and publish the profile values (V12).
2. Render the execution and review documents in the chart (V4).
3. Add the bootstrap Job (C1, V2, V9, S3) and generate-once hooks (S2, S5).
4. Make the database host and TLS mode configurable for an external
   PostgreSQL (C5, gap 5.5), and document role provisioning.
5. Install the published chart on kind and on OpenShift with Kata by following
   this page, without `install.sh` (the Beta "installable" criterion).

## Publication

The [`chart`](../../.github/workflows/chart.yml) workflow has four jobs:

| Job | Runs on | Permissions | Does |
|---|---|---|---|
| `package` | pull requests and `main` pushes that touch the chart, its docs, tests or tool pins | `contents: read` | Packages the chart reproducibly with `tools/chart_release.py` and uploads the archive and its record (`chart-release.json`: version, tag, revision, SHA-256, size). Runs only the checkout, the checksum-verified client tools and the standard-library packager; installs no Python package. |
| `test` | after `package` | `contents: read` | Downloads the archive, runs `helm lint --strict`, checks that it renders like the source chart, and checks the Secret contract. Its only third-party package is PyYAML from hash-pinned wheels (`--require-hashes --only-binary :all: --no-deps`). Publication needs it to pass. |
| `publish` | `main` pushes only | `contents: read`; the Quay robot secret | Checks anonymously that the public repository exists, pushes the archive with the verified Helm client, and records the manifest digest Helm reports. Holds no OIDC token. |
| `sign` | `main` pushes only, after a publication | `contents: read`, `id-token: write` | Pulls the manifest anonymously by tag, checks that it has the recorded digest and a single chart layer equal to the packaged archive, signs the digest keyless with the checksum-pinned cosign, and verifies the signature as an adopter would. Runs no third-party action and no repository code. |

This mirrors the `containers` workflow's image signing. As with the images,
the digest the `sign` job signs comes from the same workflow run: the same
trust domain as the build, not an independent measurement. The anonymous
re-read only proves that what is public is what was built.

- **Registry:** `oci://quay.io/os-santiago`, so the chart is the repository
  `quay.io/os-santiago/ai-sdlc-k8s-runtime`, next to the images in
  `quay.io/os-santiago/ai-sdlc`. Helm names the repository after the chart.
- **Version:** the committed `Chart.yaml` version plus the first 12 digits of
  the revision as SemVer build metadata, for example `0.4.0+0e3b5262a30b`.
  OCI tags cannot contain `+`, so the tag is `0.4.0_0e3b5262a30b`; Helm
  converts between them. The same revision always yields the same version.
  Bumping `Chart.yaml` remains the reviewed signal of a values or template
  change.
- **What changes in rendered output:** only the `helm.sh/chart` label
  (`ai-sdlc-k8s-runtime-0.4.0_0e3b5262a30b`) and, because it hashes the
  labelled ConfigMap, the Deployments' `checksum/config` annotation. The
  installer keeps rendering the source chart, so existing installations
  render unchanged.
- **Reproducible bytes:** after `helm package`, the archive is rewritten with
  sorted members, the commit time as every file's modification time, root
  ownership, fixed modes and a gzip header without a timestamp. Re-running
  the same commit yields the same archive SHA-256 (a test packages, touches
  every file, packages again and compares).
- **The signed bytes and the test dependencies are kept apart:** the job that
  hashes and uploads the archive installs no Python package, and the test job
  installs only hash-pinned wheels. Hash pinning, not job order alone, is
  what matters: any job in the run can use the run's artifact token, so the
  test job must not run unreviewed code either.
- **Tags are never signed;** the signature is bound to the manifest digest.
  If a run fails after the push and before signing, the tag stays in the
  repository unsigned. `cosign verify` refuses it (no matching signature),
  so adopters who follow the steps below fail closed. A re-run of the same
  commit pushes the same archive and signs it.

### Owner action

Creating a repository in the `os-santiago` Quay organization is an owner
action. Until it is done, the `publish` job ends with the notice *Chart
publication skipped* and the `sign` job does not run; nothing fails. To
enable publication:

1. Create the repository `quay.io/os-santiago/ai-sdlc-k8s-runtime` and make
   it **public**: the `publish` job refuses a private repository, because
   adopters could not pull from it.
2. Give the robot account whose credentials are in the `QUAY_ROBOT_USERNAME`
   and `QUAY_ROBOT_TOKEN` repository secrets **write** permission on it (the
   same robot that pushes the images and their signatures).

The next push to `main` that touches the chart publishes and signs it.

## Verify and install the published chart

These commands were checked with the repository's pinned Helm 4.3.2 and
cosign 3.0.6 (see Verification evidence). Choose a published version, for example
from the repository's tag list, and the full 40-digit revision it was built
from.

```bash
CHART=oci://quay.io/os-santiago/ai-sdlc-k8s-runtime
VERSION=0.4.0+0e3b5262a30b                       # chart version + 12-digit revision
REVISION=<full 40-digit commit SHA starting with 0e3b5262a30b>

# 1. Download the archive; Helm prints the manifest digest it pulled.
helm pull "$CHART" --version "$VERSION" --destination ./chart
#   Digest: sha256:<digest>

# 2. Verify that exact digest was signed by the chart workflow on main, at that revision.
cosign verify quay.io/os-santiago/ai-sdlc-k8s-runtime@sha256:<digest> \
  --certificate-identity https://github.com/os-santiago/ai-sdlc/.github/workflows/chart.yml@refs/heads/main \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-github-workflow-trigger push \
  --certificate-github-workflow-sha "$REVISION"

# 3. Install the archive you verified, never the tag again.
helm install ai-sdlc ./chart/ai-sdlc-k8s-runtime-"$VERSION".tgz \
  --namespace ai-sdlc-runtime -f my-values.yaml
```

If `cosign verify` reports no matching signatures, do not install: the
version was pushed but never signed (for example, a run that failed after
the push).

Use the exact `--certificate-identity`; do not replace it with
`--certificate-identity-regexp`. Helm checks the downloaded archive against
the layer digest in the manifest, so verifying the manifest digest covers
the archive you install.

Before step 3, create every Secret the
[Secret contract](helm-chart-secret-contract.md) requires for your profile,
and supply the values in the gap analysis above. Until the bootstrap Job
exists (C1), the identity Secrets and `authority.policyJSON` must come from
one run of the installer's identity step or from a backup.

## Verification evidence

Observed on 2026-11-24 against a local OCI registry (`registry:2`), with the
pinned Helm 4.3.2 and cosign 3.0.6:

- `helm push` printed a single `Digest:` line; the manifest had the Helm
  config media type and one chart layer whose digest equals the SHA-256 of
  the packaged archive.
- `helm pull` by version and by digest returned that manifest.
- cosign signed and verified the chart manifest digest with a local key
  (keyless signing needs the GitHub OIDC token, so it was not exercised).

Not yet observed: a publication to Quay, keyless signing in the workflow,
and the anonymous re-read against quay.io for this repository. The first
real run happens on `main` after the owner action.
