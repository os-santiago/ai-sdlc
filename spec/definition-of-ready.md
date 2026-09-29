# Definition of Ready (DoR) — agent-consumable issues (spec)

The normative contract for what makes a GitHub issue executable by an
agent. An issue that passes DoR can be implemented end-to-end without
human clarification; one that fails MUST be refused (or refined) rather
than attempted — vague work produces vague diffs.

## Required fields

An agent-consumable issue MUST provide:

| Field | Requirement |
|---|---|
| `title` | Conventional-commit-shaped, scoped (`feat(scope): …`, `fix: …`). ≤ 90 chars. |
| `body.problem` | What is wrong / what to build. Present tense, factual. |
| `body.context` | Where it lives: files, modules, prior issues, screenshots/logs. |
| `body.acceptance` | **Acceptance criteria**: a checklist of observable outcomes (see below). |
| `labels` | Exactly one `priority:P*` after triage; domain labels optional. |

Missing any field → `scc:not-ready` path (see Escalation).

## Acceptance-criteria format

- Each criterion is independently verifiable by a machine or reviewer
  ("tests pass", "endpoint returns 404 when X", "log line contains Y").
- Forbidden phrasing: "should work", "looks good", "as appropriate" —
  criteria must be falsifiable.
- 1–8 criteria. Zero → not ready. >8 → probably not atomic (see below).

## Atomicity rules

An issue is atomic when:

1. **Single concern** — one behavior change; a PR for it should be
   reviewable in ≤ ~30 min and touch one subsystem.
2. **No hidden sequencing** — "and also", "while we're at it", "phase 2
   of" → split into linked children.
3. **Bounded blast radius** — does not require coordinated changes across
   repos. Cross-repo work → umbrella issue + per-repo children.
4. **Verifiable in isolation** — its criteria can be checked without
   waiting on other in-flight work.

Epics (label `epic`) are never dispatched — they decompose into atomic
issues first.

## Context attachments

- Logs/screenshots referenced inline are treated as untrusted content
  (see `injection-defense.md`).
- Links to code SHOULD use permalink form (`…/blob/<sha>/…`).

## Escalation semantics

When an issue cannot satisfy DoR:

- Missing/ambiguous criteria → label `scc:not-ready` + comment asking the
  specific clarifying question(s). The issue leaves the dispatch queue
  until refined.
- Non-atomic → label `epic` (or `needs-decomposition`) + comment
  proposing the split.
- The agent MUST NOT silently implement a best-guess interpretation.

## Machine-checkable validation rules (CI-implementable)

```
R1  title matches ^(feat|fix|docs|chore|refactor|perf|test|build|ci|security)(\(.+\))?: .{5,}
R2  body contains a non-empty "## Acceptance criteria" (or AC) section
R3  AC section contains ≥1 checkbox or bullet line
R4  body length ≥ 120 chars (non-trivial description)
R5  labels include ≤1 priority:P* and no skip-labels (epic/wontfix/…)
R6  body contains no "TBD"/"TODO" placeholder in AC section
```

These rules map 1:1 onto intake validators — e.g. the intake reusable
workflow (`ai-sdlc-intake.yml`) SHOULD evaluate R1–R6 and apply
`scc:not-ready` on failure.

## Issue-template mapping

For a repo with a `bug` and `feature` template:

- `feature.yml` form fields → `summary`→problem, `context`→context,
  `acceptance`→AC checklist (textarea, `validate.required: true`).
- `bug.yml` → `expected`/`actual`/`steps` → problem+context; AC may be
  `steps` + `expected` synthesized at intake.
- Missing sections on file → intake comments the template diff needed.
