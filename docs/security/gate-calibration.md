# Security-gate calibration — which findings block vs report (issues #90, #93; epic #87)

Normative calibration of the deterministic supply-chain gate declared by
the `security:` contract section
([spec/ai-sdlc.schema.json](../../spec/ai-sdlc.schema.json)) and floored
by the org baseline
([spec/org-policy.md](../../spec/org-policy.md#security-gate) —
Security Gate). The gate emits the required check `ai-sdlc-security`;
this page is the lookup table a maintainer consults to answer "does this
finding hold the merge?"

Two axes decide the verdict, both resolved by code — never by generative
judgment:

1. **Finding class** — which scanner produced it (secrets, dependency
   vulnerabilities, licenses) and, for CVEs, the severity plus whether an
   upstream fix exists.
2. **PR risk tier** — the `pr:risk-*` label the classifier assigned
   ([spec/risk-taxonomy.md](../../spec/risk-taxonomy.md)), consulted
   through `security.severity_matrix`.

## Finding-class calibration (epic #87 design decisions)

| Finding class | Verdict | Contract control |
|---|---|---|
| Secret / credential material | **Blocks** — always, at every tier and severity | `security.secrets_scan: enforce` (default) + `security.block_on_secret: true` (default, boolean floor) |
| CVE ≥ `security.min_severity` (default `high`), upstream fix available | Per `security.severity_matrix` for the PR's `pr:risk-*` tier | `security.dependency_audit: enforce` (default) + `min_severity` + `severity_matrix` |
| CVE ≥ `min_severity`, **no** upstream fix | **Report-only** — `blocking-if-fix-available` degrades when nothing can be acted on; only an explicit `blocking` tier outcome fails the check anyway | `severity_matrix.<tier>` |
| CVE < `min_severity` | **Report-only** — recorded on the check, never fails it | `security.min_severity` (severity floor — org `high` clamps a repo `critical`) |
| License observation | **Report-only** at the normative `license_check: report` default — licenses are not severity-classed, so `severity_matrix` never applies; an org may raise the floor to `enforce` | `security.license_check` (`off` \| `report` \| `enforce`) |
| SBOM | Never a verdict — deterministic evidence artifact attached per `security.sbom.attach_to` (`pr`, `release`); feeds the SLSA attestation flow (issue #92) | `security.sbom.{enabled,format,attach_to}` |

The `blocking` severity-matrix outcome is deliberately stronger than any
default: it fails the check even for fixless findings, for orgs that
would rather halt a tier than track unfixable debt.

## Tier calibration (`severity_matrix` normative defaults)

| `pr:risk-*` tier | Default outcome | Blocks on |
|---|---|---|
| `pr:risk-low` | `report-only` | Nothing — findings recorded only (secrets still block via `block_on_secret`) |
| `pr:risk-medium` | `report-only` | Nothing (secrets still block) |
| `pr:risk-high` | `blocking-if-fix-available` | CVE ≥ `min_severity` with an upstream fix |
| `pr:risk-critical` | `blocking-if-fix-available` | CVE ≥ `min_severity` with an upstream fix |

## Org-floor calibration (who may weaken what)

Every org-declared `security.*` leaf is a floor — repos tighten, never
loosen ([floor semantics table](../../spec/org-policy.md#floor-fields)):

| Org declares | Repo attempting | Resolved |
|---|---|---|
| `enabled: true` | `enabled: false` | `true` — gate cannot be switched off |
| `min_severity: high` | `min_severity: critical` | `high` — stricter (lower-rank) threshold stands |
| `secrets_scan: enforce` | `secrets_scan: report` / `off` | `enforce` — ranked scan-mode floor |
| `license_check: report` | `license_check: off` | `report` — same floor, repo may only raise to `enforce` |
| `sbom.enabled: true` | `sbom.enabled: false` | `true` — boolean floor |
| `sbom.attach_to: [release]` | `attach_to: [pr]` | `[pr, release]` — strictly-additive union |
| `severity_matrix.pr:risk-high: blocking-if-fix-available` | `report-only` | `blocking-if-fix-available` — strictest outcome wins |

Fields with no strictness dimension (`sbom.format`) are pin-able outright
via `policy_floor.pin` — a pinned scalar takes the org value with no
tightening direction. A repo's own `policy_floor` section is always
ignored: floors are org-baseline only.

## What the gate does not do

- It does not replace the AI review gate — it is additive and
  deterministic (epic #87 non-goals).
- It never blocks on what cannot be fixed — `blocking-if-fix-available`
  exists precisely so fixless CVEs cannot paralyze the merge loop.
- A repo cannot opt out under an org baseline — `enabled` is a boolean
  floor. Standalone repos (no `org-config`) keep full control.
