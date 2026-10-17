# Secret-scan baseline — gitleaks full-history triage (issue #125)

One-time baseline of `gitleaks git .` over the full history of
`os-santiago/ai-sdlc`, run at the introduction of the `secret-scan`
workflow (2026-10-17, gitleaks v8.30.1, 234 commits scanned). Four
findings, all triaged below; their fingerprints live in the repo-root
`.gitleaksignore`, which gitleaks auto-loads on every scan.

## Semantics

- **`false-positive`** — matched a detector shape but is not a real
  credential (test fixtures, doc examples, hash chains).
- **`historical-revoked`** — was a real credential but is verifiably
  rotated/revoked; kept in history with the fingerprint suppressed.
- **anything else** — a *verified* finding. The `secret-scan` job fails
  closed on it: PR/push scans cover only the incoming commits, the weekly
  Sunday sweep re-scans all history.

A finding's fingerprint embeds the commit SHA (`<commit>:<file>:<rule>:<line>`).
Re-committing a suppressed secret in a NEW commit yields a NEW
fingerprint and fails the gate — suppression never excuses recurrence.

## Findings

| Fingerprint | Rule | File | Status | Notes |
|---|---|---|---|---|
| `24ef86b…:scripts/injection_defense.test.js:private-key:123` | `private-key` | `scripts/injection_defense.test.js` | `false-positive` | Synthetic PEM fixture (`MIIabc123` body) asserting the secret-redaction test path — never a real key |
| `bffb602…:scripts/injection_defense.test.js:private-key:123` | `private-key` | `scripts/injection_defense.test.js` | `false-positive` | Same fixture, re-landed by PR #302 after the file moved |
| `bfdeee1…:scripts/risk-classifier.test.js:generic-api-key:125` | `generic-api-key` | `scripts/risk-classifier.test.js` | `false-positive` | Test fixture string literal next to a `PRIVATE KEY` marker — feeds the risk classifier, not a provider |
| `e54412b…:scripts/risk-classifier.test.js:generic-api-key:125` | `generic-api-key` | `scripts/risk-classifier.test.js` | `false-positive` | Same fixture in the pre-merge commit of the same change |

(Fingerprints abbreviated for readability; `.gitleaksignore` carries the
full 40-char commit SHAs.)

## Procedure — triaging a new finding

1. `secret-scan` fails and annotates each finding (`::warning`, secret
   redacted). Reproduce locally:
   `bash tools/gitleaks.sh git . --redact --report-format json --report-path /tmp/report.json`
2. If the secret is **live**: rotate/revoke it first — suppression is not
   remediation. Then decide whether history rewriting is warranted.
3. If `false-positive` or `historical-revoked`: append the fingerprint
   from the report to `.gitleaksignore` and add a row to the table above
   in the same commit.
4. Never add a live credential's fingerprint to `.gitleaksignore`.
