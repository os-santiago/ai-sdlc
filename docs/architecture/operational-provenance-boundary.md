# Operational provenance boundary

**Proposed implementation contract for [#574](https://github.com/os-santiago/ai-sdlc/issues/574),
not an installed capability.** The current [readiness report](../operations/saferus-e2e-readiness.md)
separates tested foundation preparation from the unproven autonomous E2E.

## Existing boundary and missing evidence

[`ReadBuiltRecord`](../../runtime/k8s/internal/store/built_records.go) requires the
verifier database identity and validates historical signed command, plan, slot,
ordered original observations and immutable result. It returns command, plan,
terminations, result and recording time. Slot bytes are checked internally but
not returned. Complete compiler profile and request bytes are not archived by
[`BeginBuildEffect`](../../runtime/k8s/internal/store/build_effects.go): that
transaction records the plan and effect intent, including their digests.

[`Compile`](../../runtime/k8s/internal/buildworkload/compile.go) creates both
requests, each bounded to 32 KiB. Its definition digest commits to an acyclic
template containing the two requests with the literal `DEFINITION` placeholder,
profile digest and slot digest. The final requests then contain that definition
digest. A digest alone cannot reconstruct the original definition after cleanup.

The [isolated verifier](closed-build-provenance.md) can reconstruct it because
its fixture retains profile, slot and request files. Operational verification
must not import that fixture directory or derive expected identities from the
attestation being evaluated. Cleanup must remain able to delete original Tekton
objects; retention belongs in the authenticated journal, not in live objects.

## First implementation slice: immutable compilation context

Extend the existing pre-effect transaction to retain a versioned, bounded context
with the exact compiler profile, selected slot, both compiled request byte
sequences and the approved compiler/recipe identity. Persist it with the plan
before any external create. Validation must bind it to the authenticated command,
plan, allocated slot and effect request digests. Do not add an arbitrary manifest
input or a second execution path.

The retained context must contain only public compilation facts. Registry
credentials, private keys and kubeconfig material remain outside it. Validate
strict decoding, field/byte limits and the closed request shape before storage;
unknown versions or extra authority-bearing fields are rejected. Preserve exact
request bytes for hashing rather than reserializing them in another language.

Expose the context through the verifier's authenticated historical reader, with
read-only grants and immutable storage. Verification reconstructs the known
acyclic template and checks every plan/definition/request digest. Use a separate
historical validator: replaying `Compile` with the current clock would reject an
expired command, while changing its clock to obtain execution authority would
cross the historical/current-authority boundary. Historical validation must
never issue a command, reserve a slot or call a transport.

Old records without complete retained context remain explicitly unverifiable.
Do not backfill them from current cluster state, infer missing bytes from a
mutable profile, or rebuild another artifact and label it the original result.

## Second slice: independent current verification

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
| Historical reader | Wrong role, changed bytes/digests, mismatched source/tree/slot/profile, unknown version and missing legacy context all fail; valid history remains readable after build authority expires. |
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
