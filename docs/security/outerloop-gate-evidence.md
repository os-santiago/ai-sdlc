# Outerloop gate evidence before activation

The release-manager's legacy pure evaluator is a diagnostic building block.
Its unsigned HTTP handler is **not registered** by the installed component.
A `Snapshot` or `approved` result is not a signed capability, merge permission
or proof that evidence was collected from GitHub.

Before evaluating a prospective PR, the trusted caller must supply its current
40-character head SHA and collect bounded, current check evidence. Required
names from the reviewed contract are a floor; caller-supplied names may add
requirements. Missing names cannot disappear merely because the API omitted
failed or pending runs. The portable contract defines an empty required list
as **all reported checks**, so `required: false` does not exempt a failed check
in that mode. An explicitly required observed check also remains required.

Every evaluated check must identify the same head, be completed and report
success. Neutral, skipped, stale, unknown, running and mismatched-head evidence
cannot approve. Duplicate check names or required names are ambiguous and
escalate rather than allowing a successful duplicate to hide a failure. A future
collector must resolve run ordering and app identity authoritatively before
constructing this normalized input; the evaluator does not guess which run won.

Negative feedback/remediation counts and oversized evidence sets are invalid.
Existing review-feedback, issue-coverage, validation and bounded remediation
requirements still apply. Their legacy scalar fields do not establish
provenance: signed independent verification and authoritative review collection
remain prerequisites for operational publication/merge.

After merge, a successful forge release now stays `waiting`. It cannot produce
`production-verified`: that requires current authenticated deployment identity,
the promoted artifact digest, readiness/user-outcome evidence and rollback
policy. This change intentionally adds no scalar override for those guarantees.
Failed release evidence still escalates.

Regression tests cover missing/optional-only checks, required-name omission,
head drift, running checks carrying old success, duplicate identities,
skipped/neutral/unknown outcomes, invalid counters, contract-floor preservation
and release-only false success. Build/test results for this evaluator are not
an issue-to-production E2E result. Track operational collection in #437 and
production verification in #439.
