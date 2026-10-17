// tests/exit_code_contract.test.js — drift check for the scc exit-code
// contract (issue #133). spec/engine-exit-codes.md is the normative source:
// this suite parses its contract table and asserts the adapter registry
// (config/engine_adapters.json), the implement/ci-repair workflows, and the
// stall classifier (scripts/stall-classifier.js) all agree with it — so a
// renumbered engine code, a changed workflow gate, or a missing registry
// label fails loudly here instead of mis-classifying silently at runtime.
//
// Independence: expected values are derived from the spec document, never
// from the implementation under test; the classifier module is loaded only
// to check its coverage of registry labels (that coverage IS the contract).

'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const ROOT = path.join(__dirname, '..');
const SPEC_PATH = path.join(ROOT, 'spec', 'engine-exit-codes.md');
const REGISTRY_PATH = path.join(ROOT, 'config', 'engine_adapters.json');
const IMPLEMENT_YML = path.join(ROOT, '.github', 'workflows', 'ai-sdlc-implement.yml');
const CI_REPAIR_YML = path.join(ROOT, '.github', 'workflows', 'ai-sdlc-ci-repair.yml');
const RUNTIME_MD = path.join(ROOT, 'docs', 'runtime.md');
const SPEC_README = path.join(ROOT, 'spec', 'README.md');

const { CLASSES, LABEL_CLASS } = require('../scripts/stall-classifier');
const { EngineAdapterResolver } = require('../scripts/engine_adapter');

const STALL_VERDICTS = new Set([...CLASSES, 'timeout']);

// --- spec contract-table parsing ---------------------------------------------
// Rows: | `code` | engine outcome | `label` | `stall` | fallback | handling |
// The label cell is either `` `Label` `` (registry-declared) or
// `` `default` → `Label` `` (unmapped code resolved through `default`).

function firstBacktick(cell) {
  const m = cell.match(/`([^`]+)`/);
  return m ? m[1] : null;
}

function backticks(cell) {
  return [...cell.matchAll(/`([^`]+)`/g)].map((m) => m[1]);
}

function parseContractTable() {
  const doc = fs.readFileSync(SPEC_PATH, 'utf8');
  const m = doc.match(/<!--\s*contract-table\s*-->([\s\S]*?)<!--\s*\/contract-table\s*-->/);
  assert.ok(m, 'spec/engine-exit-codes.md must carry a <!-- contract-table --> block');
  const rows = [];
  for (const line of m[1].split('\n')) {
    const t = line.trim();
    if (!t.startsWith('|')) continue;
    if (/^\|[\s:|-]+\|?$/.test(t)) continue; // separator row
    const cells = t.split('|').slice(1, -1).map((c) => c.trim());
    if (cells.length < 6 || /^code$/i.test(firstBacktick(cells[0]) || cells[0])) continue; // header
    const code = firstBacktick(cells[0]);
    const labelParts = backticks(cells[2]);
    const row = {
      code,
      numeric: /^\d+$/.test(code) ? Number(code) : null,
      outcome: cells[1],
      stall: firstBacktick(cells[3]),
      fallback: cells[4].replace(/[^a-z]/gi, '').toLowerCase(),
    };
    if (/^default$/i.test(labelParts[0] || '')) {
      row.mapped = false;
      row.label = labelParts[1] || null;
    } else {
      row.mapped = true;
      row.label = labelParts[0] || null;
    }
    rows.push(row);
  }
  assert.ok(rows.length > 0, 'contract table must declare at least one code');
  return rows;
}

const DOC = fs.readFileSync(SPEC_PATH, 'utf8');
const ROWS = parseContractTable();
const CODE_ROWS = ROWS.filter((r) => r.numeric !== null);
const DEFAULT_ROW = ROWS.find((r) => /^default$/i.test(r.code));
const registry = JSON.parse(fs.readFileSync(REGISTRY_PATH, 'utf8')).scc;
const exitCodeMap = registry.exitCodeMap;
const stallClassMap = registry.stallClassMap || {};
const registryCodes = Object.keys(exitCodeMap).filter((k) => k !== 'default').map(Number);

// Resolved label for a code (mirrors classifyExitCode semantics: declared
// label else `default`) — used to derive the expected stall class.
function resolvedLabel(code) {
  return Object.prototype.hasOwnProperty.call(exitCodeMap, String(code))
    ? exitCodeMap[String(code)]
    : exitCodeMap.default;
}

test('contract table is well-formed', () => {
  const seen = new Set();
  for (const row of ROWS) {
    assert.ok(row.code, 'every row names a code');
    assert.ok(!seen.has(row.code), `duplicate contract row for ${row.code}`);
    seen.add(row.code);
    assert.ok(row.label, `row ${row.code} must name a label`);
    assert.ok(STALL_VERDICTS.has(row.stall),
      `row ${row.code} stall class "${row.stall}" must be a stall class or the timeout pseudo-class`);
    assert.match(row.fallback, /^(yes|no)$/, `row ${row.code} fallback cell must be yes/no`);
  }
  assert.ok(DEFAULT_ROW, 'the contract must declare a `default` row for undeclared codes');
  assert.equal(DEFAULT_ROW.mapped, true, 'the `default` row declares its label directly');
});

test('registry exitCodeMap covers exactly the explicitly-mapped contract codes', () => {
  const mappedInDoc = CODE_ROWS.filter((r) => r.mapped).map((r) => r.numeric).sort((a, b) => a - b);
  assert.deepEqual([...registryCodes].sort((a, b) => a - b), mappedInDoc,
    'scc.exitCodeMap keys must equal the set of codes the spec maps explicitly');
  for (const row of CODE_ROWS) {
    if (row.mapped) {
      assert.equal(exitCodeMap[String(row.numeric)], row.label,
        `exitCodeMap[${row.numeric}] must be "${row.label}" per the spec`);
    } else {
      assert.ok(!Object.prototype.hasOwnProperty.call(exitCodeMap, String(row.numeric)),
        `code ${row.numeric} is documented as unmapped (default → ${row.label}) — remove the explicit key or update the spec`);
      assert.equal(resolvedLabel(row.numeric), row.label,
        `code ${row.numeric} must resolve through default to "${row.label}"`);
    }
  }
  assert.equal(exitCodeMap.default, DEFAULT_ROW.label,
    `exitCodeMap.default must be "${DEFAULT_ROW.label}" per the spec`);
});

test('every documented stall class matches the classifier resolution', () => {
  for (const row of CODE_ROWS) {
    const expected = Object.prototype.hasOwnProperty.call(stallClassMap, String(row.numeric))
      ? stallClassMap[String(row.numeric)]
      : LABEL_CLASS[resolvedLabel(row.numeric)];
    assert.equal(row.stall, expected,
      `code ${row.numeric}: spec documents stall class "${row.stall}" but the registry/classifier resolves "${expected}"`);
  }
  assert.equal(DEFAULT_ROW.stall, LABEL_CLASS[exitCodeMap.default],
    `default: spec documents "${DEFAULT_ROW.stall}" but LABEL_CLASS[default] resolves "${LABEL_CLASS[exitCodeMap.default]}"`);
});

test('every registry label has a classifier base class', () => {
  for (const [code, label] of Object.entries(exitCodeMap)) {
    assert.ok(label in LABEL_CLASS,
      `exitCodeMap["${code}"] = "${label}" has no LABEL_CLASS entry in scripts/stall-classifier.js`);
  }
});

test('every stallClassMap entry is a documented code with a valid verdict', () => {
  const docCodes = new Set(CODE_ROWS.map((r) => String(r.numeric)));
  for (const [code, verdict] of Object.entries(stallClassMap)) {
    assert.ok(docCodes.has(code),
      `stallClassMap["${code}"] is not a documented contract code — update spec/engine-exit-codes.md`);
    assert.ok(STALL_VERDICTS.has(verdict),
      `stallClassMap["${code}"] = "${verdict}" is not a stall class or the timeout pseudo-class`);
  }
});

test('scc dispatch surfaces raw exit codes', () => {
  assert.equal(registry.dispatch && registry.dispatch.rawExitCodes, true,
    'the contract branches on the raw taxonomy — dispatch.rawExitCodes must stay true');
});

test('resolved adapter agrees with the documented contract', () => {
  const adapter = new EngineAdapterResolver(REGISTRY_PATH).getEngineAdapter('scc');
  for (const row of CODE_ROWS) {
    assert.equal(adapter.classifyExitCode(row.numeric), row.label,
      `classifyExitCode(${row.numeric}) must resolve "${row.label}"`);
    // Only the success class normalizes to 0.
    const successClass = exitCodeMap['0'];
    assert.equal(adapter.mapExitCode(row.numeric), row.label === successClass ? 0 : 1,
      `mapExitCode(${row.numeric}) must be 0 only for the success class`);
  }
});

test('implement workflow fallback gate equals the documented retry set', () => {
  const yml = fs.readFileSync(IMPLEMENT_YML, 'utf8');
  const documented = CODE_ROWS.filter((r) => r.fallback === 'yes').map((r) => r.numeric).sort((a, b) => a - b);
  assert.ok(documented.length > 0, 'the spec must declare the fallback-eligible codes');

  // The raw-code arm of the gate: [ "$primary_exit" = N ] comparisons.
  const gateCodes = [...yml.matchAll(/\[ "\$primary_exit" = (\d+) \]/g)].map((m) => Number(m[1]));
  assert.deepEqual([...new Set(gateCodes)].sort((a, b) => a - b), documented,
    'the raw-code fallback gate in ai-sdlc-implement.yml must equal the documented retry set');

  // The classifier arm is the documented extension: transport verdict only.
  assert.match(yml, /\[ "\$primary_class" = "transport" \]/,
    'the fallback gate must keep the transport-verdict arm');

  // The reason-mapping case arms name only retry-eligible codes (plus `*`).
  const caseMatch = yml.match(/case "\$primary_exit" in([\s\S]*?)esac/);
  assert.ok(caseMatch, 'fallback-reason case on $primary_exit must exist');
  const arms = [...caseMatch[1].matchAll(/^\s*(\d+|\*)\)/gm)].map((m) => m[1]);
  for (const arm of arms.filter((a) => a !== '*')) {
    assert.ok(documented.includes(Number(arm)),
      `case arm ${arm}) is not a documented fallback-eligible code`);
  }
});

test('implement outcome labeler keys only on documented codes', () => {
  const yml = fs.readFileSync(IMPLEMENT_YML, 'utf8');
  const docCodes = new Set(CODE_ROWS.map((r) => r.numeric));
  const caseMatch = yml.match(/case "\$CODE" in([\s\S]*?)esac/);
  assert.ok(caseMatch, 'outcome labeler must case on $CODE');
  const arms = [...caseMatch[1].matchAll(/^\s*(\d+|\*)\)/gm)].map((m) => m[1]);
  assert.ok(arms.includes('0'), 'labeler must arm on 0 → publish/no-changes gate');
  assert.ok(arms.includes('10'), 'labeler must arm on 10 → no-changes refinement hold');
  assert.ok(arms.includes('*'), 'labeler must keep the wildcard scc:failed arm');
  for (const arm of arms.filter((a) => a !== '*')) {
    assert.ok(docCodes.has(Number(arm)), `labeler arm ${arm}) is not a documented contract code`);
  }
  // Double provider/auth fault escalation names the same retry set.
  const retrySet = new Set(CODE_ROWS.filter((r) => r.fallback === 'yes').map((r) => r.numeric));
  for (const m of yml.matchAll(/\[ "\$CODE" = (\d+) \]/g)) {
    assert.ok(retrySet.has(Number(m[1])),
      `post-fallback escalation on $CODE = ${m[1]} must stay inside the documented retry set`);
  }
  // The publish gate keys on exit_code == 0.
  assert.match(yml, /steps\.engine\.outputs\.exit_code == '0'/,
    'the apply/publish gate must key on exit_code == 0');
});

test('ci-repair ledger keys only on documented codes', () => {
  // Issue #127: the outcome mapping moved out of the workflow heredoc into
  // scripts/ci-repair-record.js — pin the shipped module, not a copy.
  const yml = fs.readFileSync(path.join(ROOT, 'scripts', 'ci-repair-record.js'), 'utf8');
  const docCodes = new Set(CODE_ROWS.map((r) => r.numeric));
  const codes = [...yml.matchAll(/code === '(\d+)'/g)].map((m) => Number(m[1]));
  assert.deepEqual([...new Set(codes)].sort((a, b) => a - b), [0, 10],
    'ci-repair outcome ledger must branch on exactly {0, 10}');
  for (const code of codes) assert.ok(docCodes.has(code), `ci-repair branch on ${code} is undocumented`);
});

test('the spec keeps the upstream emission-contract links (AC: engine coverage linked)', () => {
  assert.match(DOC, /os-santiago\/sc-agent-cli[^\s)]*docs\/exit-codes\.md/,
    'the spec must link the engine-side normative emission contract');
  assert.match(DOC, /test\/e2e\/chat-exit-codes\.test\.ts/,
    'the spec must name the engine e2e suite that asserts every code');
});

test('operator docs stay linked to the contract', () => {
  const runtime = fs.readFileSync(RUNTIME_MD, 'utf8');
  assert.match(runtime, /sc-agent-cli[^\s)]*docs\/exit-codes\.md/,
    'docs/runtime.md must link the engine emission contract');
  assert.match(runtime, /spec\/engine-exit-codes\.md|engine-exit-codes\.md/,
    'docs/runtime.md must link the normative consumption spec');
  assert.match(fs.readFileSync(SPEC_README, 'utf8'), /engine-exit-codes\.md/,
    'spec/README.md must index the contract document');
});
