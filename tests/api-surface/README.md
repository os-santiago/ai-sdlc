# API-surface snapshots (issue #135)

One JSON per reusable stage: the normalized `on.workflow_call` block —
`inputs`, `outputs`, `secrets` — extracted from `.github/workflows/*.yml`
and key-sorted so the file is byte-stable.

This directory is the public contract of the runtime. Every consumer repo
binds to these names; nothing else checks them at PR time.

## Why it exists

A PR that renames an input, changes a `type`, drops an output, or adds a
required input silently breaks every caller workflow in every consumer
repo. `tests/api_surface.js` diffs each workflow's extracted interface
against the snapshot here; the diff runs inside `node --test` via
`tools/validate-repo.sh` → the `contract` job of `ai-sdlc.yml`, so drift
fails the PR with the leaf-level diff printed.

## Semver rule (ties to spec versioning)

- **MAJOR** — remove or rename an input/output/secret, change a `type`,
  flip `required: false → true`, or add a required input. Needs a spec
  version bump and a migration note per AGENTS.md's semver rule.
- **MINOR** — additive optional inputs/outputs/secrets.

If a diff is intentional, re-record:

```bash
node tests/api_surface.js --bless
```

and call out the breaking surface in the PR body so consumers can pin
accordingly.

## Marker fixtures

Sibling golden fixtures live in `tests/markers/cases/` — emit→parse
round-trips and negative cases for the `<!-- ai-sdlc:* -->` comment markers
that steer the review/merge gates (`scripts/comment-markers.js` registry,
`tests/comment-markers.test.js`, issue #131).
