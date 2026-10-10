# Problem 0001: first-time contributor trust — vouching and sign-off without a CLA

- **Status**: Open — direction chosen (DCO + vouch, `CONTRIBUTING.md`),
  ADR pending on soak
- **Date**: 2026-10-10
- **Tracking**: [#215](https://github.com/os-santiago/ai-sdlc/issues/215) ·
  epic [#153](https://github.com/os-santiago/ai-sdlc/issues/153)

## Problem

Two exposure surfaces grow with adoption, and today neither is covered:

1. **Legal cleanliness of the contribution trail.** Enterprise adopters
   audit authorship provenance before adopting a governance tool.
   Nothing currently attests that a human contributor had the right to
   submit their patch — the trail is `git log` and good faith.
2. **AI-slop PRs.** The same tooling that powers this pipeline lets a
   throwaway account generate a plausible-looking PR at near-zero cost.
   Maintainer review bandwidth — effectively a single operator — is the
   scarcest resource in the project. An unfiltered first-time-contributor
   surface converts that bandwidth into a free QC service for slop
   generators.

The project sells governed AI contribution; it cannot be seen to run its
own intake on the honor system.

## Constraints

- **Single-maintainer bandwidth** — the gate cannot create standing work;
  it must spend attention only where a human already has to look.
- **Pipeline PRs must not be caught** — `ai-sdlc-runtime[bot]` and other
  automation identities are already governed by the merge gate
  (`spec/risk-taxonomy.md`); a human-trust gate applied to bots is dead
  weight and muddles the authorship story.
- **No legal team** — the mechanism must be self-serve (a git trailer, a
  comment), not a document a lawyer has to countersign.
- **Drive-by fixes should stay possible** — a gate that requires a
  pre-accepted issue for every typo fix trades real goodwill for marginal
  slop protection. (Tension held in Open questions.)

## Options

| Option | Mechanism | Slop filter | Legal trail | Friction |
|---|---|---|---|---|
| A — CLA | CLA-assistant bot; signature once per contributor | Weak — a form, not a judgment | Strongest — explicit grant | High — legal review deters drive-bys |
| **B — DCO + vouch** (chosen direction) | `Signed-off-by` trailer on human commits + maintainer `/vouch` on first PR | Strong — human gate on first contribution | Adequate — per-commit attestation, kernel-proven | Low |
| C — GitHub-native gates only | Actions approval for first-time authors + required reviews | Moderate — approval exists already | None — no attestation | Lowest |
| D — Require a triaged issue per PR | unlinked PRs auto-closed | Strongest | None | High — blocks unsolicited fixes |

### A — CLA

The maximal legal answer, and the wrong fit: it front-loads the
heaviest-weight instrument on the lowest-volume problem (this repo sees
human contributions in the single digits), it needs account-level
signature infrastructure, and it deters exactly the small fixes a young
project wants. Revisit only if contributions scale or an adopter's legal
team demands a grant stronger than attestation.

### B — DCO + vouch (chosen direction)

Two cheap, independent mechanisms that cover both problems separately:

- **DCO** gives the legal trail: each human commit certifies DCO 1.1 via
  `Signed-off-by:` — the Linux-kernel/CNCF pattern, self-serve,
  machine-checkable. Bot-authored commits are exempt; the `*[bot]`
  identity plus run evidence *is* the authorship trail.
- **Vouch** gives the slop filter: a first-time contributor's PR is not
  merge-eligible until a maintainer comments `/vouch`. The cost is one
  comment, paid only where a maintainer already looked — no standing
  queue, no form.

Weakness: both rely on maintainer discipline (checking trailers,
vouching honestly) rather than enforced machinery. Acceptable at current
volume; see Open questions.

### C — GitHub-native gates only

GitHub already requires approval before Actions run on first-time
contributors' PRs, and branch protection requires review. Necessary but
not sufficient: neither produces an authorship attestation, and an
approval click is not a recorded endorsement a later auditor can find.
Kept as the floor — Option B builds on it, it doesn't replace it.

### D — Require a triaged issue per PR

The strongest slop filter — a PR with no triaged issue never enters
review — but it blocks unsolicited fixes and converts every typo into a
two-step process. Held in reserve as an escalation path if B proves
insufficient at volume (see Resolution path).

## Open questions

- **Vouch scope**: per-repo or per-org — does a maintainer vouch here
  carry to consumer repos running this runtime, or does each repo vouch
  separately?
- **Enforcement**: is maintainer discipline enough at current volume, or
  does the DCO check belong in CI (a trailer lint over the PR's commits)
  sooner rather than later?
- **Bot-identity boundary**: is `*[bot]` naming + GitHub App authorship a
  sufficient exemption rule, or does it need an explicit allowlist as
  more automation identities appear (Hermes, future engines)?
- **Vouch artifact**: is a `/vouch` comment the right record, or should
  the endorsement be a label — which would put it inside the label state
  machine's write-authority rules (`spec/labels.md`)?
- **Escalation signal**: what observed signal (slop PRs/week? repeat
  offenders?) triggers revisiting Option D?

## Resolution path

The chosen direction is already written down in `CONTRIBUTING.md` — the
convention soaks before the record hardens. When ~5 first-time human PRs
have passed under the gate (or 90 days, whichever first), an ADR in
`docs/architecture/adr/` records the outcome, names this doc
(`Resolves:`), and this doc flips to `Resolved` pointing forward.
