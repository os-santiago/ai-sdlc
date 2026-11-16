# Local Saferus promotion authority

The `releasing` package defines the first, inactive contract for promotion of a
verified Saferus console artifact. It does not deploy, publish a release, admit
an issue, or advance a workflow. The runtime still needs the durable promotion
journal, verifier handoff, release-manager queue, deployer, health proof and
bounded rollback before end-to-end delivery can be enabled.

## Installed destination

A separately reviewed `Target` binds the original kind cluster, production-test
namespace, console Deployment, Service, immutable pull credential and TLS Secret
UIDs. It binds the complete Deployment spec with only its container image
normalized, the Service spec, the initial immutable console image, and the CA
and fixed TLS hostname. The names, sole container, 300-second rollout budget and
30-second smoke budget are closed by this initial recipe. Target approval lasts
at most 24 hours and cannot outlive its CA.

These hashes describe required observations; this package does not make them.
The future deployment adapter must compare current objects, their UIDs and
canonical content immediately before an image-only conditional patch. A matching
image alone cannot establish ownership after an ambiguous Kubernetes response.

## Promotion input

`promote-artifact` travels from the verifier to the release manager. Its signed
command binds authenticated provenance and build record digests, issue revision
and generation, merged commit, image digest, installed target, trust, intake,
profile and both current policy revisions. It expires within two minutes and
cannot outlive the target or provenance trust. A separate deployment command
and journal will be required for the longer rollout; promotion expiry is not a
rollout timeout.

Completed build and verification commands may have expired. Their historical
records must validate at their recorded timestamps; fresh promotion authority
is separately bounded. Freeze, revoked keys, changed policies/trust/target or
substituted history deny the promotion. Both records must come from the store's
authenticated readers. Struct construction and validation cannot establish that
origin or current issue eligibility. The dispatch transaction must establish
both before accepting a promotion.

## Validation and limits

Contract tests cover valid expired history, fresh expiry, revoked authority,
record/source/image/issue substitution, closed payload keys, target and TLS
restrictions, and preservation of the historical provenance record encoding.
Their provenance records are explicitly synthetic journal-contract fixtures;
they do not constitute a cryptographic verification or deployment test. Real
TLS/Sigstore verification is tested separately in `internal/provenance`.

The scope is kind test production with the existing local signing policy. This
increment grants no enterprise readiness, SLSA level, autonomous deployment or
end-to-end success claim.
