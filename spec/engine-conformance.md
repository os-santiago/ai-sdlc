# Engine Conformance — certifying the headless contract (spec)

The normative definition of what it means for an engine to conform to the
headless contract: the adapter-registry descriptor contract, the resolved
adapter contract surface, the conformance levels, and the certification
procedure. The executable statement of this spec is the conformance
harness — [`tests/engine_conformance_harness.js`](../tests/engine_conformance_harness.js)
with its `node --test` wrapper
[`tests/engine_conformance.test.js`](../tests/engine_conformance.test.js) —
executed against the canonical adapter registry
[`config/engine_adapters.json`](../config/engine_adapters.json) through the
canonical interpreter
[`scripts/engine_adapter.js`](../scripts/engine_adapter.js).

Tracking: [#183](https://github.com/os-santiago/ai-sdlc/issues/183) ·
epic [#152](https://github.com/os-santiago/ai-sdlc/issues/152) · harness
[#217](https://github.com/os-santiago/ai-sdlc/issues/217). The behavioral
contract the suite certifies lives in
[`docs/engine_contract.md`](../docs/engine_contract.md); this document
defines the conformance layer over it.

The key words **MUST**, **MUST NOT**, **SHOULD**, and **MAY** are to be
interpreted as described in RFC 2119.

## Terms

| Term | Meaning |
|---|---|
| engine | A headless agent implementation invocable once per run (CLI or third-party harness). |
| engine id | The registry key naming an engine (e.g. `scc`, `devin`). |
| adapter descriptor | The declarative per-engine entry in the adapter registry. |
| adapter registry | `config/engine_adapters.json` — the single canonical adapter contract. Pure data: parsed as JSON, never evaluated. |
| adapter | The object resolved from a descriptor by `EngineAdapterResolver.getEngineAdapter()`, exposing the contract surface. |
| canonical interpreter | `scripts/engine_adapter.js` — the one resolver both runtime consumers bind to (container worker directly, GHA path via `scripts/gha/engine-adapter-resolver.js`). |
| conformance harness | `tests/engine_conformance_harness.js` — the executable certification suite. |
| certified adapter id | An engine id listed in the schema's `$defs.engine_id` enum — the certification list the contract validates against. |

Three distinct gates exist and MUST NOT be conflated:

1. **Contract certification** — the engine id is admitted to
   `$defs.engine_id` in [`ai-sdlc.schema.json`](ai-sdlc.schema.json). The
   contract certifies *names*.
2. **Registry conformance** — the descriptor satisfies the declarative
   contract below and passes the conformance suite. The registry
   *implements* names. A certified id still needs a registry entry before
   a stage can dispatch to it.
3. **Behavioral certification** — the running engine demonstrates the
   five-stage ladder in [`docs/engine_contract.md`](../docs/engine_contract.md)
   (Invocable → Configured → Auditable → Exit-faithful → Soaked), with
   dispatch-eligibility promoted on trust-ladder evidence
   ([`trust-ladder.md`](trust-ladder.md)).

This spec governs gates 1 and 2 — the machine-checkable surface. Gate 3 is
defined by `docs/engine_contract.md` and is outside the harness's scope.

## The adapter descriptor contract

Every descriptor in the registry MUST satisfy the declarative contract
below. The interpreter validates each descriptor at load time, so a
malformed descriptor fails the `registry-loads` check — loudly, never
mid-dispatch.

| Field | Requirement |
|---|---|
| `binary` | REQUIRED. Non-empty string — the engine executable. |
| `probe` | OPTIONAL. Non-empty array of argv strings; `probe[0]` MUST invoke the engine binary. |
| `capabilities` | OPTIONAL. Object of declared capability flags. |
| `argvBuilder` | REQUIRED. Declarative argument-vector spec — see below. |
| `configRenderer` | OPTIONAL. Object — declarative descriptor for the engine's rendered config file. |
| `exitCodeMap` | REQUIRED. Engine exit code → pipeline classification — see below. |
| `manifestMap` | OPTIONAL. Object — declarative run-manifest location/format descriptor (`path`, `format`). |

The descriptor is data, never code: serialized functions MUST NOT be
stored or evaluated anywhere in the registry.

### The `argvBuilder` substitution contract

`argvBuilder` declares how the runtime renders a headless invocation
without embedding engine-specific code:

- `args` — REQUIRED non-empty array of strings forming the argument
  vector. `command` — OPTIONAL non-empty literal string prepended as
  `argv[0]`; it MUST NOT contain `{` or `}`.
- `substitution.mode` MUST be `"argv-element"` and `substitution.shell`
  MUST be `"never"`. Substitution is atomic per argv element — a value
  containing spaces lands as exactly one element. There is no
  re-splitting, no globbing, no shell joining, ever.
- Placeholders are whole-element tokens matching
  `^\{[A-Za-z][A-Za-z0-9]*\}$` (e.g. `{prompt}`, `{manifestFile}`). An arg
  containing `{` or `}` that is not a whole-element placeholder is
  invalid — placeholder substrings MUST NOT be embedded inside larger
  elements.
- `substitution.placeholders` is REQUIRED. Every declared placeholder
  MUST appear in `args`; every placeholder used in `args` MUST be
  declared. The sets are in 1:1 correspondence.
- Each placeholder MUST declare `pattern` — a string that compiles as a
  regular expression — and MAY declare `required` (boolean, default
  `true`).

### Value resolution rules

`buildArgv(values)` resolves each placeholder, in order:

1. An explicit `values[name]` wins when present and non-null.
2. Otherwise the `{ENGINE}_{PLACEHOLDER}` environment variable supplies
   the value — the engine id uppercased with non-alphanumerics as `_`,
   joined to the placeholder name uppercased with camelCase split into
   SNAKE (`devin` + `manifestFile` → `DEVIN_MANIFEST_FILE`).
3. A required placeholder with no resolved value MUST throw `Missing
   required value` naming both the `{placeholder}` and its
   `{ENGINE}_{PLACEHOLDER}` fallback.
4. An optional placeholder with no resolved value drops its element —
   together with a literal flag element emitted immediately before it.
5. A resolved value that fails the declared `pattern` MUST be rejected
   (`Invalid value for placeholder`), never coerced — and the rejection
   MUST NOT echo the rejected value, which may carry secrets or prompt
   payload.
6. Values for undeclared keys MUST be ignored, never substituted.

The built argv MUST be deterministic: identical inputs produce the
identical vector, with every substituted value appearing exactly once as
a whole element.

### The `exitCodeMap` contract

The pipeline acts on classification **classes, not raw numbers**. Every
descriptor MUST declare `exitCodeMap` such that:

- exit code `"0"` is classified — its class is the engine's *success
  class*;
- a string `"default"` classification is declared, so unknown codes never
  resolve to `undefined`;
- every key other than `default` is a decimal exit code; every value is
  a classification string.

The declared map SHOULD distinguish the classes the pipeline acts on —
success, validation/no-changes, provider failure, auth failure, and
budget/livelock exhaustion — so fallback, no-changes, and escalation
semantics hold as declared (`docs/engine_contract.md`, §Exit Code
Semantics).

## The resolved adapter contract surface

`getEngineAdapter(engineId)` MUST return an object carrying the
descriptor's declarative fields — `name` (equal to the engine id),
`binary`, `probe`, `capabilities`, `argvBuilder`, `configRenderer`,
`exitCodeMap`, `manifestMap` — and these methods:

| Method | Contract |
|---|---|
| `getBinary()` | Returns the `{ENGINE}_BINARY` env override trimmed of whitespace, else the registry `binary`. |
| `buildArgv(values)` | Renders the argv per the substitution contract above. |
| `probeCapabilities()` | Returns a fresh copy of the registry-declared `capabilities` — mutations MUST NOT leak back into the adapter or registry. |
| `classifyExitCode(code)` | Returns `exitCodeMap[String(code)]` when declared, else `exitCodeMap.default`; numeric and string codes classify identically. |
| `mapExitCode(code)` | Normalizes to `0` iff `classifyExitCode(code)` equals the engine's success class (the class declared for `"0"`), else `1`. |
| `mapManifest(manifest)` | Passthrough — `manifestMap` is declarative location/format metadata, not a transform. |

Additionally, resolution MUST be isolated: the returned adapter carries
its own copy of the descriptor, so mutating it can never corrupt the
shared registry.

Resolution MUST fail loudly for unknown engine ids — `hasEngine` returns
`false` and `getEngineAdapter` throws `Unknown engine ID`. Silent
fallback would run an unintended engine contract and is prohibited.

## The conformance suite

### Canonical implementation and invocation

```bash
node tests/engine_conformance_harness.js [--registry PATH]
    [--engine ID] [--live] [--report PATH] [--help]
make test-conformance
```

`node --test` runs the same suite through
`tests/engine_conformance.test.js`, which `tools/validate-repo.sh` picks
up — hence the contract job in `.github/workflows/ai-sdlc.yml` enforces
conformance on every change to main.

The harness is **registry-driven**: it subjects every descriptor in the
registry to the full check catalog. Registering a new engine
automatically extends certification coverage — coverage grows with the
registry, not with the harness source. `--engine ID` (repeatable)
restricts a run to selected adapters; `--registry PATH` certifies an
alternate registry document without touching the canonical one, so the
suite is engine-implementation-agnostic — a third-party harness that can
be described by a descriptor can be certified by the suite, not by
reading prose.

### Independence requirement

A conformance suite MUST derive its expectations independently of the
implementation under test: the harness re-derives its helpers (env-name
derivation, placeholder detection, value synthesis) rather than reusing
resolver internals for expected values. Asserting the implementation
against itself would certify nothing.

### Value synthesis

To build a valid argv without knowing a placeholder's semantics, the
suite synthesizes values from a candidate corpus — known-good values per
common placeholder name, then generic candidates — matched against each
placeholder's own declared pattern. A pattern narrow enough to reject the
whole corpus MUST fail conformance loudly; the remedy is to extend the
harness corpus, never to weaken the check. Hostile candidates (shell
metacharacters, embedded flags, NUL/control bytes, newlines) drive the
rejection checks; spaced candidates drive the atomicity checks.

### Check catalog

Each check asserts one contract clause and reports `pass`, `fail`, or
`skip` (recorded with a reason; a skip never counts as failure — it marks
a clause genuinely inapplicable to that descriptor).

Global checks (once per run):

| Check id | Clause |
|---|---|
| `registry-loads` | Registry parses and declares at least one engine. |
| `unknown-engine-rejected` | Unregistered ids fail loudly — `hasEngine` false, `getEngineAdapter` throws `Unknown engine ID`. |
| `engine-selection` | Every `--engine` selection resolves to a registered adapter. |

Per-adapter checks (every registered engine):

| Check id | Clause |
|---|---|
| `adapter-resolves` | Descriptor resolves to an adapter exposing the full contract surface, reflecting the registry descriptor. |
| `binary-resolution` | `getBinary()` returns the registry binary and honors `{ENGINE}_BINARY`, trimmed. |
| `argv-builds` | `buildArgv()` substitutes every placeholder into a well-formed argv — `command` first when declared, no unsubstituted elements, each value exactly once. |
| `argv-atomic-elements` | Spaced values remain one argv element — no re-splitting. |
| `argv-deterministic` | Identical inputs produce the identical argv. |
| `argv-required-enforced` | Missing required placeholders throw `Missing required value`, naming the placeholder and its env fallback. |
| `argv-invalid-rejected` | Pattern-failing values throw `Invalid value for placeholder` without echoing the rejected value. |
| `argv-unknown-inputs-ignored` | Undeclared value keys never alter the built argv. |
| `argv-env-fallback` | `{ENGINE}_{PLACEHOLDER}` env vars supply values; explicit values win. |
| `exitcode-classification` | `classifyExitCode()` honors every declared code (numeric and string); unmapped codes classify as `default`. |
| `exitcode-normalization` | `mapExitCode()` is `0` only for the declared success class, `1` otherwise. |
| `capabilities-contract` | `probeCapabilities()` returns a fresh copy of the declared flags — mutation-proof. |
| `manifest-contract` | `mapManifest()` passes the manifest through unchanged; `manifestMap`/`configRenderer` stay declarative. |
| `probe-declaration` | A declared probe is non-empty argv whose first element invokes the engine binary. |
| `probe-live` | The declared probe executes successfully against the installed engine — `--live` only. |
| `adapter-isolation` | Mutating a resolved adapter cannot corrupt the shared registry. |

### Verdicts and reporting

- The run verdict is **CONFORMING** when every check passes or skips, and
  **NON-CONFORMING** on any failure — including an unloadable registry or
  an unregistered `--engine` selection.
- Process exit status: `0` = all adapters conform; `1` = any conformance
  failure; `2` = CLI misuse.
- The human-readable report prints one `[PASS]`/`[FAIL]`/`[SKIP]` line
  per check as `<engine-id>/<check-id>: <clause> — <detail>`, then a
  summary line — a failing engine gets a readable report of exactly which
  contract clauses failed.
- `--report PATH` MUST write the machine-readable JSON artifact:
  `{ ok, registryPath, live, global[], engines[], summary }` where each
  engine entry carries `engineId`, `ok`, and its per-check results, and
  `summary` carries `engines`, `checks`, `passed`, `failed`, `skipped`.

## Conformance levels

Conformance is asserted in levels of increasing strength:

| Level | Invocation | Certifies |
|---|---|---|
| **Static** (default) | `node tests/engine_conformance_harness.js` | The registry-declared contract surface — resolution, binary override, the full substitution contract, exit-code classification and normalization, capabilities, manifest passthrough, probe declaration, adapter isolation — without executing the engine. `probe-live` reports `skip — requires --live`. |
| **Live** | `… --live` | Static, plus execution of each declared `probe` argv against the installed binary (honoring `{ENGINE}_BINARY`); the probe MUST exit `0`. |
| **Behavioral** | outside the harness | The five-stage ladder in `docs/engine_contract.md` — real runs at `shadow`/`suggest` autonomy producing emitted-evidence proof (audit log, run manifest, `--no-commit`, budget adherence), promoted on trust-ladder metrics. |

Static conformance is the REQUIRED minimum for every registered adapter —
it runs unconditionally in CI. Live conformance SHOULD be run before an
engine is dispatch-admitted, since it proves the declared probe actually
invokes a working binary. Neither substitutes for behavioral
certification: a green suite certifies the declared contract surface, not
run outcomes.

## Certifying a new engine

The registration procedure, in order:

1. **Author the descriptor.** Add the engine's entry to
   `config/engine_adapters.json` satisfying the descriptor contract
   above. Load-time validation is the first gate — a malformed
   descriptor fails `registry-loads`.
2. **Pass static conformance.** `make test-conformance` (or `node
   tests/engine_conformance_harness.js`) reports `CONFORMING` with no
   failures; skips only where a clause is genuinely inapplicable.
3. **Pass live conformance.** With the binary installed, `--live` proves
   the declared probe exits `0`.
4. **Certify the name.** Add the engine id to the `$defs.engine_id` enum
   in `ai-sdlc.schema.json` — a spec-visible, semver-sensitive change.
   Contract validation then accepts the id for `engine.name` and the
   `engines` stage map; the registry entry makes it dispatchable.
5. **Earn behavioral certification.** Run the engine at
   `shadow`/`suggest` autonomy on real issues and promote per the
   trust-ladder evidence, per `docs/engine_contract.md`.

A descriptor that regresses conformance fails the contract job in CI —
fix the descriptor or remove it. A check MUST NOT be weakened to admit an
engine; when a pattern is too narrow for the value corpus, the corpus is
what extends.

## Non-goals

- **Certifying engine quality.** Conformance asserts the contract
  surface, not diff quality or model capability — the trust ladder judges
  outcomes.
- **Executing full issue runs.** A fixed task corpus with assertions on
  emitted artifacts (audit-log schema, summary schema, `--no-commit` on a
  live tree) is the behavioral level of `docs/engine_contract.md`, not
  this suite.
- **Reaching into engine internals.** The suite is implementation-blind:
  it certifies the registry-declared surface any conforming harness
  exposes, CLI or otherwise.
- **Mandating an engine.** Registration is additive; nothing here
  privileges one engine over another.

## References

- [`docs/engine_contract.md`](../docs/engine_contract.md) — the headless
  contract and behavioral certification ladder
- [`scripts/engine_adapter.js`](../scripts/engine_adapter.js) — canonical
  interpreter
- [`config/engine_adapters.json`](../config/engine_adapters.json) — the
  adapter registry
- [`tests/engine_conformance_harness.js`](../tests/engine_conformance_harness.js)
  · [`tests/engine_conformance.test.js`](../tests/engine_conformance.test.js)
  — the executable statement of this spec
- [`interop-posture.md`](interop-posture.md) — the seam this suite certifies
- [`trust-ladder.md`](trust-ladder.md) — promotion evidence for
  dispatch-eligibility
- [`injection-defense.md`](injection-defense.md) — why rejection errors
  never echo candidate values
