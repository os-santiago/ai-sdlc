#!/usr/bin/env node
// tests/regression_harness.js — golden-fixture regression harness (issue #193)
//
// Replays known pipeline behaviors against checked-in fixtures and fails
// when the shipped code drifts from the recorded golden output. Modeled on
// the Hermes eval-suite pattern (golden fixtures + replay + diff report):
// every case replays the real decision code — never a copy of it — so a
// behavior change anywhere in the pipeline surface lands as a readable
// diff here.
//
// Case types (the deterministic decision surfaces of the pipeline):
//   intake-dor        DoR (R1–R6) intake verdict — replays the inline
//                     `node - <<'NODE'` script of the "Validate DoR" step in
//                     .github/workflows/ai-sdlc-intake.yml verbatim, in a
//                     sandboxed fs/env/process.
//   merge-gate        merge-gate decision table — replays the inline node
//                     script of the "Evaluate merge gate" step in
//                     .github/workflows/ai-sdlc-automerge.yml, with the
//                     fetched pr/checks/comment JSONs and Date.now() fixed
//                     by the fixture.
//   implement-sanitize  sanitize + DoR-surface check — replays the inline
//                     node script of the "Fetch issue → sanitize → prompt
//                     file" step in .github/workflows/ai-sdlc-implement.yml,
//                     with issue.json + trusted-issue-comments.json as
//                     fixtures (issue #122 drift verdicts + the fenced
//                     prompt it writes).
//   risk-classify     scripts/risk-classifier.js classifyDiff() — the
//                     pr:risk-* taxonomy assignment.
//   label-ops         scripts/risk-classifier.js applyRiskLabel() against a
//                     recording `gh` stub — the label-mutation decision.
//   engine-adapter    scripts/engine_adapter.js — adapter resolution:
//                     argv building, exit-code mapping, capabilities,
//                     binary override.
//   contract-resolve  .github/actions/load-config/resolve.py — contract
//                     resolution + flat step outputs (requires python3).
//   normalized-event  .github/actions/github-to-normalized-event —
//                     webhook → normalized event conversion.
//
// CLI:
//   node tests/regression_harness.js [--case ID ...] [--json]
//   node tests/regression_harness.js --bless [ID ...]   (re)write goldens
//   node tests/regression_harness.js --report FILE      diff artifact path
//   node tests/regression_harness.js --cases-dir D --golden-dir D
//
// Exit: 0 when every replayed case matches its golden (skips tolerated),
// 1 on any diff/missing golden/replay error, 2 on usage/tooling errors.
// `node --test` runs the same suite through tests/regression.test.js.
'use strict';

const fs = require('fs');
const os = require('os');
const path = require('path');
const { spawnSync } = require('child_process');

const ROOT = path.join(__dirname, '..');
const CASES_DIR = path.join(__dirname, 'regression', 'cases');
const GOLDEN_DIR = path.join(__dirname, 'regression', 'golden');
const RESULTS_DIR = path.join(__dirname, 'regression', 'results');

const INTAKE_WORKFLOW = path.join(ROOT, '.github', 'workflows', 'ai-sdlc-intake.yml');
const AUTOMERGE_WORKFLOW = path.join(ROOT, '.github', 'workflows', 'ai-sdlc-automerge.yml');
const IMPLEMENT_WORKFLOW = path.join(ROOT, '.github', 'workflows', 'ai-sdlc-implement.yml');
const RESOLVE_PY = path.join(ROOT, '.github', 'actions', 'load-config', 'resolve.py');
const NORMALIZED_EVENT = path.join(ROOT, '.github', 'actions', 'github-to-normalized-event', 'index.js');

const { classifyDiff, applyRiskLabel } = require('../scripts/risk-classifier');
const { getEngineAdapter } = require('../scripts/engine_adapter');

// --- Tool probes --------------------------------------------------------------

const TOOL_PROBES = {
  python3: () => spawnSync('python3', ['--version'], { stdio: 'ignore' }).status === 0,
};

function missingRequirements(caze) {
  return (caze.requires || []).filter((name) => {
    const probe = TOOL_PROBES[name];
    return probe ? !probe() : true; // unknown requirement → treat as missing
  });
}

// --- Inline-script extraction + sandboxed replay ------------------------------

// Pull the `node - <<'NODE' … NODE` script out of a workflow run block.
// Extraction is deliberately textual: the fixture suite replays the exact
// code the runner executes. `mustContain` pins the *identity* of the script
// so a workflow restructure fails loudly ("the DoR evaluator moved") instead
// of silently replaying the wrong block — that failure is itself the
// regression signal.
function extractNodeHeredoc(workflowPath, { index = 0, mustContain } = {}) {
  const text = fs.readFileSync(workflowPath, 'utf8');
  const blocks = [];
  const re = /node - <<'NODE'\r?\n([\s\S]*?)\r?\n[ \t]*NODE[ \t]*\r?\n/g;
  let m;
  while ((m = re.exec(text)) !== null) blocks.push(m[1]);
  if (!blocks.length) {
    throw new Error(`${path.basename(workflowPath)}: no \`node - <<'NODE'\` inline script found — the step was restructured or moved`);
  }
  const src = blocks[index];
  if (src === undefined) {
    throw new Error(`${path.basename(workflowPath)}: inline node script #${index} not found (${blocks.length} block(s) present)`);
  }
  if (mustContain && !src.includes(mustContain)) {
    throw new Error(`${path.basename(workflowPath)}: extracted node script no longer contains marker ${JSON.stringify(mustContain)} — the decision code moved or was rewritten; review the drift and update the harness marker`);
  }
  return src;
}

class SandboxExit extends Error {
  constructor(code) {
    super(`process.exit(${code})`);
    this.code = code;
  }
}

// `new Date()`/`Date.now()` pinned to a fixture timestamp so aging decisions
// (pending timeouts, review-wait caps) replay deterministically.
function makeFixedDate(nowMs) {
  const Real = Date;
  function FixedDate(...args) {
    return args.length === 0 ? new Real(nowMs) : new Real(...args);
  }
  FixedDate.now = () => nowMs;
  FixedDate.parse = Real.parse.bind(Real);
  FixedDate.UTC = Real.UTC.bind(Real);
  FixedDate.prototype = Real.prototype;
  return FixedDate;
}

// GITHUB_OUTPUT format: `k=v` lines and `k<<DELIM\nv\nDELIM` blocks.
function parseGithubOutput(text) {
  const out = {};
  const lines = String(text || '').split('\n');
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    if (!line) continue;
    const dm = /^([A-Za-z_][A-Za-z0-9_]*)<<(.+)$/.exec(line);
    if (dm) {
      const key = dm[1];
      const delim = dm[2];
      const buf = [];
      for (i += 1; i < lines.length && lines[i] !== delim; i += 1) buf.push(lines[i]);
      out[key] = buf.join('\n');
      continue;
    }
    const em = /^([^=\s]+)=(.*)$/.exec(line);
    if (em) out[em[1]] = em[2];
  }
  return out;
}

/**
 * Replay an extracted inline node script with a virtual filesystem and a
 * captured GITHUB_OUTPUT. `files` maps the literal paths the script reads
 * ('issue.json', 'pr.json', …) to their fixture contents; writes land in
 * `written`. `env` replaces process.env wholesale — the sandbox never sees
 * ambient runner state. Returns {outputs, written, log, exitCode, error}.
 */
function replayInlineScript(src, { files = {}, env = {}, nowMs } = {}) {
  const written = {};
  const log = [];
  const fsStub = {
    readFileSync(p) {
      const key = String(p);
      if (Object.prototype.hasOwnProperty.call(files, key)) return files[key];
      const e = new Error(`ENOENT: regression sandbox has no fixture for '${key}'`);
      e.code = 'ENOENT';
      throw e;
    },
    appendFileSync(p, s) {
      const k = String(p);
      written[k] = (written[k] || '') + String(s);
    },
    writeFileSync(p, s) {
      written[String(p)] = String(s);
    },
    existsSync(p) {
      return Object.prototype.hasOwnProperty.call(files, String(p))
        || Object.prototype.hasOwnProperty.call(written, String(p));
    },
  };
  const proc = Object.create(process);
  proc.env = env;
  proc.exit = (code) => { throw new SandboxExit(code ?? 0); };
  const push = (...a) => log.push(a.map(String).join(' '));
  const consoleStub = { log: push, info: push, warn: push, error: push };
  const req = (m) => (m === 'fs' ? fsStub : require(m));

  let exitCode = null;
  let error = null;
  try {
    // eslint-disable-next-line no-new-func
    new Function('require', 'process', 'console', 'Date', 'module', 'exports', src)(
      req, proc, consoleStub, nowMs === undefined ? Date : makeFixedDate(nowMs),
      { exports: {} }, {});
    exitCode = 0;
  } catch (e) {
    if (e instanceof SandboxExit) exitCode = e.code;
    else { error = `${e.name}: ${e.message}`; exitCode = null; }
  }
  const outPath = env.GITHUB_OUTPUT || '__github_output__';
  return { outputs: parseGithubOutput(written[outPath]), written, log, exitCode, error };
}

// --- Diff ---------------------------------------------------------------------

const ABSENT = '<absent>';
const MAX_DIFFS = 100;

function shortJson(v) {
  if (v === undefined) return ABSENT;
  let s;
  try { s = JSON.stringify(v); } catch { s = String(v); }
  if (s === undefined) s = String(v);
  return s.length > 160 ? `${s.slice(0, 157)}…` : s;
}

// Recursive structural diff → [{path, expected, actual}] with formatted
// values. Bounded by MAX_DIFFS so a wholesale restructure reports the first
// divergences readably instead of a wall.
function diffObjects(expected, actual, base = '', diffs = []) {
  if (diffs.length >= MAX_DIFFS) return diffs;
  const isObj = (v) => v !== null && typeof v === 'object' && !Array.isArray(v);
  if (Object.is(expected, actual)) return diffs;
  if (Array.isArray(expected) && Array.isArray(actual)) {
    const n = Math.max(expected.length, actual.length);
    for (let i = 0; i < n && diffs.length < MAX_DIFFS; i += 1) {
      diffObjects(expected[i], actual[i], `${base}[${i}]`, diffs);
    }
    return diffs;
  }
  if (isObj(expected) && isObj(actual)) {
    const keys = [...new Set([...Object.keys(expected), ...Object.keys(actual)])].sort();
    for (const k of keys) {
      diffObjects(expected[k], actual[k], base ? `${base}.${k}` : k, diffs);
    }
    return diffs;
  }
  diffs.push({ path: base || '(root)', expected: shortJson(expected), actual: shortJson(actual) });
  return diffs;
}

// `compare.omit` dotted paths are the tolerance mechanism — volatile fields
// (tmp paths, timestamps) a fixture legitimately cannot pin.
function omitPaths(obj, paths) {
  if (!paths || !paths.length) return obj;
  const clone = obj === undefined ? obj : structuredClone(obj);
  for (const p of paths) {
    const parts = p.split('.');
    let node = clone;
    for (let i = 0; i < parts.length - 1 && node != null; i += 1) node = node[parts[i]];
    if (node && typeof node === 'object') delete node[parts[parts.length - 1]];
  }
  return clone;
}

// --- Replayers ---------------------------------------------------------------

const REPLAYERS = {
  // Intake DoR verdict — replays the shipped inline script verbatim.
  'intake-dor': (input) => {
    const src = extractNodeHeredoc(INTAKE_WORKFLOW, {
      index: input.scriptIndex || 0,
      mustContain: input.mustContain || 'const checks = {',
    });
    const env = {
      TRIGGER: input.trigger ?? 'ready-to-implement',
      SKIP_LABELS: JSON.stringify(input.skip_labels ?? []),
      NOT_READY: input.not_ready_label ?? 'scc:not-ready',
      GITHUB_OUTPUT: '__github_output__',
      ...(input.env || {}),
    };
    const r = replayInlineScript(src, {
      files: { 'issue.json': JSON.stringify(input.issue || {}) },
      env,
      nowMs: input.now ? Date.parse(input.now) : undefined,
    });
    const checksLine = r.log.find((l) => l.startsWith('DoR: '));
    return {
      outputs: r.outputs,
      checks: checksLine ? JSON.parse(checksLine.slice('DoR:'.length).trim()) : null,
      log: r.log,
      exitCode: r.exitCode,
      error: r.error,
    };
  },

  // Merge-gate decision table — replays the shipped inline script verbatim
  // with the fetched JSONs as fixtures and the clock pinned by `input.now`.
  'merge-gate': (input) => {
    const src = extractNodeHeredoc(AUTOMERGE_WORKFLOW, {
      index: input.scriptIndex || 0,
      mustContain: input.mustContain || 'decision:',
    });
    const env = {
      SKIP_LABELS: '[]',
      ESCALATION: 'needs-human',
      PENDING_MAX: '60',
      MAX_REPAIRS: '3',
      REVIEW_MODE: 'ai',
      REVIEW_WAIT: '15',
      EXT: '',
      REPO: 'owner/repo',
      REQUIRE_CHECKS: 'true',
      REQUIRE_PROV: 'true',
      PIPELINE_AUTHOR: 'ai-sdlc-runtime[bot]',
      ALLOW_AUTHORS: '[]',
      GITHUB_OUTPUT: '__github_output__',
      ...(input.env || {}),
    };
    const r = replayInlineScript(src, {
      files: {
        'pr.json': JSON.stringify(input.pr || {}),
        'checks.json': JSON.stringify(input.checks || []),
        'trusted-comments.json': JSON.stringify(input.trusted_comments || []),
        'ext-reviews.json': JSON.stringify(input.ext_reviews || []),
      },
      env,
      nowMs: Date.parse(input.now || '2026-01-01T00:00:00Z'),
    });
    return { outputs: r.outputs, log: r.log, exitCode: r.exitCode, error: r.error };
  },

  // Implement sanitize + DoR-surface pin — replays the shipped inline
  // script verbatim with the fetched JSONs as fixtures. The fetched
  // sanitizer module is intentionally absent in the sandbox → the script
  // exercises its inline-fencing fallback path, which is deterministic.
  'implement-sanitize': (input) => {
    const src = extractNodeHeredoc(IMPLEMENT_WORKFLOW, {
      index: input.scriptIndex || 0,
      mustContain: input.mustContain || 'const DORMARK = ',
    });
    const env = {
      RUNNER_TEMP: '/tmp/ai-sdlc-regression-fixture',
      GITHUB_OUTPUT: '__github_output__',
      ...(input.env || {}),
    };
    const r = replayInlineScript(src, {
      files: {
        'issue.json': JSON.stringify(input.issue || {}),
        'trusted-issue-comments.json': JSON.stringify(input.trusted_comments || []),
      },
      env,
      nowMs: input.now ? Date.parse(input.now) : undefined,
    });
    return {
      outputs: r.outputs,
      // '' = an empty prompt file was written (blocked/drift); null = none.
      prompt: r.written['task-prompt.md'] ?? null,
      log: r.log,
      exitCode: r.exitCode,
      error: r.error,
    };
  },

  // Risk taxonomy — classifyDiff over a fixture diff.
  'risk-classify': (input) => {
    try {
      return {
        result: classifyDiff({
          files: input.files,
          diffText: input.diffText,
          diffMaxBytes: input.diffMaxBytes,
        }),
      };
    } catch (e) {
      return { error: `${e.name}: ${e.message}` };
    }
  },

  // Label-mutation decision — applyRiskLabel against a recording `gh` stub.
  // Output captures both the returned decision and the exact gh argv stream,
  // so drift in either is a diff.
  'label-ops': (input) => {
    const calls = [];
    const exec = (argv) => {
      calls.push(argv.join(' '));
      const key = argv.slice(0, 2).join(' ');
      if (key === 'pr view') {
        return JSON.stringify({ labels: (input.labels || []).map((name) => ({ name })) });
      }
      if (key === 'label create' || key === 'pr edit') return '';
      throw new Error(`unexpected gh call: ${argv.join(' ')}`);
    };
    try {
      const result = applyRiskLabel({
        repo: input.repo || 'fixture/repo',
        pr: input.pr ?? 1,
        tier: input.tier,
        exec,
      });
      return { result, calls };
    } catch (e) {
      return { error: `${e.name}: ${e.message}`, calls };
    }
  },

  // Engine-adapter resolution surface. Ambient {ENGINE}_* env is scrubbed so
  // only the fixture's env participates — replays stay hermetic.
  'engine-adapter': (input) => {
    const engine = input.engine;
    const prefix = `${String(engine).toUpperCase().replace(/[^A-Z0-9]/g, '_')}_`;
    const saved = {};
    for (const k of Object.keys(process.env)) {
      if (k.startsWith(prefix)) { saved[k] = process.env[k]; delete process.env[k]; }
    }
    Object.assign(process.env, input.env || {});
    try {
      const adapter = getEngineAdapter(engine);
      switch (input.op) {
        case 'argv':
          return { argv: adapter.buildArgv(input.values || {}) };
        case 'exit-codes': {
          const codes = {};
          for (const c of input.codes || []) {
            codes[String(c)] = {
              classification: adapter.classifyExitCode(c),
              mapped: adapter.mapExitCode(c),
            };
          }
          return { exitCodes: codes };
        }
        case 'capabilities':
          return { capabilities: adapter.probeCapabilities() };
        case 'binary':
          return { binary: adapter.getBinary() };
        case 'descriptor':
          return {
            name: adapter.name,
            binary: adapter.binary,
            probe: adapter.probe,
            capabilities: adapter.capabilities,
            manifestMap: adapter.manifestMap || null,
          };
        default:
          throw new Error(`unknown engine-adapter op ${JSON.stringify(input.op)}`);
      }
    } catch (e) {
      return { error: `${e.name}: ${e.message}` };
    } finally {
      for (const k of Object.keys(input.env || {})) delete process.env[k];
      Object.assign(process.env, saved);
    }
  },

  // Contract resolution — runs the real resolve.py over a fixture contract
  // and captures contract.json plus the flat step outputs (GITHUB_OUTPUT).
  'contract-resolve': (input) => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'ai-sdlc-reg-'));
    try {
      const inFile = path.join(dir, 'in.json');
      fs.writeFileSync(inFile, JSON.stringify(input.repo_cfg || {}));
      const args = [
        RESOLVE_PY,
        '--input', inFile,
        '--out-dir', dir,
        '--overrides', JSON.stringify(input.overrides || {}),
        '--source-repo', input.source_repo || 'fixtures/repo',
        '--source-ref', input.source_ref || 'main',
        '--source-path', input.source_path || '.ai-sdlc.yaml',
      ];
      if (input.org) {
        const orgFile = path.join(dir, 'org.json');
        fs.writeFileSync(orgFile, JSON.stringify(input.org));
        args.push('--org', orgFile);
        if (input.org_repo) args.push('--org-repo', input.org_repo);
        if (input.org_ref) args.push('--org-ref', input.org_ref);
      }
      const ghOut = path.join(dir, 'github-output.txt');
      const r = spawnSync('python3', args, {
        encoding: 'utf8',
        env: { ...process.env, GITHUB_OUTPUT: ghOut },
      });
      const contractPath = path.join(dir, 'contract.json');
      const outputs = fs.existsSync(ghOut) ? parseGithubOutput(fs.readFileSync(ghOut, 'utf8')) : null;
      // Normalize the runner-temp artifact path — it carries a random
      // mkdtemp dir. (compare.omit may also exclude it; normalization keeps
      // goldens readable on any machine.)
      if (outputs && typeof outputs.path === 'string') {
        outputs.path = outputs.path.replace(dir, '<out-dir>');
      }
      return {
        exitStatus: r.status,
        stderr: String(r.stderr || '').trim() || null,
        contract: fs.existsSync(contractPath)
          ? JSON.parse(fs.readFileSync(contractPath, 'utf8'))
          : null,
        outputs,
      };
    } finally {
      fs.rmSync(dir, { recursive: true, force: true });
    }
  },

  // Webhook → normalized event conversion.
  'normalized-event': (input) => {
    const { convert } = require(NORMALIZED_EVENT);
    try {
      return { event: convert(JSON.stringify(input.payload || {}), input.event_name) };
    } catch (e) {
      return { error: `${e.name}: ${e.message}` };
    }
  },
};

// --- Suite --------------------------------------------------------------------

function loadCases(casesDir = CASES_DIR) {
  const files = fs.readdirSync(casesDir).filter((f) => f.endsWith('.json')).sort();
  return files.map((f) => {
    const c = JSON.parse(fs.readFileSync(path.join(casesDir, f), 'utf8'));
    return { ...c, _file: f };
  });
}

function runCase(caze, { goldenDir = GOLDEN_DIR } = {}) {
  const base = { id: caze.id || caze._file, type: caze.type, file: `cases/${caze._file}` };
  if (!REPLAYERS[caze.type]) {
    return { ...base, status: 'error', detail: `unknown case type ${JSON.stringify(caze.type)}` };
  }
  const missing = missingRequirements(caze);
  if (missing.length) {
    return { ...base, status: 'skip', detail: `missing tool(s): ${missing.join(', ')}` };
  }
  let actual;
  try {
    actual = REPLAYERS[caze.type](caze.input || {});
  } catch (e) {
    return { ...base, status: 'error', detail: `${e.name}: ${e.message}` };
  }
  const goldenPath = path.join(goldenDir, `${caze.id}.expected.json`);
  if (!fs.existsSync(goldenPath)) {
    return {
      ...base, status: 'fail', actual,
      diffs: [{ path: '(golden)', expected: ABSENT, actual: `missing golden ${path.relative(ROOT, goldenPath)} — record it with --bless ${caze.id}` }],
    };
  }
  let expected;
  try {
    expected = JSON.parse(fs.readFileSync(goldenPath, 'utf8'));
  } catch (e) {
    return { ...base, status: 'error', detail: `unparsable golden: ${e.message}` };
  }
  const omit = (caze.compare && caze.compare.omit) || [];
  const diffs = diffObjects(omitPaths(expected, omit), omitPaths(actual, omit));
  return { ...base, status: diffs.length ? 'fail' : 'pass', diffs, actual };
}

function summarize(results) {
  const s = { total: results.length, pass: 0, fail: 0, error: 0, skip: 0 };
  for (const r of results) s[r.status] = (s[r.status] || 0) + 1;
  s.ok = s.fail === 0 && s.error === 0;
  return s;
}

function runAll({ casesDir = CASES_DIR, goldenDir = GOLDEN_DIR, filter } = {}) {
  let cases = loadCases(casesDir);
  if (filter && filter.length) {
    const wanted = new Set(filter);
    cases = cases.filter((c) => wanted.has(c.id));
  }
  const results = cases.map((c) => runCase(c, { goldenDir }));
  return { generatedAt: new Date().toISOString(), cases: results, summary: summarize(results) };
}

// Record (or re-record) golden outputs — the bless path. Cases that error or
// skip produce no golden; they are reported and abort the bless.
function bless({ casesDir = CASES_DIR, goldenDir = GOLDEN_DIR, filter } = {}) {
  let cases = loadCases(casesDir);
  if (filter && filter.length) {
    const wanted = new Set(filter);
    cases = cases.filter((c) => wanted.has(c.id));
    if (!cases.length) throw new Error(`no case matches: ${filter.join(', ')}`);
  }
  fs.mkdirSync(goldenDir, { recursive: true });
  const written = [];
  const problems = [];
  for (const c of cases) {
    if (!REPLAYERS[c.type]) { problems.push(`${c.id}: unknown type ${c.type}`); continue; }
    const missing = missingRequirements(c);
    if (missing.length) { problems.push(`${c.id}: skipped — missing tool(s): ${missing.join(', ')}`); continue; }
    let actual;
    try {
      actual = REPLAYERS[c.type](c.input || {});
    } catch (e) {
      problems.push(`${c.id}: replay error — ${e.message}`);
      continue;
    }
    const p = path.join(goldenDir, `${c.id}.expected.json`);
    fs.writeFileSync(p, `${JSON.stringify(actual, null, 2)}\n`);
    written.push(p);
  }
  return { written, problems };
}

function formatReport(report) {
  const lines = [];
  const mark = { pass: '[✓]', fail: '[✗]', error: '[!]', skip: '[~]' };
  for (const r of report.cases) {
    lines.push(`${mark[r.status] || '?'} ${r.id} (${r.type})${r.detail ? ` — ${r.detail}` : ''}`);
    if (r.diffs) {
      for (const d of r.diffs.slice(0, 25)) {
        lines.push(`      ${d.path}: expected ${d.expected}, got ${d.actual}`);
      }
      if (r.diffs.length > 25) lines.push(`      … ${r.diffs.length - 25} more diff(s)`);
    }
  }
  const s = report.summary;
  lines.push(`— ${s.pass} pass / ${s.fail} fail / ${s.error} error / ${s.skip} skip of ${s.total}`);
  return lines.join('\n');
}

// --- CLI ----------------------------------------------------------------------

function usage() {
  return [
    'usage: regression_harness.js [--case ID ...] [--bless [ID ...]] [--json]',
    '                             [--report FILE] [--cases-dir D] [--golden-dir D]',
    '',
    '  (no flags)    replay every case against its golden; exit 1 on drift',
    '  --case ID     run only the named case(s) (repeatable or comma-separated)',
    '  --bless       record current behavior as the golden (all cases, or the',
    '                IDs listed after --bless up to the next flag)',
    '  --report F    write the diff report artifact (default: regression/results/report.json)',
    '  --json        print the full JSON report to stdout',
  ].join('\n');
}

function parseArgs(argv) {
  // bless: undefined = flag absent; [] = --bless with no IDs (bless all);
  // [ids] = --bless ID ...
  const opts = { cases: [], bless: undefined, json: false, report: undefined };
  for (let i = 0; i < argv.length; i += 1) {
    const a = argv[i];
    if (a === '--case') {
      i += 1;
      if (!argv[i]) throw new Error('missing value for --case');
      opts.cases.push(...argv[i].split(',').filter(Boolean));
    } else if (a === '--bless') {
      opts.bless = [];
      while (argv[i + 1] && !argv[i + 1].startsWith('--')) {
        i += 1;
        opts.bless.push(...argv[i].split(',').filter(Boolean));
      }
      // empty list = bless every case
    } else if (a === '--report') {
      i += 1;
      if (!argv[i]) throw new Error('missing value for --report');
      opts.report = argv[i];
    } else if (a === '--cases-dir') {
      i += 1;
      if (!argv[i]) throw new Error('missing value for --cases-dir');
      opts.casesDir = argv[i];
    } else if (a === '--golden-dir') {
      i += 1;
      if (!argv[i]) throw new Error('missing value for --golden-dir');
      opts.goldenDir = argv[i];
    } else if (a === '--json') {
      opts.json = true;
    } else if (a === '--help' || a === '-h') {
      console.log(usage());
      process.exit(0);
    } else {
      throw new Error(`unknown argument: ${a}`);
    }
  }
  return opts;
}

function main(argv) {
  const opts = parseArgs(argv);
  const casesDir = opts.casesDir || CASES_DIR;
  const goldenDir = opts.goldenDir || GOLDEN_DIR;
  const reportPath = opts.report === undefined
    ? path.join(RESULTS_DIR, 'report.json')
    : opts.report;

  if (opts.bless !== undefined) {
    const r = bless({ casesDir, goldenDir, filter: opts.bless.length ? opts.bless : undefined });
    for (const p of r.written) console.log(`[✓] blessed ${path.relative(ROOT, p)}`);
    for (const p of r.problems) console.log(`[!] ${p}`);
    console.log(`— ${r.written.length} golden(s) written, ${r.problems.length} problem(s)`);
    return r.problems.length ? 1 : 0;
  }

  const report = runAll({
    casesDir,
    goldenDir,
    filter: opts.cases.length ? opts.cases : undefined,
  });
  try {
    fs.mkdirSync(path.dirname(reportPath), { recursive: true });
    fs.writeFileSync(reportPath, `${JSON.stringify(report, null, 2)}\n`);
  } catch (e) {
    console.error(`[i] report artifact write failed: ${e.message}`);
  }
  if (opts.json) console.log(JSON.stringify(report, null, 2));
  else console.log(formatReport(report));
  if (reportPath) console.log(`[i] report → ${path.relative(ROOT, reportPath) || reportPath}`);
  return report.summary.ok ? 0 : 1;
}

if (require.main === module) {
  try {
    process.exit(main(process.argv.slice(2)));
  } catch (e) {
    console.error(`regression-harness: ${e.message}`);
    process.exit(2);
  }
}

module.exports = {
  ROOT,
  CASES_DIR,
  GOLDEN_DIR,
  RESULTS_DIR,
  INTAKE_WORKFLOW,
  AUTOMERGE_WORKFLOW,
  extractNodeHeredoc,
  replayInlineScript,
  parseGithubOutput,
  diffObjects,
  omitPaths,
  REPLAYERS,
  loadCases,
  runCase,
  runAll,
  bless,
  formatReport,
  main,
};
