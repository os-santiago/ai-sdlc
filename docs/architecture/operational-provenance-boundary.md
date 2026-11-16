# Operational provenance boundary

**Context retention and an inactive registry-verification library are implemented;
the authorized operational verification journal remains work for
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
record authenticity; a caller-created `buildrecord.Record` does not. No runtime
component calls this library yet.

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
requirement. Current policy, freeze, revocation and durable completion still
belong to the next journal increment.

## Next slice: independent current verification

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
