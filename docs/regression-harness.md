# Pipeline regression harness (issue #193)

A golden-fixture replay suite that pins the pipeline's deterministic
decision surfaces. Every case replays the **real** shipped code — inline
workflow scripts verbatim, library modules directly — never a copy, so any
behavior change lands as a readable diff and fails the suite.

Modeled on the Hermes eval-suite pattern (golden fixtures + replay + diff
report): fixtures capture known-good behavior once ("blessing"); every
later run replays the same inputs and structural-diffs the output.

## What it covers

| Case type | Replay target |
|---|---|
| `intake-dor` | DoR (R1–R6) verdict — the inline `node - <<'NODE'` script of the *Validate DoR* step in `.github/workflows/ai-sdlc-intake.yml`, replayed verbatim in a sandboxed fs/env/process |
| `merge-gate` | Merge-gate decision table — the inline node script of *Evaluate merge gate* in `.github/workflows/ai-sdlc-automerge.yml`, with fetched JSONs (`pr.json`, `checks.json`, `trusted-comments.json`, `ext-reviews.json`) and `Date.now()` pinned by the fixture |
| `risk-classify` | `classifyDiff()` in `scripts/risk-classifier.js` — `pr:risk-*` taxonomy assignment |
| `label-ops` | `applyRiskLabel()` against a recording `gh` stub — the label-mutation decision and the exact argv stream |
| `engine-adapter` | `scripts/engine_adapter.js` — argv building, exit-code mapping, capabilities, `{ENGINE}_BINARY` override |
| `contract-resolve` | `.github/actions/load-config/resolve.py` — contract resolution and flat step outputs (requires `python3`; case skips when absent) |
| `normalized-event` | `.github/actions/github-to-normalized-event/index.js` — webhook → normalized event |

Because the DoR and merge-gate cases extract the script text from the
workflow files themselves, a workflow restructure that moves the decision
code fails the harness loudly (the `mustContain` marker names what was
expected) — that failure is itself the regression signal.

## Running

```bash
node tests/regression_harness.js            # replay all cases, print diff report
node tests/regression_harness.js --case ID  # one case (repeatable, or comma list)
node --test tests/regression.test.js        # same suite under node --test
npm run test:regression                     # npm hook for the direct CLI
```

The suite is wired into `tools/validate-repo.sh` (the `*/*.test.js` step),
so `npm test`, `npm run build`, and the verify stage all run it.

Exit 0 = every case matches its golden (skips tolerated). Exit 1 = drift,
missing golden, or replay error. On failure the console report shows
per-field diffs and a machine-readable artifact lands in
`tests/regression/results/report.json` (gitignored; override with
`--report FILE`).

## Case anatomy

`tests/regression/cases/<id>.json`:

```json
{
  "id": "gate-review-blocked-held",
  "type": "merge-gate",
  "title": "blocked verdict within the wait window holds the merge",
  "requires": ["python3"],
  "input": { "…": "type-specific replay input" },
  "compare": { "omit": ["outputs.path"] }
}
```

- `id` — matches `golden/<id>.expected.json`.
- `type` — one of the replayers above.
- `requires` — optional tool probes; a missing tool **skips** the case
  rather than failing it (keeps the suite green on minimal environments).
- `input` — fixture data handed to the replayer (issue payloads, PR +
  checks JSON, diff text, contract documents, env, pinned `now`).
- `compare.omit` — the tolerance mechanism: dotted output paths excluded
  from the diff for fields that are legitimately volatile (e.g. the
  runner-temp `outputs.path` emitted by `resolve.py`). Everything not
  omitted must match exactly — there is no fuzzy tolerance, so an
  unexpected change is always a failure.

The matching golden lives at `tests/regression/golden/<id>.expected.json`
and holds the full replayed output.

## Blessing / updating fixtures

Goldens are recorded behavior, not hand-written truth — bless after you
have verified the current output is correct:

```bash
node tests/regression_harness.js --bless          # re-record every case
node tests/regression_harness.js --bless ID ID2   # re-record specific cases
```

Workflow for an intentional behavior change:

1. Change the pipeline code (workflow script, classifier, adapter,
   resolver).
2. `node tests/regression_harness.js` — the affected cases fail with the
   diff between the old golden and the new behavior.
3. Verify each diff is the intended change.
4. `node tests/regression_harness.js --bless <ids>` to record it; review
   the golden diff in git like any other change.
5. Unrelated cases that also fail are an unplanned blast radius — fix the
   code, don't bless over them.

## Adding a case

1. Drop `cases/<id>.json` describing the input (mirror an existing case of
   the same `type`).
2. `node tests/regression_harness.js --bless <id>` — inspect the written
   golden to confirm the captured behavior is correct.
3. Commit case + golden together.

A case with no golden **fails** with a bless hint — committing a case
without its golden is a suite failure, not a silent pass.
