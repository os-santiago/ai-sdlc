# Helm chart Secret contract

The `ai-sdlc-k8s-runtime` chart never creates a Secret and never receives a
secret value through Helm values. Every workload reads its private material
from a Secret that exists in the release namespace before the chart's Pods
start. This page lists each of those Secrets, the keys the chart and the
runtime read, and who creates them today. It is the contract a cluster
administrator follows to install the chart without `install.sh`, and the
contract the installer already satisfies.

`tools/test_helm_chart_secret_contract.py` renders the chart in every
capability combination the tests cover and fails when a rendered
`secretKeyRef`, Secret volume or Secret RBAC rule is missing from the tables
below, or when a table row is no longer used. The same test scans the
template source, so a new Secret reference cannot be added in a branch the
renderings do not reach without updating this page.

Observed against the chart at `0.4.0` on 2026-11-25.

## Lifecycle terms

- **Supplied:** the administrator (or an external secret operator such as
  External Secrets) provides the value. AI-SDLC does not generate it.
- **Generated once and adopted:** a random value created at the first
  installation and then reused unchanged on every later installation and
  upgrade. Rotating it is a reviewed operation, not a reinstall. Today the
  installer generates it; a chart administrator may generate it with the
  commands below instead.

Secrets the installer creates are `type: Opaque`, labelled
`app.kubernetes.io/part-of: ai-sdlc`, and annotated
`ai-sdlc.io/cluster-uid: <UID of the kube-system namespace>`. The installer
refuses to adopt a Secret without that annotation; a chart-only installation
does not need it, but should set it if the installer may manage the same
namespace later.

## Secrets mounted or referenced by workloads

`{role}` is one of the component identities: `intake`,
`admission-controller`, `orchestrator`, `worker`, `verifier`,
`release-manager`, `relay`, `deployer`, `build-controller`. A role's Secrets are
needed only when its workload renders: the six core Deployments always, the
`verifier` with `runtime.mode=verification`, the `build-controller` with
`build.enabled`, and the `deployer` with `release.enabled`.

<!-- secret-contract:begin -->
| Secret (default name; value that renames it) | Keys | Required when | Consumed by | Created by | Lifecycle |
|---|---|---|---|---|---|
| `ai-sdlc-postgres-app` (`postgres.databaseURLSecret.name`; `<postgres.cloudNativePG.clusterName>-app` when CloudNativePG is enabled) | `uri`, `password` | always | migrator Job (`uri`: the bootstrap owner's `postgresql://` URL; `postgres.databaseURLSecret.key` renames it); backup CronJob (`uri`, only with `backup.enabled`); bundled PostgreSQL StatefulSet (`password`, only with `postgres.local.enabled`) | installer (local PostgreSQL); the CloudNativePG operator (CloudNativePG) | generated once and adopted; supplied for an external database |
| `ai-sdlc-database-roles` (`migrator.credentialsSecret`) | `passwords.json` | `migrator.enabled` | migrator Job, which creates or updates one login role per component | installer | generated once and adopted; a new role is appended, existing passwords never change |
| `ai-sdlc-database-{role}` (`authority.databaseSecretPrefix`) | `uri`; installer also keeps `password` | the role's workload renders | that component's `DATABASE_URL` | installer | generated once and adopted |
| `ai-sdlc-identity-{role}` (`authority.identitySecretPrefix`) | `identity.json`, `public-key.json` | the role's workload renders | that component (`/etc/ai-sdlc/identity/identity.json`) | installer, from the pinned migrator image's `bootstrap-identities` | generated once and adopted; never rotated by reinstalling |
| `ai-sdlc-github-client` (`intake.credentialSecret`) | `credential.json` | `runtime.mode` is `shadow`, `candidate` or `verification` | intake | installer, imported from an authenticated `gh` CLI | supplied |
| `ai-sdlc-publication-client` | `credential.json` | `publication.enabled` | release-manager | installer, imported from an authenticated `gh` CLI | supplied |
| `ai-sdlc-codex-auth` | `auth.json` | `runtime.mode` is `candidate` or `verification` with `modelGateway.adapter=agent-cli` | worker and verifier model gateways | installer, from `--codex-auth-file` | supplied |
| `ai-sdlc-model-client` | `token` | `runtime.mode` is `candidate` or `verification` | worker, and its model gateway | installer | generated once and adopted |
| `ai-sdlc-review-model-client` | `token` | `runtime.mode=verification` | verifier, and its model gateway | installer | generated once and adopted |
| `{modelGateway.upstream.credentialSecret}` (no default) | `credential` | `modelGateway.upstream.credentialSecret` is set with an upstream adapter | worker and verifier model gateways | administrator (the model-upstream upgrade; the installer refuses it at installation) | supplied |
| `ai-sdlc-source-reader` | `credential.json` | `verifier.sourceCredential` is `true` (with `runtime.mode=verification`) | the verifier controller container only (`/var/run/ai-sdlc/source/credential.json`), for the source reads of verification and independent review; never the model gateway sidecar or any pod the verifier creates | administrator (or an external secret operator); the installer only references an existing one | supplied |
| `ai-sdlc-provenance-registry` (`provenance.registryCredentialSecret`, which must equal `<release fullname>-provenance-registry`) | `credential.json` | `provenance.enabled` | provenance verifier | installer (`provenance_runtime_install.py`) | supplied |
| `ai-sdlc-flow-console-database` (`console.databaseSecret`) | `uri`; installer also keeps `password` | `console.enabled` | the read-only flow console (`DATABASE_URL`: the `aisdlc_flow_console` role's `postgresql://` URL; see [flow console](flow-console.md)) | installer with `--flow-console` (`password` kept for its consistency checks), or the administrator | generated once and adopted; removed when a later install omits the option; its password is also the optional `flow-console` entry of `passwords.json` |
<!-- secret-contract:end -->

`modelProvider.apiKeySecret` is accepted by the values schema but no template
mounts it. The chart only refuses to reuse its name for the provenance
registry credential. It is not part of the contract.

## Secrets read through the Kubernetes API

These are read by name through narrowly scoped RBAC rules, never mounted.
They belong to the reviewed Saferus build and release fixtures and live
outside the release namespace.

<!-- secret-api-contract:begin -->
| Secret | Namespace | Required when | Read by | Created by |
|---|---|---|---|---|
| `registry-auth` | `ai-sdlc-builds` | `build.enabled` | build-controller | build foundation installer |
| `ai-sdlc-release-reader` | `ai-sdlc-production` | `release.enabled` | deployer | release target preparation |
| `saferus-console-tls` | `ai-sdlc-production` | `release.enabled` | deployer | release target preparation |
<!-- secret-api-contract:end -->

## Key formats

- `uri` (component): `postgresql://aisdlc_<role with - as _>:<password>@ai-sdlc-postgres:5432/ai_sdlc?sslmode=disable`.
  The installer checks that `uri` embeds exactly `password`. The host and
  `sslmode=disable` are fixed today; an external database with TLS needs gap
  5.5 (see [Helm chart installation](helm-chart-install.md)).
- `password` (component): 32 to 128 characters of `[A-Za-z0-9_-]`, distinct for
  every role.
- `passwords.json`: a JSON object mapping each role name to the same password
  as its `ai-sdlc-database-{role}` Secret. It may also hold an optional
  `flow-console` entry: the migrator then provisions the read-only
  `aisdlc_flow_console` role with that password (the same as in
  `ai-sdlc-flow-console-database`), and disables that role when the entry is
  removed. It is not a component role and has no identity Secret.
- `identity.json`: `{"keyId", "role", "privateKey"}`; `public-key.json`:
  `{"role", "value"}`. The public key must also appear under `keyId` in the
  authority policy (`authority.policyJSON`), so both are produced together
  by `bootstrap-identities` and checked by `validate-bootstrap`.
- `credential.json` (GitHub): `{"version", "mode", "host", "repository",
  "actorID", "actorLogin", "token"}`. The intake configuration's
  `authorizedActorIDs` is derived from `actorID`.
- `token` (model clients): 32 to 128 characters of `[A-Za-z0-9_-]`.
- `credential.json` (source reader): `{"version": 1, "host": "github.com",
  "repository": "<owner>/<name>", "access": "read", "token": "<token>"}`, at
  most 8 KiB, no other field. Keys are matched exactly (case-sensitive) by
  both the installer and the verifier. `repository` must equal the policy's repository
  exactly; `token` is 20 to 4096 characters of `[A-Za-z0-9_.-]`. Use a token
  that can only read that one repository, for example a fine-grained personal
  access token restricted to it with only the *Contents: read* permission (and
  the implied *Metadata: read*). Never reuse `ai-sdlc-github-client` or
  `ai-sdlc-publication-client`: they hold a write-capable account token. See
  [Source availability](source-availability.md#authenticated-verification-and-review-reads).
- `auth.json`: the agent CLI's JSON authentication document, at most 16 KiB.

## Generating once without the installer

For a role's database credential, for example, with `openssl` available:

```bash
password="$(openssl rand -base64 48 | tr '+/' '-_' | tr -d '=\n')"
kubectl -n ai-sdlc-runtime create secret generic ai-sdlc-database-intake \
  --from-literal=password="$password" \
  --from-literal=uri="postgresql://aisdlc_intake:${password}@ai-sdlc-postgres:5432/ai_sdlc?sslmode=disable"
```

Keep `ai-sdlc-database-roles` consistent with every role's password. Identity
Secrets cannot be produced this way: they must come from the pinned migrator
image together with the authority policy. Until the chart has a bootstrap
Job for them (gap analysis item C1), a chart-only installation runs the
installer's identity step once, or restores Secrets and policy from a backup.

Never put these values in Git, Helm values, `--set` arguments or CI logs.
