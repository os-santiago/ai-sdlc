# Security scanning — Dependabot + CodeQL + gitleaks (issues #105, #125; epic #89)

Three halves of the vulnerability-handling posture ship in-repo:

- **`.github/dependabot.yml`** — weekly dependency-update PRs for the
  `npm` and `github-actions` ecosystems, bounded (≤5 open PRs each) with
  minor+patch bumps grouped into one PR per ecosystem.
- **`.github/workflows/codeql.yml`** — CodeQL static analysis
  (`javascript`, default query suite) on push + PR to `main` and a weekly
  cron. Results upload as SARIF to **Security → Code scanning alerts**.
- **`.github/workflows/secret-scan.yml`** — gitleaks over git diffs
  (issue #125): incoming-commit range on PR + push, full history on a
  weekly cron and manual dispatch. Pinned binary + SHA-256 verify via
  `tools/gitleaks.sh`; fails closed on any finding not fingerprinted in
  `.gitleaksignore`. This is a different surface from the secret-shaped-
  literal check in `tools/validate-ai-sdlc-config.sh` — that one scans
  `.ai-sdlc.yaml` *content* only, at a point in time; gitleaks scans the
  commit history. Baseline + triage register:
  [`security/baseline-scan.md`](security/baseline-scan.md).

The pipeline integration applies to Dependabot only: its PRs carry the
`dependencies` label → the classifier's dep fast-path → review →
automerge, identical to the vetted sweep (`ai-sdlc-dep-update.yml`).

## Repo-settings toggles (admin scope — cannot ship in YAML)

`dependabot.yml` only configures **version** updates. The **security** side
and the fail-the-check threshold are repository settings, reachable via
the UI or `gh api` — all require admin/repo-security-manager scope:

| Setting | Location | Effect |
|---|---|---|
| Dependency graph | Settings → Code security → Dependency graph | Source signal for Dependabot alerts — prerequisite for everything below |
| Dependabot alerts | same section | Advisories land under Security → Dependabot |
| Dependabot security updates | same section | Advisories auto-generate fix PRs (independent of `dependabot.yml` version updates) |
| Code scanning → check-runs failure | Settings → Code security → Code scanning | Severity threshold at which a code-scanning check **fails the PR** — this is where "fail on high-severity findings" is enforced; CodeQL's analyze step itself only uploads SARIF |
| Secret scanning | Settings → Code security → Secret scanning | GitHub-side detection across the whole repo (independent of the gitleaks gate) — alerts land under Security → Secret scanning |
| Push protection | same section | **Blocks the push** when a commit contains a recognized secret — the pre-merge defense layer gitleaks can't provide, since the gitleaks job only runs after the push lands |

API equivalents (admin scope):

```bash
# Dependency graph / Dependabot alerts / security updates
gh api -X PUT repos/{owner}/{repo}/vulnerability-alerts
gh api -X PUT repos/{owner}/{repo}/automated-security-fixes

# GitHub-side secret scanning + push protection
gh api -X PATCH repos/{owner}/{repo} \
  -f 'security_and_analysis[secret_scanning][status]=enabled' \
  -f 'security_and_analysis[secret_scanning_push_protection][status]=enabled'

# Code-scanning check failure threshold → repo Rulesets or
# Settings → Code security → "Check runs failure" (UI-only today)
```

Enabling these is a one-time admin action on `os-santiago/ai-sdlc` after
this lands; nothing further in-repo is required.
