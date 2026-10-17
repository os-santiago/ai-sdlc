# Security scanning — Dependabot + CodeQL (issue #105, epic #89)

Two halves of the vulnerability-handling posture ship in-repo:

- **`.github/dependabot.yml`** — weekly dependency-update PRs for the
  `npm` and `github-actions` ecosystems, bounded (≤5 open PRs each) with
  minor+patch bumps grouped into one PR per ecosystem.
- **`.github/workflows/codeql.yml`** — CodeQL static analysis
  (`javascript`, default query suite) on push + PR to `main` and a weekly
  cron. Results upload as SARIF to **Security → Code scanning alerts**.

Both integrate with the pipeline: Dependabot PRs carry the `dependencies`
label → the classifier's dep fast-path → review → automerge, identical to
the vetted sweep (`ai-sdlc-dep-update.yml`).

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
| Secret scanning + push protection | same page | Defense-in-depth alongside supply-chain scanning |

API equivalents (admin scope):

```bash
# Dependency graph / Dependabot alerts / security updates
gh api -X PUT repos/{owner}/{repo}/vulnerability-alerts
gh api -X PUT repos/{owner}/{repo}/automated-security-fixes

# Code-scanning check failure threshold → repo Rulesets or
# Settings → Code security → "Check runs failure" (UI-only today)
```

Enabling these is a one-time admin action on `os-santiago/ai-sdlc` after
this lands; nothing further in-repo is required.
