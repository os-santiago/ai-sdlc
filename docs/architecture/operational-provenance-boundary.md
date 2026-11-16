# Operational provenance boundary

**Context retention, the inactive registry-verification library and a bounded
verification journal, signed handoff and explicitly gated component wiring are
implemented; installation remains work for
[#574](https://github.com/os-santiago/ai-sdlc/issues/574).**
This does not activate the installed build controller or authorize promotion.
The current [readiness report](../operations/saferus-e2e-readiness.md)
separates tested foundation preparation from the unproven autonomous E2E.

## Authenticated historical boundary

[`ReadBuiltRecord`](../../runtime/k8s/internal/store/built_records.go) requires the
verifier database identity and validates historical signed command, plan, slot,
ordered original observations, immutable compilation context and result. The
context exposes the complete configuration preimage, selected slot, compiler
profile and exact request bytes through that authenticated reader.

[`BeginBuildEffect`](../../runtime/k8s/internal/store/build_effects.go) requires
that context and reconstructs its closed requests under the original command/slot
lock before atomically recording context, plan and effect intent. Migration 0024
provides immutable, bounded storage; the build component may insert context only
before any existing plan/effect, and the verifier has read-only access. Historical
readback also requires retention to precede the original plan/create intent.

[`Compile`](../../runtime/k8s/internal/buildworkload/compile.go) creates both
requests, each bounded to 32 KiB. Its definition digest commits to an acyclic
template containing the two requests with the literal `DEFINITION` placeholder,
profile digest and slot digest. The final requests then contain that definition
digest. The retained exact bytes now permit reconstruction after cleanup; a
digest alone could not.

The [isolated verifier](closed-build-provenance.md) can reconstruct it because
its fixture retains profile, slot and request files. Operational verification
must not import that fixture directory or derive expected identities from the
attestation being evaluated. Cleanup must remain able to delete original Tekton
objects; retention belongs in the authenticated journal, not in live objects.

## Implemented first slice: immutable compilation context

The pre-effect transaction retains a versioned, bounded context
with the exact compiler profile, complete public configuration binding, selected
slot, both compiled request byte sequences and the approved compiler/recipe
identity. It is persisted with the plan before any external create. Validation binds it to the authenticated command,
plan, allocated slot and effect request digests. Do not add an arbitrary manifest
input or a second execution path.

There are two distinct profile digests to reconstruct. The template uses
`Profile.Digest()`. The signed command instead binds
[`Configuration.ProfileDigest()`](../../runtime/k8s/internal/building/configuration.go),
whose preimage includes version `ai-sdlc.build-profile/v1`, `RecipeProfile`
(the compiler profile digest), `IssueMaxAgeSeconds` and the complete ordered
`Slots` list. Retain that exact public preimage, validate every slot and prove
that the selected slot is an exact member. Bind the separately signed
`IntakeConfigSHA256` as well; a selected slot alone cannot reconstruct the signed
configuration binding. Preserve the archived signed policies already held by the
journal for historical validation without treating them as current authority.

The retained context must contain only public compilation facts. Registry
credentials, private keys and kubeconfig material remain outside it. Validate
strict decoding, field/byte limits and the closed request shape before storage;
unknown versions or extra authority-bearing fields are rejected. Preserve exact
request bytes for hashing rather than reserializing them in another language.

Expose the context through the verifier's authenticated historical reader, with
read-only grants and immutable storage. Verification reconstructs the known
acyclic template and checks every plan/definition/request digest. The separate
`ValidateHistorical` method reconstructs the closed definition without a current
clock or an executable return value: replaying `Compile` with the current clock
would reject an
expired command, while changing its clock to obtain execution authority would
cross the historical/current-authority boundary. Historical validation must
never issue a command, reserve a slot or call a transport.

Old records without complete retained context are explicitly unverifiable.
The migration performs no backfill. Keep build activation and enabled-runtime
upgrades behind their existing installation gates; this migration is not an
approval to switch a live controller or downgrade it to a context-free writer.
Do not backfill them from current cluster state, infer missing bytes from a
mutable profile, or rebuild another artifact and label it the original result.

## Implemented second slice: inactive cryptographic verifier

[`provenance`](../../runtime/k8s/internal/provenance/) verifies a structurally valid
historical record using independently supplied public trust and a separately
provisioned registry identity. Only the role-checked database reader establishes
record authenticity; a caller-created `buildrecord.Record` does not. The explicitly enabled dedicated provenance process calls this library; the
installed runtime remains inactive.

Trust explicitly restricts this version to Saferus in `ai-sdlc-production` in kind.
It pins cluster/namespace, publisher/compiler/verifier identities, registry CA,
ECDSA P-256 public key fingerprint, format and a validity window of at most 24
hours. The pinned Sigstore 1.11.3 library requires Go 1.27; all eleven component
build stages and local validation use the same immutable Go 1.27.4 image.

A single-use verifier permits five GET requests within one 60-second deadline:
image manifest by immutable digest, Cosign signature manifest and blob, and DSSE
attestation manifest and blob. It uses TLS 1.3, its approved CA, a fixed registry
host/repository, bounded response sizes, and no proxy, redirect, compression or
implicit retry. Legacy `.sig`/`.att` tags only locate evidence: their exact
manifest/blob digests are retained, and signed claims must bind the expected
image digest. Multiple evidence layers or signatures are rejected as ambiguous.

The proof binds the complete record, context and trust digests, image and evidence
digests, verifier identity and original build/publish observation digests. The
signed publisher Run UID is checked; original Pod/Task UIDs remain authenticated
journal observations, not claims directly signed by Chains. The verifier checks
the image manifest, but does not download every application config/layer blob or
certify their availability for deployment. Deployment must verify those needs.

This version explicitly uses a local key without transparency-log verification.
It claims no SLSA level and always returns `releaseAuthorized: false`. Tests use
an isolated TLS registry and actual Sigstore signing/verification, plus an
unmodified archived Chains signature from a removed real workload. That archive
is partial format evidence, never operational history or approved trust.

The username requirement does not establish registry permissions. Installation
must provision a native pull-only ACL and prove denied push/delete/other-repository
access before activation. The existing kind registry does not yet satisfy this
requirement. The journal below provides current-policy and durable-completion fences; it
is not yet wired to a runtime component.

## Implemented third slice: separately authorized verification journal

[`provenance.Command`](../../runtime/k8s/internal/provenance/command.go) binds one
existing authenticated build record, the installed trust revision, both current
loop policies, intake configuration and original compilation configuration. A
fresh `verify-built` signature must come from the build-controller identity and
target the verifier. Its lifetime is at most two minutes and cannot outlast
approved trust. Expired build authority remains historical; it does not authorize
this new attempt or prevent a separately authorized verification of valid history.

[`ProvenanceJournal`](../../runtime/k8s/internal/store/provenance_journal.go)
rechecks signed authority, original history and current issue eligibility at
queue, exclusive claim, input read and completion. Configuration maps/slices are
copied. Policy/trust, clock and cancellation are reobserved after potentially
blocking SQL reads and after COMMIT before returning a claim or input. Completion
rechecks after inserting the proof and after the final state update. Unknown,
expired, frozen, revoked or changed authority prevents a successful result.
These checks do not create an atomic transaction across mounted configuration,
PostgreSQL and the registry; later promotion must obtain its own current authority.

Migration 0025 permits one command per built record. A consumed claim is never
reissued after expiry, restart or an uncertain COMMIT response. The immutable
proof and terminal `verified` state commit together; a lost response can be
resolved by the role-checked historical reader. Historical readback validates
archived signed authority and the complete proof binding at its original
completion time. It grants no current promotion permission.

`ProvisionRolesForProvenance` requires its own explicit capability in addition to
build. Existing build provisioning and maintenance do not grant access to this
journal. The verifier can append its proof, but cannot modify the stored command,
configuration, expiry or accepted proof; other components cannot write it. These
methods remain inactive in the installation. The handoff library and dedicated
process below use them only with an explicit capability; no installer option
enables them yet.

## Implemented fourth slice: signed handoff and bounded coordination

[`provenanceflow`](../../runtime/k8s/internal/provenanceflow/) selects eligible
completed builds without accepting a caller-selected build ID or artifact.
Dispatch requires immutable cleanup evidence bound to both original Run UIDs,
resource names, slot and completion timestamps. Missing, substituted or unknown
cleanup prevents dispatch; physical storage remains consumed.

Migration 0026 records one handoff per build, atomically with the build-controller's
signed `verify-built` outbox message. The verifier inbox and queue also commit
together. Redelivery uses a new relay transport nonce with the same signed command;
reusing a transport nonce is rejected, while semantic duplicate delivery has no
second queue effect. Current policy is reloaded after potentially blocking reads
and writes before returning from the dispatch transaction.

The runtime coordinator expires old requests before loading current
policy, obtains an exclusive claim, reads authenticated input, constructs the
verifier, and revalidates input after that potentially blocking construction.
It bounds registry work by the claim deadline. Verification or completion failure
retires the attempt; a lost completion response is resolved from immutable history
without repeating network verification. All results remain non-authorizing for
release. The dedicated process below keeps this bounded read-only queue from delaying
containment in the inner verifier process.

Real PostgreSQL tests exercise signed outbox/inbox delivery, idempotency, rollback,
cleanup substitutions and revocation before/during the verifier call. Those tests
explicitly use synthetic cleanup/proofs and a bounded fake verifier to isolate
coordination; they do not demonstrate installed Kubernetes cleanup or an E2E.
The cryptographic library's separate tests cover actual TLS and signature handling.

## Remaining integration before activation

A new bounded verification attempt consumes the authenticated historical record
and independently installed current trust. Its identity binds the build record,
retained context and trust-policy revision. A fresh exclusive claim, expiry,
freeze and revocation checks authorize this attempt; the historical build receipt
does not. A policy change before completion prevents a successful current result.

Resolve the exact result `ImageURL@ImageDigest` through the approved registry
using a pull-only identity. Restrict host/redirect behavior, response bytes,
request count and total duration. Retained OCI signatures and DSSE evidence must
remain available after original PipelineRuns and TaskRuns are removed. Missing,
ambiguous or malformed evidence produces a bounded failure, never a fallback to
another image, a mutable tag or local fixture output.

Use the pinned signature verifier and separately approved signing trust. Bind the
cryptographically verified subject, source/tree, definition, publisher/materials,
steps and original publisher Run UID to historical expectations. Record exact
artifact/evidence digests, verifier/policy identities and bounded outcome codes;
never raw credentials. A public key discovered beside an attestation or in a
cluster is an observation, not automatic approval of that key.

The existing fixture uses local key custody and omits transparency-log checks.
An operational trust policy must explicitly define its signature and transparency
requirements and fail when required evidence is missing. This proposal does not
select a production trust policy or claim an SLSA level.

## Acceptance matrix before activation

| Boundary | Required real validation |
|---|---|
| Pre-effect retention | PostgreSQL rollback/failure injection proves that incomplete context never permits a create; a lost response does not replace context or duplicate execution. |
| Historical reader | Wrong role, changed bytes/digests, mismatched source/tree/slot/profile, altered unselected slot, slot ordering or freshness budget, unknown version and missing legacy context all fail; valid history remains readable after build authority expires. |
| Immutability | Component roles cannot update/delete accepted context or manufacture another component's record; repeated reads after restart preserve exact identity. |
| Registry and signatures | Real retained image/DSSE succeeds after Tekton cleanup; wrong key, subject, source/tree/definition/run, missing/truncated/ambiguous evidence and forbidden redirects fail. |
| Current authority | Real PostgreSQL tests cover exclusive claim, duplicate delivery, expiry, freeze, revocation and policy change while verification is running. |
| Completion recovery | A lost result response and process restart yield the same durable outcome, never another promotion authority. |
| Separation | Verifier, observer and generative identities cannot publish, promote, deploy or write another component's journal. |

Run isolated failure injection first. Enabling this verifier still does not enable
promotion. [#439](https://github.com/os-santiago/ai-sdlc/issues/439) must separately
consume current-policy-authorized verification and implement digest-preserving
deployment, rollback and acceptance. [#441](https://github.com/os-santiago/ai-sdlc/issues/441)
must preserve containment across installation changes. Only then may the actual
Saferus issue enter the installed chain for [#440](https://github.com/os-santiago/ai-sdlc/issues/440).


## Implemented fifth slice: dedicated component process

`AI_SDLC_PROVENANCE=true` requires the complete existing verification, independent
review, publication, merge and build capability chain. Unknown options fail
closed. The migrator grants provenance privileges only for that explicit option.
Maintenance and build-only installations retain their previous privileges.

The build-controller runs handoff **inside its existing exclusive lease**, after
original workload containment and before selecting a new build. A separate SQL
transaction lock serializes provenance dispatch. A retained handoff reserves the
single local verification lane for 125 seconds from its database insertion: the
command lifetime is at most 120 seconds with at most five seconds of future clock
skew. This conservative backpressure covers the original command's entire
lifetime, even if relay delivery is marked published or exhausted. It does not
renew a command, retry a consumed verification or reopen a completed build.
The next distinct eligible build may enter after that window. This limits the
local test profile to at most one provenance dispatch per 125 seconds.

Run the existing verifier image with the single argument `provenance` in a
**dedicated process and Service**. Its only work route is authenticated
`POST /internal/verify-built`; `RuntimeBoundary` and the relay's exact seven-route
contract require the capability. The normal verifier process continues inner
behavior/review work and does not register this route. No new container image is
required. The installer must route `verify-built` to `provenance-verifier`, not
to the inner verifier Service.

This separation matters: model reviews can last several minutes, while a fresh
provenance command lasts only two. A shared serial queue or readiness dependent
on the model gateway would consume valid requests while waiting for unrelated
work. Dedicated readiness checks the database role/migrations, canonical loop
policies, public trust and pull-only credential, without loading model or
candidate-execution configuration. The periodic coordinator bounds each poll to
75 seconds and retires expired requests even when current trust or policy cannot
be loaded.

Both processes currently share the `verifier` signing identity and database role.
This separates scheduling and mounted credentials, **not** database privileges or
cryptographic authority between the two processes. Mount registry custody only
into the dedicated process. `AI_SDLC_PROVENANCE_VERIFIER_IMAGE` is the independently
installed image pin and must match public trust; the installer must also verify
the actual Deployment uses that digest. The loader checks cluster, namespace and
publisher bindings against the installed build profile and rereads canonical
policy documents on each call. It never derives approved trust from evidence.

The dedicated loader also fences uncertain or expired inner custody at inbox,
claim/input and completion. It rejects failed cleanup, explicit uncertainty and
expired claimed review/behavior work; database errors reject as well. A live
inner claim is allowed to run independently. Inner reconcilers keep their
stronger unchanged exclusion checks before creating Kubernetes resources.

The code is not an installation: reviewed Helm manifests, independent trust and
native registry ACL/credentials, network restrictions and actual installed
component validation still precede activation. Model outage and remote registry
or relay failure cannot create a release permission; a failed or expired attempt
remains consumed under the journal's current policy.
## Native registry access conformance

The [registry ACL blueprint](../../runtime/k8s/deploy/konflux/provenance-registry-access.json)
gives `ai-sdlc-verifier` only read access to `ai-sdlc-fixtures/saferus`. The publisher
can read/create/update that repository, with no delete or cross-repository rights.
Anonymous and unmatched users receive no default access. This blueprint is not
installed automatically and is not evidence that the live registry has this ACL.

On Linux with Podman, run:

```bash
python3 tools/validate_provenance_registry.py --output /tmp/provenance-registry-evidence
```

The output directory must not exist. The tool uses the pinned Go toolchain and
native Zot image, creates a private TLS registry bound only to a random loopback
port, generates fixture-only passwords/signatures, and publishes the actual image,
Cosign signature and DSSE attestation with their referenced blobs. Python uses only
its standard library; cryptographic fixture generation uses the locked Go modules.
The registry runs without root, capabilities or a writable root filesystem. Its
bounded ephemeral storage and test administrator exist only inside the disposable
fixture; the administrator is absent from the installation blueprint.

The test verifies TLS 1.3 and positive read → denied upload/manifest write/delete/
cross-repository read → positive read. It also proves publisher cross-repository
write/delete and anonymous read are denied. The other repository is populated,
so a missing artifact cannot masquerade as access control. Finally the actual Go
verifier authenticates the retained fixture using five registry GETs; only its
socket dial destination is mapped to the owned loopback port. The fixed registry
hostname, TLS server identity, scope and cryptographic checks remain intact.

`report.json` records source revision/dirty state, image digests, exact status
codes, required Go test completion and owned-container cleanup. A public proof is
retained; generated credentials, private TLS key and synthetic history are
removed. Cleanup deletes only inspected container IDs bearing this run's ownership
label and verifies absence with a successful container listing.

This validates native registry protocol and ACL behavior with synthetic retained
history. It neither authenticates operational build history nor installs approved
trust, activates controllers, authorizes release or proves the issue-driven E2E.
Installation must still provision the separate identity, apply the reviewed ACL,
revalidate foundation trust and run equivalent installed-access checks.

## Registry configuration recovery validation

The existing local Konflux registry uses an `emptyDir`. Replacing its Pod can
lose retained artifacts; a Deployment rollout is not a safe ACL upgrade strategy.
A native probe of the pinned Zot image found that `SIGHUP` terminates its process.
Do not infer hot-reload support from documentation for a different Zot version.

The disposable native recovery exercise is:

```bash
python3 tools/validate_provenance_registry_recovery.py --output /tmp/provenance-registry-recovery
```

It starts without repository ACLs and without the verifier identity, using an
existing `konflux` publisher credential. It seeds actual image/signature/DSSE
objects, atomically replaces the projected configuration, restarts the same
owned container with retained fixture storage, and proves the restrictive ACL
before adding the reader credential. A second restart exercises the new
credential. Both restarts preserve the publisher credential and endpoint. The
full denial matrix and actual five-GET cryptographic verifier run afterward.
The two-phase order prevents a dynamically reloaded htpasswd from introducing a
reader with broad access before its ACL is enforced.

This fixture uses a private directory for retained storage; it is removed with
all generated custody at completion. Unlike the default validator's bounded
tmpfs, its filesystem has no dedicated quota. It holds only the small synthetic
payload. The report explicitly does not claim Kubernetes projected-volume
convergence, same-Pod restart validation, Pod replacement recovery, live
installation, durability across node loss or operational E2E. An installed
upgrade must separately prove original Pod/container/volume custody and retained
artifact access, and fail closed if those identities change.

## Declarative deployment boundary

The Helm chart defines the dedicated process through `provenance.enabled`
(default `false`), `trustJSON`, a fixed dedicated registry Secret name and an
opaque credential revision. It requires the entire preceding capability chain,
a pinned verifier image and matching public trust/build profile. Runtime parsing
still validates actual CA, signing key, policy format and expiry; Helm rendering
alone does not approve those values.

The dedicated ServiceAccount has no token or role binding. Only this process
mounts the pull-only registry credential, selecting the `credential.json` key;
the builder mounts public trust only. Its Secret must be named
`<release-fullname>-provenance-registry` and cannot alias configured identity,
database, migrator, intake or model custody. The installation procedure must
verify its exact key set, identity, revision and native registry permissions.

An always-present policy admits only relay ingress on port 8089 and limits egress
to DNS, the configured database and the Zot Pods in `kind-registry` on target port
5000. The shared runtime policy explicitly excludes this Pod, so additive
NetworkPolicy semantics cannot restore its broader internal access. Database
selectors cover local PostgreSQL and the configured CloudNativePG cluster;
explicit database CIDRs retain their existing port-5432 restriction.

The chart is a reviewed declaration, not the completed installer workflow.
`tools/install.py` still needs provenance foundation preparation, independent
trust approval and ACL/custody preflight before it can safely expose this option.
Rendering and Kubernetes server dry-run validate object shape and admission;
they do not demonstrate effective network enforcement or a running verifier.
