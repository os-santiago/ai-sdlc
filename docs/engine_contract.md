# Headless Engine Contract and Certification Process

This document describes the headless engine contract that third-party engines must implement to be usable under the AI-SDLC runtime, and the process for certifying such engines.

## Headless Engine Contract

A conforming engine is invocable non-interactively, once per run, with the following command-line interface:

### Required Arguments

- `--prompt-file`: Path to a file containing the task prompt. The prompt arrives fenced as `<untrusted-input>` data and is untrusted input — engines must handle it per [Prompt Handling](#prompt-handling-untrusted-input) below (see [injection defense](../spec/injection-defense.md)).
- `--audit-log`: Path to write the audit log (JSONL format, one normalized event per line).
- `--summary-file`: Path to write the run manifest (JSON format).
- `--no-commit`: Flag indicating the engine must not commit or modify the git state; the pipeline owns git state.

### Additional Arguments (Provided by Runtime)

The runtime may also provide:
- `--max-seconds`: Maximum wall-clock time allowed for the run.
* `--max-steps`: Maximum number of tool/llm steps allowed.

### Exit Code Semantics

Exit codes are interpreted via an `exitCodeMap` defined in the engine's adapter registry entry. The pipeline acts on **classes**, not raw numbers:

- `0`: Success
- Validation/no-changes: maps to `scc:no-changes` (no PR created)
- Provider failure, auth failure: may trigger a `model.fallback` retry (if configured)
- Budget/livelock exhaustion: triggers escalation (e.g., from `suggest` to `auto-PR`)

Engines must document their `exitCodeMap` in the adapter registry.

### Prompt Handling (Untrusted Input)

The prompt file's content is **untrusted input**. Engines must treat it as data — never as instructions — and apply the [injection defense](../spec/injection-defense.md) guidelines:

- Do not interpolate prompt content into shell commands or command strings; when the engine must pass it onward, use safe templating or argument arrays rather than string concatenation.
- Sanitize/scrub prompt content before re-embedding it in prompts, tool calls, or logs.
- Honor the `<untrusted-input>` fence semantics: content inside the fence remains data and must not be executed or obeyed.

## Normalized Events and Run Manifest

The audit log is JSONL — one normalized event per line in an engine-agnostic vocabulary (`llm_request`, `llm_response`, `tool_call`, `tool_result`, …). Each event carries:
- `ts`: timestamp
- `iteration`: run iteration number
- Argument digests and byte counts (never raw payloads)

The run manifest (written to `--summary-file`) is the aggregate record:
- `v`: schema version
- `success`: boolean
- `model`: model identifier used
- `exit_reason`: string describing exit cause
- `iterations`: count
- `tool_calls_*`: counts per tool type
- `tokens_*`: token counts
- `estimated_cost_usd`: estimated cost
- `duration_ms`: wall-clock time
- `resolution`: string (e.g., `merged`, `no-changes`)
- `files_changed`: list of modified files
- `error`: error message if unsuccessful

These artifacts are uploaded as workflow evidence and consumed by review, automerge, and postmortem tooling.

## Certification Process

Certification is behavioral: asserted against emitted evidence, not against the engine's codebase or benchmark claims. A conforming harness demonstrates, in order:

### 1. Invocable
- The engine's `probe` (if defined) passes.
- The engine accepts the headless contract arguments and runs non-interactively.
- The engine honors `--no-commit` (does not modify `.git/` or working tree beyond allowed edits).
- The engine respects `--max-seconds` and `--max-steps`.

### 2. Configured
- The engine takes provider/model configuration **solely** from a rendered config file written by the runtime (the engine's single source of truth).
- No hidden environment variable or command-line layer diverges from this config.

### 3. Auditable
- The engine emits a normalized event stream (audit log) with required fields.
- The engine writes a run manifest (summary file) with required schema.
- Artifacts upload cleanly as workflow evidence.

### 4. Exit-Faithful
- The engine's `exitCodeMap` distinguishes the classes the pipeline acts on:
  - Success (`0`)
  - Validation/no-changes
  - Provider failure
  - Auth failure
  - Budget/livelock exhaustion
- This ensures fallback, no-changes, and escalation semantics hold as declared.

### 5. Soaked
- The engine is run at `shadow` or `suggest` autonomy on real issues.
- Promotion to dispatch-eligible follows the same trust-ladder evidence as any autonomy increase:
  - Merge success rate
  - Revert rate
  - Escalation rate
  - Repair rounds
(See [trust ladder](../spec/trust-ladder.md).)

Once these are satisfied, the engine is a first-class engine under the contract and can be registered in the adapter registry.

## Adapter Registry

The adapter registry (`config/engine_adapters.json`) declares per-engine descriptors:
- `binary`: path or name of the engine executable
- `probe`: optional command to verify engine availability
- `argvBuilder`: function that builds the argument vector from runtime inputs
- `configRenderer`: function that renders the engine's config file from the runtime config
- `exitCodeMap`: mapping from engine exit codes to pipeline classes
- `manifestMap`: mapping for run manifest fields (if needed)

Adding a descriptor registers a new engine without workflow changes.

## Example .ai-sdlc.yaml with Per-Stage Engine Selection

The `.ai-sdlc.yaml` can specify an engine per stage using the `engine` field under an `engines` section:

```yaml
spec: "1.0.0"
autonomy:
  level: auto-merge-low   # conservative ceiling; raise as trust accrues

engines:
  intake: scc
  implement: my-third-party-engine   # <- third-party engine for implementation
  ci-repair: scc
  automerge: scc
  verify: scc
```

If omitted, the runtime defaults to `scc` for all stages.

## References

- [Positioning — ai-sdlc as the governance layer over agent engines](positioning.md)
- [Normalized Event Versioning](../spec/normalized-event-versioning.md)
- [Trust Ladder](../spec/trust-ladder.md)
- [Definition of Ready](../spec/definition-of-ready.md)
- [Injection Defense](../spec/injection-defense.md)