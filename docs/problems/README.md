# docs/problems/ — Problem docs

Problem docs capture a design question **while it is still open**: the
problem, the options on the table, their trade-offs, and the open
questions. They are the project's design-rigor surface — an evaluator
should be able to see *why* a decision went the way it did, including the
options that were weighed and rejected.

A problem doc is **not** an ADR. ADRs (`../architecture/adr/`) record
decisions already taken and bind the project; problem docs record the
analysis while the question is undecided. The pair links both ways — the
**reversal link is mandatory**:

- when a decision lands, the ADR names the problem doc it resolves
  (`Resolves: docs/problems/NNNN-<slug>.md`);
- the problem doc's status flips to `Resolved` and gains the forward
  pointer (`Resolved by: ADR-NNNN`).

A problem doc abandoned without a decision flips to `Abandoned` with a
one-line cause — it stays in the index either way; the record is the
point.

## Format

- **File**: `NNNN-<slug>.md` — next free number in this directory.
- **Header**: Status (`Open` | `Resolved` | `Abandoned`), Date, Tracking
  (issue/epic links).
- **Sections**, in order:
  1. `## Problem` — what hurts, who it hurts, why now.
  2. `## Constraints` — the non-negotiables any option must respect.
  3. `## Options` — every serious option *with its trade-offs*; mark the
     preferred direction if one exists, never hide the rest. A comparison
     table plus a short analysis per option is the house style.
  4. `## Open questions` — genuinely unresolved points; resolved items
     move into the decision, they don't linger here.
  5. `## Resolution path` — what evidence or event turns this doc into an
     ADR.
- **English only**; link issues, ADRs, and spec anchors rather than
  restating them.

Agents: prefer writing a problem doc over jumping straight to an ADR when
the direction is not yet decided — the ADR asserts, the problem doc
argues.

## Index

| Doc | Problem | Status |
|---|---|---|
| [0001](0001-first-time-contributor-trust.md) | First-time contributor trust — vouching and sign-off without a CLA | Open |
