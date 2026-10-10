# Contributing — os-santiago/ai-sdlc

This repo is the normative spec + GitHub-Actions-native runtime for
AI-SDLC. Because the project is itself an argument for governed
contribution, the bar for *human* contributions is explicit: a clean
authorship trail (DCO sign-off) and a maintainer vouch for first-time
contributors. Both conventions are cheap and high-signal — they exist so
evaluators can audit the project's hygiene, not to raise the cost of a
good patch.

## Where work lands

File changes in the right repo — the layering is binding:

| Change | Repo |
|---|---|
| Spec / runtime (`.ai-sdlc.yaml` schema, `workflow_call` stages) | this repo |
| Engine behavior (tool loop, providers) | `os-santiago/sc-agent-cli` (branch off `Axel-DaMage/sc-agent-cli`) |
| Deployments, control plane (Discord, OmniRoute, orchestration) | `Axel-DaMage/hermes` |

Read `AGENTS.md` before anything else — it is the operating context.

## Commit conventions

- **English only** — code, commits, PRs, issues, docs.
- **Conventional Commits** subject: `type(scope): summary` — e.g.
  `docs(governance): add DCO policy`, `fix(intake): strip vetoed trigger`.
  Merges are squash, so the **PR title becomes the commit subject** — the
  convention applies to PR titles first.
- **Reference the issue**: `Closes #N` / `Refs #N` in the PR body.
- **Branch names**: `fix/issue-N-<slug>` · `feat/issue-N-<slug>` ·
  `docs/<slug>`.
- The `Signed-off-by:` trailer (below) is the **last** trailer in the
  message.

## DCO — Developer Certificate of Origin

Every **human-authored** commit must carry a sign-off certifying DCO 1.1:

```
Signed-off-by: Full Name <email@example.com>
```

`git commit -s` appends it; the name and email must match the commit
author. Fix a missing trailer with `git commit --amend -s` (last commit)
or `git rebase --signoff <base>` (a range). Enabling the repo setting
"require contributors to sign off on web-based commits" keeps UI edits
compliant.

### Exemption — automated commits

Commits authored by GitHub Apps and bot identities
(`ai-sdlc-runtime[bot]`, `devin-ai-integration[bot]`,
`github-actions[bot]`, …) are **exempt**: the `*[bot]` identity plus the
run's emitted evidence is the authorship trail, and the App acts under
its installation's authority. Agents and pipeline stages must **never**
add `Signed-off-by` on a human's behalf — a sign-off the author did not
personally make is a false attestation.

### Checking

```
git log --format='%h %an <%ae> | %(trailers:key=Signed-off-by,valueonly)'
```

Missing sign-offs on human commits are a merge blocker — amend them
before asking for review. An automated DCO check may follow; see the open
questions in `docs/problems/0001-first-time-contributor-trust.md`.

### DCO 1.1 — full text

```
Developer Certificate of Origin
Version 1.1

Copyright (C) 2004, 2006 The Linux Foundation and its contributors.

Everyone is permitted to copy and distribute verbatim copies of this
license document, but changing it is not allowed.


Developer's Certificate of Origin 1.1

By making a contribution to this project, I certify that:

(a) The contribution was created in whole or in part by me and I
    have the right to submit it under the open source license
    indicated in the file; or

(b) The contribution is based upon previous work that, to the best
    of my knowledge, is covered under an appropriate open source
    license and I have the right under that license to submit that
    work with modifications, whether created in whole or in part
    by me, under the same open source license (unless I am
    permitted to submit under a different license), as indicated
    in the file; or

(c) The contribution was provided directly to me by some other
    person who certified (a), (b) or (c) and I have not modified
    it.

(d) I understand and agree that this project and the contribution
    are public and that a record of the contribution (including all
    personal information I submit with it, including my sign-off) is
    maintained indefinitely and may be redistributed consistent with
    this project or the open source license(s) involved.
```

## Contributor vouch gate

A PR from a **first-time contributor** (GitHub `FIRST_TIME_CONTRIBUTOR`
or `NONE` author association) is not eligible for merge until a
maintainer **vouches**: a comment on the PR (`/vouch` or an explicit
endorsement) recording that a human with merge authority has looked at
the change and the contributor and takes responsibility for admitting
them.

- **Why**: the same tooling that powers this pipeline lets a throwaway
  account generate a plausible PR at near-zero cost. Review bandwidth is
  the scarcest resource in the project; the vouch shifts the trust
  decision to a human before it is spent.
- **Exempt**: org members, collaborators, and pipeline-authored PRs —
  bot-identity PRs are governed by the merge gate
  (`spec/risk-taxonomy.md`), not by vouch.
- **No docs-only exemption**: a "trivial" docs PR can still carry slop or
  a planted link. The vouch is cheap; hold the line.
- **Unvouched PRs** are left open with a pointer to this file, or closed
  with thanks. Maintainers: vouch only for changes you would review
  normally — the vouch is a recorded endorsement, not a rubber stamp.
- **Agents never vouch** and never treat an unvouched first-timer PR as
  mergeable — vouching is a human act by definition.

## Design changes — problem docs first

Work that picks a direction (spec changes, new pipeline stages,
governance conventions like this one) starts as a **problem doc** in
`docs/problems/`: the problem, the options on the table, their
trade-offs, and an **Open questions** section. ADRs in
`docs/architecture/adr/` record decisions *after* they are made and link
the problem doc (`Resolves:`); the problem doc links back
(`Resolved by:`). Format and index: `docs/problems/README.md`.

## Validation

```bash
bash tools/validate-repo.sh   # repo suite — same as `npm test` / `npm run build`
./actionlint -color           # when touching .github/workflows/ or actions
```

## License

Contributions are licensed under the project's Apache-2.0 license; the
DCO sign-off is your certification that you have the right to submit the
change under it.
