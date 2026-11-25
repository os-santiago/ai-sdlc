# Candidate quality and security baseline (B8)

Status: first part in review. Secret scanning and optional static analysis
are implemented; dependency scanning is designed below and not implemented.

[Strategic principle P3](strategic-principles.md#p3-embedded-automation-and-policy-enforce-the-expected-behaviour)
requires every candidate change to meet a standard quality and security
baseline before it can merge: tests, static analysis, dependency scanning and
secret scanning. [Audit gap 3.3](principles-audit-2026-11-24.md) found that
candidates were checked only by the target repository's tests and the
independent review, plus the publication path allowlists and size bounds.
This is release plan item B8.

## Where the baseline runs

Everything runs in the verifier, before the independent review. A refused
candidate never gets a behaviour proof. Without a proof there is no review,
no publication and no merge.

| Check | When | Where | Default | Refusal stop |
|---|---|---|---|---|
| Secret scan | After the candidate delta is decoded and applied to the fetched base, before any pod | Verifier controller, in process | Always on | `refused-candidate-secret` |
| Static analysis | With the baseline and candidate test runs | The repository-tests pods, in the pinned toolchain image | Off; a manifest opts in with `verification.analysis` | `refused-candidate-analysis` |
| Dependency scan | Not implemented | Designed below | None | None |

## How it composes with the evidence and the merge gate

The merge gate does not need a new evidence field.
- A refusal is a verification decision. The controller records it like any
  other refusal: the request fails with reason `comparison-failed`, under a
  live claim and current authority. No database migration is needed.
- **The secret scan is bound into signed evidence.** The `repository-tests/v1`
  recipe revision now hashes `secretscan.Version` (`secret-scan/v1`) along with
  the decision rules. The verification command signs that revision. The
  verifier, the publication command and the published-proof check all
  compare it with their own build. The merge controller runs that
  published-proof check before every merge request. So:
  - a command signed for an older rule set is rejected before any effect;
  - a proof produced without the scan cannot become a publication;
  - a pull request published from such a proof is not merged by the
    runtime.
- **The analysis settings are bound by the manifest digest.** The authority
  policy binds the manifest digest (`projectSHA256`), and every command
  carries the policy revision. The proof's decision summary records:
  - `analysisCommands`: how many commands ran;
  - `analysisBaselineFailing`: commands that already fail on the base code;
  - `analysisNewFailures`: commands the candidate newly fails.

  The reviewer sees the same summary.

Only `repository-tests/v1`, the recipe every manifest repository uses,
binds the scan version. The reference recipes (Homedir, Saferus) are
fixed demonstration oracles. Their candidates are scanned too, but their
recipe revisions hash only their reviewed oracle files, so a proof from them
does not attest that the scan ran.

## Secret scanning

Package `runtime/k8s/internal/secretscan`. It is deterministic and offline:
- a fixed set of regular expressions (RE2, linear time);
- no network, no model, no new image.

**Delta only.** It scans only the lines the candidate adds:
- a line counts as added when the base file has no identical line;
- a new file is all added lines;
- a deleted file adds nothing;
- moving or keeping existing text never refuses a candidate;
- a new key under an existing PEM header line is still found, by its new
  body line.

The input is bounded by the candidate delta limits (64 changes, 2 MiB
encoded).

### Rules

| Rule ID | Matches | False-positive controls |
|---|---|---|
| `private-key` | `-----BEGIN [... ]PRIVATE KEY-----` (RSA, EC, DSA, OPENSSH, ENCRYPTED, PGP `... BLOCK`) followed by key material: an escaped `\n` and a base64 run on the same line (JSON service account files), or a base64 line within the next four lines (PEM armor headers and blank lines skipped) | A header alone does not match: a PEM parser, an error message or a `HasPrefix` check does not match. The body must have 40 or more base64 characters with upper case, lower case and digits, so a long word does not count. Public keys and certificates never match |
| `aws-access-key-id` | `AKIA` or `ASIA` followed by exactly 16 characters of the base32 alphabet (`A-Z`, `2-7`) | Not inside a longer alphanumeric run. Values ending in `EXAMPLE`, as in AWS documentation, are ignored |
| `aws-secret-access-key` | `aws_secret_access_key`, `AWS_SECRET_ACCESS_KEY` or `awsSecretAccessKey`, optionally quoted, then `=`, `:`, `:=` or `=>`, then exactly 40 base64 characters | Keyword-anchored. A run of 40 characters alone never matches. The value needs 10 or more distinct characters, which rules out `aaaa...`. Values ending in `EXAMPLEKEY` are ignored. `${VAR}` references do not match |
| `github-token` | `ghp_`, `gho_`, `ghu_`, `ghs_` or `ghr_` followed by exactly 36 alphanumerics, or `github_pat_` followed by 22, an underscore and 59 alphanumerics | Exact lengths with boundaries on both sides. The body needs 10 or more distinct characters, which rules out `ghp_xxxx...` and `ghp_0123...` placeholders |
| `gitlab-token` | `glpat-` followed by 20–64 characters from `[A-Za-z0-9_-]` | Bounded on both sides. The body needs 10 or more distinct characters |
| `google-api-key` | `AIza` followed by exactly 35 characters from `[A-Za-z0-9_-]` | Exact length. The body needs 10 or more distinct characters |
| `azure-storage-key` | `AccountKey=` followed by exactly 86 base64 characters and `==` | The exact length of a 64-byte key. The body needs 10 or more distinct characters |

**Not covered, on purpose:** generic high-entropy assignments
(`password = "..."`, `token: ...`). They cannot be made precise enough to
refuse candidates automatically: test fixtures, hashes, UUIDs and encoded
data all match. Vendor formats without a fixed prefix are not covered
either. Adding a rule needs a new version and its own true- and
false-positive tests.

**Remaining false positives.** A target repository's test that embeds a
realistic fake token or a real-looking private key, for example a TLS test
fixture, is refused. This is deliberate: a scanner cannot tell a fake from a
real credential. Committing either trains reviewers to ignore secrets.
Fixtures should be generated at run time, or assembled from split literals,
as the scanner's own tests do. A candidate that keeps an existing fixture
line unchanged is not refused.

### Leakage guarantees

- A finding is only `{Rule, PathClass}`. The rule is one of the seven IDs
  above. The path class is `code`, `config`, `docs`, `key` or `other`,
  derived from the file name. The matched text, its line and the path are
  never returned, so no caller can log, store or publish them.
- The refusal reaches the database only as `comparison-failed`.
- The verifier log line is
  `verification reconciliation stopped safely: refused-candidate-secret rule=<rule> class=<class>`.
  Both values are checked against the bounded sets before logging.
- The metric stage is `refused-candidate-secret`, without the rule.
- The scan runs before any pod is created and before the independent review,
  so the candidate's code never runs and the reviewer model never receives
  the delta.

Tests check that neither the rendered finding nor the error text, stage or
log detail contains the secret, a prefix of it, or the path. Every test
token is assembled at run time, so the repository's own gitleaks gate does
not match the test sources.

## Static analysis

`verification.analysis` is optional. It declares up to 4 argv commands and one
`timeoutSeconds`: 10–300 seconds per command, at most 600 seconds in total.

```json
"analysis": {"commands": [["go", "vet", "./..."]], "timeoutSeconds": 120}
```

- The commands run in the same pinned toolchain image, offline, with the
  same working directory, fixed environment, reviewed `env` and discarded
  output as the tests. No new image or network is involved.
- They run in the baseline and candidate pods, not in the cross run. The
  runner makes their own copy of the verified tree with the other copies,
  before any repository code runs. It runs the analysis before any test, so
  no test can alter what is analysed.
- Each command's exit code goes on the first result line. A timeout is 124;
  a command that could not run, or was not found on the reviewed `PATH`, is
  125. The pod deadline and the result wait grow by commands × timeout.
- **Decision.** The analysis rule applies only after every test rule passes,
  so an earlier test refusal keeps its reason. Then:
  - a command that passes on the baseline and fails on the candidate refuses
    with `candidate-analysis`;
  - a command that already fails on the baseline gives no signal for this
    candidate and is counted in `analysisBaselineFailing`.

  Exit codes cannot tell new findings from old ones. A repository whose
  analysis already fails should fix it, or scope the command to a passing
  subset, to get a signal.
- **Missing or extra evidence.** Analysis results that do not match the
  settings refuse with `incomplete-evidence`. This covers a missing code,
  codes on the cross run or a repetition, and codes without settings.
- **Choosing commands.** Prefer analyzers that do not execute repository
  code (`go vet`, linters). An analyzer that runs repository code has the
  same known limits as the tests.
- **Protect the analyzer's configuration.** A candidate can switch off its
  own analysis by changing the analyzer's configuration, for example
  `.golangci.yml`, an ESLint or Ruff configuration, or a `Makefile` target
  the command calls. List those files in `verification.harnessGlobs`, so a
  candidate that changes them is refused as `harness-changed`. In-source
  suppressions, such as `//nolint` or `eslint-disable` comments, cannot be
  refused by path. They are visible in the delta the independent reviewer
  reads, and the reviewer should treat a new suppression as a finding.
- **Time budget.** Analysis runs inside the same 30-minute verification
  command window as the tests (`MaxCommandAge`). The candidate pod needs
  about `repeat × timeoutSeconds + commands × analysis timeout`, plus up to
  480 s of overhead (300 s for image pulls, 120 s for the ready wait and a 60 s
  margin, as `repositorytests.DeadlineSeconds` adds). The manifest rules bound analysis
  to 600 s, but they do not check the sum against the window: a manifest
  near the test limits (for example `repeat: 3` with a 600 s timeout)
  already exceeds it without analysis. Keep the total well inside 30
  minutes. Otherwise the run stops at `runs-context` or `runs-deadline`,
  which is never a pass.

## Upgrade and default behaviour

- **Manifests without `verification.analysis` are unchanged, byte for byte:**
  - the canonical form and digest;
  - the verifier settings document and its digest;
  - every pod render (checked against digests recorded on `main`);
  - the runner's result lines;
  - the decision summary and its digest.
- **The secret scan is always on.** It can refuse a candidate that passes
  today only when that candidate adds a credential that matches a rule above,
  or a realistic fake one. No valid candidate should add either. Making it
  opt-in would leave the default installation without the baseline that P3
  requires.
- **In-flight work.** The `repository-tests/v1` recipe revision changes.
  Work signed under the previous revision can never pass the current proof
  checks. Every queue skips or rejects it, so it cannot block newer work:
  - a queued verification command is rejected when polled;
  - review selection and publication handoff skip a passed verification
    whose command names another revision;
  - merge selection skips a published pull request whose verification
    command names another revision. Before this, such a pull request would
    have been selected first on every tick and denied
    (`merge-authority-denied`), blocking every later merge.

  Selection only filters. It never changes historical evidence and grants
  no authority. A queued review under the old revision fails its proof check
  and is rejected on the review controller's next poll. A queued publication
  request under the old revision fails its proof check and lapses at its
  expiry, at most the 30-minute command window.

  **Recovery.** A skipped pull request stays open, unmerged, with its work
  item still running, as for any other obsolete publication (see the
  [merge installation guide](../operations/merge-installation.md)). Close
  the pull request by hand; delivering the change needs a fresh eligible
  issue. To avoid this, drain the inner loop before upgrading: no queued
  verification and no open runtime pull request.
- **Matching runner.** The runner that executes analysis ships in the
  model-mailbox image, which the installer pins from the same revision as
  the verifier. An older runner refuses analysis settings as unknown fields,
  and the `analyze` argument as a usage error. Either is a transport stop,
  never a pass.

## Dependency scanning: next step

Not implemented in this part. It cannot be done safely offline with what the
verifier has today:
- vulnerability databases (the Go vulnerability database, OSV) are fetched
  over the network;
- verification pods run with `network: none`.

Two facts limit today's exposure:
- **The harness rule.** The sample manifest lists `go.mod` and `go.sum` in
  `harnessGlobs`, and the generic guidance lists dependency files there. A
  candidate that changes them is refused (`harness-changed`), so a candidate
  cannot add or upgrade a dependency under such a manifest.
- **The analysis hook.** A repository whose toolchain image carries a
  database snapshot can already run an offline scanner through
  `verification.analysis`. Only new failures refuse. Example:
  `govulncheck -db file:///opt/vulndb ./...`.

**Proposed design** for manifests that let candidates change dependencies:
1. **A pinned vulnerability database image.** A scheduled main-branch
   workflow publishes an OCI image with a database snapshot and its
   timestamp. The image is keyless-signed and its digest is recorded, like
   the runtime images (B5). The installation pins it by digest.
2. **A freshness bound.** The verifier reads the snapshot timestamp from the
   image label. It refuses to run the scan, as a named stop rather than a
   pass, when the snapshot is older than a configured age (for example 7
   days). A stale database cannot silently approve a candidate.
3. **A declared scope.** `verification.dependencies` names the dependency
   globs and the scanner. Examples: `govulncheck` with a `file://` database,
   or `osv-scanner --offline`.
4. **When it runs.** The scan runs only when the delta changes a matching
   file. The verifier copies the snapshot into the baseline and candidate
   pods, as the generator toolchain is copied today.
5. **Decision.** Only findings new on the candidate refuse, as
   `candidate-dependency`, compared by vulnerability ID rather than exit code.
   This needs a bounded, structured result from the scanner: IDs only, never
   repository output.
6. **Binding.** The database image digest is part of the manifest or the
   installation policy, so the proof binds which snapshot was used.

## What remains for B8

- Dependency scanning, as designed above.
- A managed SAST rule set beyond what a repository declares, for example a
  pinned Semgrep or CodeQL image with a reviewed rule pack. The analysis
  hook covers what the repository's own toolchain provides.
- Comparing analysis findings rather than exit codes, through a structured
  report format such as SARIF, digested like the JUnit report. Repositories
  with existing findings would then get a signal too.
- Running the analysis commands in the generator's sandbox before export
  (like B1 for tests), so generated candidates fix findings before they are
  verified.
- A live evaluation: a battery with a manifest that opts in to analysis,
  and an observed `refused-candidate-secret` on a seeded issue.
