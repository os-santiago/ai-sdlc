# Repository quality checks

GitHub Actions validates this repository. AI-SDLC execution lives only in
`runtime/k8s/`; the QA workflows do not implement issues or merge PRs.

| Workflow | Evidence |
| --- | --- |
| `quality.yml` | `contract`: portable schema examples, rejection tests, Go build/vet and PostgreSQL tests; security-log tamper/signature tests; Codex wrapper protocol tests |
| `secret-scan.yml` | `gitleaks`: incoming commits on PR/push, full history weekly |
| `codeql.yml` | Go runtime, JavaScript security log and Python wrapper static analysis, SARIF in Code scanning |
| `sbom.yml` | `sbom`: CycloneDX inventory of Go dependencies, retained 14 days |
| `actionlint.yml` | Workflow syntax, expressions and shell checks |
| `security-log-validate.yml` | Integrity of `SECURITY_LOG.md` when present |
| `security-log-sign.yml` | Weekly signature evidence for the log when present, not a software release |

Required checks retain their existing names: `contract`, `gitleaks`, `sbom`,
`codeql (javascript)` and the CodeQL App's `CodeQL`. They must complete on the
PR revision; do not substitute manually posted statuses. PR quality checks
run on all target branches to support stacked changes and on forks with read
permissions. No model credentials or runtime App keys are used.

## Reproduce locally

From the repository root, with Python 3, Node.js 22, Go (version in
`runtime/k8s/go.mod`), Bash, curl and shellcheck:

```bash
python3 -m venv /tmp/ai-sdlc-quality-venv
source /tmp/ai-sdlc-quality-venv/bin/activate
python3 -m pip install -r tools/quality-requirements.txt
bash tools/test-contracts.sh
node --test scripts/security-log.test.js
python3 -m unittest discover -s runtime/k8s/tools -p 'test_*.py' -v
bash tools/actionlint.sh
bash tools/gitleaks.sh git . --redact
cd runtime/k8s
go build ./...
go vet ./...
go test ./...
```

To include persistence tests, provision a **fresh disposable PostgreSQL 17
database**, export `AI_SDLC_TEST_DATABASE_URL`, then run `go test -p 1 ./...`
from `runtime/k8s`. Use a new database on every invocation: fixtures share
stable IDs and are not designed for concurrent package execution or reuse.
CI provisions a fresh service for every run. Without the variable, database
tests skip; a green local unit run alone is not persistence evidence.

CodeQL and SBOM evidence are produced by the SHA-pinned actions. Download the
SBOM from the run's artifacts and review the Code scanning results before
merging. Security-log signing publishes only log attestations and does not
grant the QA jobs release, deployment, or AI-SDLC orchestration permissions.
