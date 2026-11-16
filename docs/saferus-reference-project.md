# Saferus reference project

Saferus is the Kubernetes-native, agentless vault reference application at
<https://github.com/os-santiago/saferus>. Its reviewed bootstrap uses OpenBao;
AI-SDLC does not generate cryptographic primitives or change vault authorization.

This adapter enables one finite low-risk presentation recipe. It does **not**
claim autonomous issue-to-production delivery: artifact build, provenance,
promotion and deployment controllers must also be integrated and demonstrated.

## Reviewed scope

`AI-SDLC validation recipe: saferus-token-lifetime/v1` selects the token lifetime
formatter recipe only for `os-santiago/saferus`. An issue is untrusted input;
it cannot supply a recipe implementation, executable validation command,
credential, destination, policy or success proof.

The candidate must change exactly these existing regular files:

- `web/static/ttl.js`: render nonnegative safe integer seconds as nonzero days,
  hours, minutes and seconds, with singular/plural units; zero is `0 seconds`;
  invalid values remain `Unavailable`.
- `tests/ttl.test.mjs`: regression tests for the formatter.
- `tools/verify_browser.py`: only replace the single exact assertion
  `expect(page.locator('#ttl')).to_have_text('60 seconds')` with the same assertion
  expecting `1 minute`. Every other byte, including certificate validation and
  authentication checks, must remain identical.

Twenty reviewed cases run in separate offline baseline, candidate and reference
Pods. Generated tests use a fourth Pod and do not establish trusted behavior.
Candidate Pods cannot read the expected values or execute the reference driver.
The Node VM restricts module imports and evaluation time; the enforced offline
RuntimeClass, process boundary, resource limits and Pod deadline provide the
containment boundary. Node experimental warnings are suppressed by the trusted
command so the combined Kubernetes log remains strict JSON; invalid output or
nonzero exits still fail validation.

## Installation authority

`examples/saferus.local-install.json` selects a separate local kind installation,
independent review, publication and low-risk merge. Use the standard installer
with reviewed, matching immutable component image evidence and explicit local
GitHub/model credentials. The example is desired configuration, not evidence
that all stages are installed. Keep eligible issue ingestion closed until the
complete intended delivery path has been validated.

Publication plans, merge observations, installed merge policies and API clients
bind the repository independently. A Homedir credential cannot authorize a
Saferus plan or observation, even when PR numbers and Git object IDs coincide.
Only these two reviewed projects are supported; this is not arbitrary repository
onboarding. Existing Homedir recipe behavior remains unchanged.

The installer adds `AI-SDLC low-risk merge protection` requiring strict `quality`
checks from GitHub Actions app `15368`, resolved review threads and no bypass
actors. It never rewrites existing rules, including a stronger human approval
requirement. Current requests for changes and unsuccessful optional checks also
block merge. The durable installer receipt binds repository, manifest and native
ruleset identity; uncertain creation is never replayed.

Migration `0020` extends the publication repository constraint to Saferus without
changing row-level security or service grants. New repository-bound plan and
merge evidence must be produced by the updated components; historical records
must never be edited to manufacture the new fields.

## Reproducible isolated validation

Prepare synthetic input outside any product checkout:

```bash
python3 tools/prepare_saferus_fixture.py \
  --source-repository /path/to/saferus \
  --output /tmp/saferus-recipe-fixture
```

Build the `internal/podtransport` Go test binary, then run
`tools/verify_verification_recipe.py` with `--project os-santiago/saferus`,
`--source-sha d5df3a8aa0a19b8f55dfc29c7bc13a10405a2aaa`, the source checkout,
synthetic candidate directory, pinned worker image, disposable cluster kubeconfig
and report path. The tool refuses an existing verifier namespace and removes its
own namespace by UID after the test. It uses real Kubernetes transport, four
separate Pods and a restricted ServiceAccount. The synthetic candidate is only a
conformance fixture; it is never published as an autonomous product improvement.

The full Go/PostgreSQL suite is run with `tools/validate_runtime.py`. Installer
unit tests include additive creation/reapply, lost-create refusal, cross-repository
receipts and Helm scope validation. These checks do not replace an actual issue,
a real generated PR, native merge checks, provenance and a deployed release.
