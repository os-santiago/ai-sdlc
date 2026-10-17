# AI-SDLC Failure-Mode Runbook

Machine-read failure-mode memory for the self-healing runtime (epic #146).
`scripts/failure-runbook.js` parses the marked entries below and injects the
recorded fix at the top of the ci-repair brief when a failing check's
signature matches — see `.github/workflows/ai-sdlc-ci-repair.yml`.

## Entry contract

Each entry is a `### `<id>` — <title>` section containing the marker
`<!-- ai-sdlc:runbook-entry -->` plus `- **Key:**` bullets. Every field value
is a single line — continuation lines are NOT joined into the field.

- **Signatures:** `deadbeef12345678`, `cafe99` — ci-repair failure-signature
  hashes (exact 16-hex, or a ≥6-hex prefix). The signature is sha256[:16] over
  the sorted normalized `<check>:<first-error-line>` parts — normalization in
  `scripts/failure-runbook.js` (`normalizeLogLine`/`failureSignature`), kept in
  lockstep with the repair workflow's prompt step.
- **Match:** `literal-text`, `/regex/` — tested against the normalized
  signature text plus the root cause. Literals match as case-insensitive
  substrings; `/regex/` compiles with `im` flags; an invalid regex degrades
  to a literal. At least one of `Signatures:`/`Match:` is required for an
  entry to be reachable.
- **Root cause:** one line (optional — clipped into the advisory).
- **Fix:** one line — **required**: entries without a fix are never injected.
- **Prevention:** one line (optional).
- **Resolved by:** provenance — `pr:owner/repo#N`, `issue #N`, … (optional).

Entries are human-maintained here until the postmortem-ingest stage
(`spec/postmortem-lifecycle.md` §Runbook Ingest) writes them automatically.
Consumer repos may carry their own store at the same path on the base branch —
it is consulted first, and the PR head can never supply its own runbook.

## Entries

### `contract-schema-violation` — repo validation gate fails on the contract
<!-- ai-sdlc:runbook-entry -->
- **Match:** `/\.ai-sdlc\.yaml|schema violation|validate-ai-sdlc-config/`
- **Root cause:** `.ai-sdlc.yaml` (or an `examples/*.ai-sdlc.yaml` contract) no longer conforms to `spec/ai-sdlc.schema.json` — usually a renamed/removed field or a drifted constraint.
- **Fix:** Run `bash tools/validate-ai-sdlc-config.sh .ai-sdlc.yaml spec/ai-sdlc.schema.json`, read the reported violation, and correct the contract to conform — never weaken or edit the schema to fit the contract.
- **Prevention:** Validate the contract locally before committing any change under `.ai-sdlc.yaml` or `examples/`.
- **Resolved by:** seed entry — repo contract gate.

### `node-test-failure` — `node --test` red on the repo suite
<!-- ai-sdlc:runbook-entry -->
- **Match:** `/not ok|# fail [0-9N]+|node --test/`
- **Root cause:** A `*.test.js` assertion fails under `node --test` — the implementation drifted from the contract the test encodes.
- **Fix:** Reproduce locally with `node --test <file>.test.js` on the failing file, then fix the implementation to satisfy the assertion — never weaken, skip, or delete the test.
- **Prevention:** Run `node --test` on the touched `*.test.js` siblings before pushing.
- **Resolved by:** seed entry — repo suite gate.

### `actionlint-workflow-violations` — workflow lint red (not repairable by the loop)
<!-- ai-sdlc:runbook-entry -->
- **Match:** `/actionlint/`
- **Root cause:** `actionlint` reports violations in a workflow file under `.github/workflows/`.
- **Fix:** Do NOT attempt a code fix — the repair loop is forbidden from editing `.github/workflows/` and the push step excludes that path. Leave the tree unchanged and report `HUMAN REQUIRED: actionlint violations must be fixed in a workflow-editing change`.
- **Prevention:** Run `./actionlint -color` before pushing workflow changes.
- **Resolved by:** seed entry — workflow lint gate.
