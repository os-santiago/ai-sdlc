# Security Policy

This document is the vulnerability disclosure policy for
`os-santiago/ai-sdlc` — the canonical AI-SDLC normative spec and
GitHub-Actions-native runtime.

## Scope

In scope:

- The reusable `workflow_call` runtime stages under `.github/workflows/`
  (intake, implement, automerge, verify) and composite actions under
  `.github/actions/`.
- Validation and maintenance tooling under `tools/`, `scripts/` and
  `tests/` (contract validation, schema checks, conformance harnesses).
- The normative spec surface under `spec/` where a defect creates a
  security weakness in conforming deployments — e.g. a trust-boundary
  violation of the model in
  [`spec/injection-defense.md`](spec/injection-defense.md), a promotion-
  gate bypass in [`spec/trust-ladder.md`](spec/trust-ladder.md), or a
  gap in [`spec/risk-taxonomy.md`](spec/risk-taxonomy.md) that lets a
  high-risk change ship without its required evidence.

Out of scope — file these in the owning repository per the ecosystem
layering in [AGENTS.md](AGENTS.md):

- Engine internals → `os-santiago/sc-agent-cli`.
- Deployment and control-plane infrastructure (OmniRoute, Discord,
  memory, orchestration) → `Axel-DaMage/hermes`.
- Vulnerabilities in a downstream consumer's own code or configuration.

## Reporting a Vulnerability

**Do not open a public issue, pull request, or discussion for an
undisclosed vulnerability.**

Report privately via **GitHub Private Vulnerability Reporting**:

1. Open the [Security tab](../../security) of this repository.
2. Select **Report a vulnerability** to draft a private security
   advisory (GHSA).

If Private Vulnerability Reporting is unavailable for your account,
open a draft advisory request through the repository maintainer listed
in the GitHub organization profile — never through public channels.

A good report includes, where possible:

- The affected component (workflow file, tool, spec section, version or
  `spec/v*` tag).
- Reproduction steps or a minimal proof of concept.
- The assessed impact — especially whether it crosses a spec trust
  boundary: untrusted content (T1–T3) executed as instructions,
  secret exfiltration past the secret-scan pass, autonomy escalation
  past the repo's declared trust-ladder ceiling, or a kill-switch
  bypass (`spec/kill-switch.md`).

Reports are accepted in English. No lawyer-friendly preamble is
required; we will not pursue legal action against good-faith security
research that respects this policy.

## Expected Response Timeline

All response steps are bounded — the same doctrine the runtime applies
to itself:

- **Acknowledgment**: within 72 hours of the report.
- **Initial triage**: within 7 days — severity classified per
  [`spec/risk-taxonomy.md`](spec/risk-taxonomy.md) and the threat
  classes in [`spec/threat-model.md`](spec/threat-model.md)
  (external injection, insider threat, denial of service, drift,
  supply chain).
- **Status updates**: at least every 14 days until resolution.
- **Fix target**: 90 days from acknowledgment for confirmed
  vulnerabilities; critical issues are prioritized ahead of planned
  work.

If we cannot reproduce or assess the report, we will ask follow-up
questions inside the private advisory rather than closing it silently.

## Disclosure Process

1. **Private triage** — the maintainers confirm the report inside the
   draft GHSA and classify severity.
2. **Fix development** — remediation is developed on a private branch
   or a temporary private fork attached to the advisory; the fix ships
   through the normal review and merge gates (bounded retries, risk
   tiering, squash merge).
3. **Coordinated disclosure** — once the fix is merged and released,
   the advisory is published with a CVE requested where impact
   warrants. We ask reporters to give us the 90-day window before any
   public disclosure; we will agree on an earlier date jointly if a
   fix lands sooner.
4. **Credit** — reporters are credited in the published advisory unless
   they prefer to remain anonymous.

Superseded or unsupported spec tags receive fixes on a best-effort
basis; only the latest `spec/v*` minor line is guaranteed a backport.

## Note for consumers of this runtime (template)

This policy covers **only** `os-santiago/ai-sdlc`. If your repository
consumes this runtime via `.ai-sdlc.yaml` and the thin caller workflow,
publish your own `SECURITY.md` at your repository root — GitHub does
not inherit this file downstream. A minimal consumer policy is:

```markdown
# Security Policy

## Reporting a Vulnerability

Report vulnerabilities privately via GitHub Private Vulnerability
Reporting (Security tab → "Report a vulnerability"). For defects in
the upstream AI-SDLC runtime itself, report to
os-santiago/ai-sdlc — see https://github.com/os-santiago/ai-sdlc/security/policy.

## Expected Response Timeline

Acknowledgment within 72 hours; status updates at least every 14 days.

## Disclosure Process

Coordinated disclosure: private advisory, fix, then publication with
reporter credit.
```

Distinguish in your policy between defects in **your** code/config and
defects in the upstream spec or reusable stages — the latter route to
this repository.
