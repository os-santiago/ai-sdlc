// tools/ai-sdlc-init.test.js — node --test coverage for the repo installer
// (issue #185). fs is mocked by operating in mkdtemp'd directories; spawn is
// injectable so --pr / git paths run without a real GitHub remote.

const test = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const path = require('path');
const os = require('os');
const { spawnSync } = require('child_process');

const init = require('./ai-sdlc-init.js');

const HAS_PY_YAML = spawnSync('python3', ['-c', 'import yaml'], { encoding: 'utf8' }).status === 0;

function tmpdir() {
  return fs.mkdtempSync(path.join(os.tmpdir(), 'ai-sdlc-init-test-'));
}

function silentInit(opts, deps = {}) {
  const lines = [];
  return { res: init.init(opts, { ...deps, log: (m) => lines.push(m) }), lines };
}

// --- starterConfig ----------------------------------------------------------

test('starterConfig pins autonomy.level: shadow by default', () => {
  const cfg = init.starterConfig();
  assert.match(cfg, /level: shadow\b/);
  assert.match(cfg, /spec: "1\.0\.0"/);
});

test('starterConfig honors level/archetype/language flags', () => {
  const cfg = init.starterConfig({ level: 'auto-pr', archetype: 'library', language: 'typescript' });
  assert.match(cfg, /level: auto-pr\b/);
  assert.match(cfg, /archetype: library\b/);
  assert.match(cfg, /primary_language: typescript\b/);
});

test('generated contract conforms to spec/ai-sdlc.schema.json (real validator)', () => {
  const v = init.validateContract(init.starterConfig(), {});
  assert.equal(v.ok, true, v.output);
});

test('validator rejects a tampered contract (fail-closed generation)', () => {
  const bad = init.starterConfig().replace('level: shadow', 'level: godmode');
  const v = init.validateContract(bad, {});
  assert.equal(v.ok, false);
});

// --- callerWorkflow ---------------------------------------------------------

test('callerWorkflow references every reusable stage at the pinned ref', { skip: !HAS_PY_YAML && 'python3+yaml needed to parse generated YAML' }, () => {
  const wf = init.callerWorkflow({ runtime: 'os-santiago/ai-sdlc', ref: 'v9', base: 'main' });
  for (const stage of ['intake', 'implement', 'classify', 'review', 'automerge', 'ci-repair', 'verify']) {
    assert.match(wf, new RegExp(`uses: os-santiago/ai-sdlc/\\.github/workflows/ai-sdlc-${stage}\\.yml@v9`), stage);
  }
  // Fork guard + secrets inheritance on every reusable call.
  assert.match(wf, /head\.repo\.full_name == github\.repository/);
  assert.equal((wf.match(/secrets: inherit/g) || []).length, 7);
  // Label bootstrap inside the PR — no direct API writes pre-merge.
  assert.match(wf, /labels-bootstrap:/);
  assert.match(wf, /gh label create/);
  // Parses as YAML.
  const tmp = path.join(tmpdir(), 'wf.yml');
  fs.writeFileSync(tmp, wf);
  const r = spawnSync('python3', ['-c', 'import yaml,sys; yaml.safe_load(open(sys.argv[1]))', tmp], { encoding: 'utf8' });
  assert.equal(r.status, 0, r.stderr);
});

test('callerWorkflow pins a custom runtime repo', () => {
  const wf = init.callerWorkflow({ runtime: 'acme/fork', ref: 'main', base: 'main' });
  assert.match(wf, /uses: acme\/fork\/\.github\/workflows\/ai-sdlc-intake\.yml@main/);
});

// --- sweepWorkflow ----------------------------------------------------------

test('sweepWorkflow re-points the gate job at the cross-repo automerge', () => {
  const dir = tmpdir();
  const src = path.join(dir, 'sweep.yml');
  fs.writeFileSync(src, 'jobs:\n  gate:\n    uses: ./.github/workflows/ai-sdlc-automerge.yml\n    secrets: inherit\n');
  const out = init.sweepWorkflow({ runtime: 'os-santiago/ai-sdlc', ref: 'main', sweepSource: src });
  assert.match(out, /uses: os-santiago\/ai-sdlc\/\.github\/workflows\/ai-sdlc-automerge\.yml@main/);
  assert.match(out, /secrets: inherit/);
  assert.doesNotMatch(out, /uses: \.\/\.github\/workflows/);
});

test('sweepWorkflow returns null when the source is missing or unexpected', () => {
  assert.equal(init.sweepWorkflow({ sweepSource: '/nonexistent/sweep.yml' }), null);
  const dir = tmpdir();
  const src = path.join(dir, 'sweep.yml');
  fs.writeFileSync(src, 'no gate here\n');
  assert.equal(init.sweepWorkflow({ sweepSource: src }), null);
});

// --- applyPlan / idempotency ------------------------------------------------

test('init writes contract + caller + sweep into a fresh dir', () => {
  const dir = tmpdir();
  const { res } = silentInit({ dir, base: 'main' });
  const rels = Object.fromEntries(res.results.map((r) => [r.rel, r.status]));
  assert.equal(rels['.ai-sdlc.yaml'], 'written');
  assert.equal(rels[path.join('.github', 'workflows', 'ai-sdlc.yml')], 'written');
  assert.equal(rels[path.join('.github', 'workflows', 'ai-sdlc-sweep.yml')], 'written');
  assert.match(fs.readFileSync(path.join(dir, '.ai-sdlc.yaml'), 'utf8'), /level: shadow/);
});

test('init is idempotent — identical re-run reports unchanged', () => {
  const dir = tmpdir();
  silentInit({ dir, base: 'main' });
  const { res } = silentInit({ dir, base: 'main' });
  assert.ok(res.results.every((r) => r.status === 'unchanged'), JSON.stringify(res.results));
});

test('init keeps divergent existing files without --force', () => {
  const dir = tmpdir();
  fs.writeFileSync(path.join(dir, '.ai-sdlc.yaml'), 'spec: "1.0.0"\ncustom: true\n');
  const { res } = silentInit({ dir, base: 'main' });
  const cfg = res.results.find((r) => r.rel === '.ai-sdlc.yaml');
  assert.equal(cfg.status, 'kept');
  assert.match(fs.readFileSync(path.join(dir, '.ai-sdlc.yaml'), 'utf8'), /custom: true/);
});

test('--force overwrites divergent files', () => {
  const dir = tmpdir();
  fs.writeFileSync(path.join(dir, '.ai-sdlc.yaml'), 'spec: "1.0.0"\ncustom: true\n');
  const { res } = silentInit({ dir, base: 'main', force: true });
  const cfg = res.results.find((r) => r.rel === '.ai-sdlc.yaml');
  assert.equal(cfg.status, 'overwritten');
  assert.match(fs.readFileSync(path.join(dir, '.ai-sdlc.yaml'), 'utf8'), /level: shadow/);
});

test('--dry-run validates but writes nothing', () => {
  const dir = tmpdir();
  const { res, lines } = silentInit({ dir, base: 'main', dryRun: true });
  assert.ok(res.results.every((r) => r.status === 'would write'));
  assert.equal(fs.readdirSync(dir).length, 0);
  assert.ok(lines.some((l) => /conforms/.test(l)));
});

test('--no-sweep writes only contract + caller', () => {
  const dir = tmpdir();
  const { res } = silentInit({ dir, base: 'main', sweep: false });
  assert.equal(res.results.length, 2);
  assert.ok(!fs.existsSync(path.join(dir, '.github', 'workflows', 'ai-sdlc-sweep.yml')));
});

test('init fails on a nonexistent target dir', () => {
  assert.throws(() => silentInit({ dir: path.join(tmpdir(), 'nope') }), /not found/);
});

// --- --pr path (spawn injected) ----------------------------------------------

test('--pr drives git branch/commit/push and gh pr create', () => {
  const dir = tmpdir();
  const calls = [];
  const fakeSpawn = (cmd, args) => {
    calls.push([cmd, ...args]);
    if (cmd === 'gh' && args[0] === 'pr') return { status: 0, stdout: 'https://github.com/acme/x/pull/42\n' };
    return { status: 0, stdout: '', stderr: '' };
  };
  const { res } = silentInit({ dir, base: 'main', pr: true }, { spawnSync: fakeSpawn });
  assert.equal(res.prUrl, 'https://github.com/acme/x/pull/42');
  const flat = calls.map((c) => c.join(' '));
  assert.ok(flat.some((c) => c.startsWith('git checkout -b chore/ai-sdlc-onboarding')));
  assert.ok(flat.some((c) => c.startsWith('git push -u origin chore/ai-sdlc-onboarding')));
  assert.ok(flat.some((c) => c.startsWith('gh pr create')));
});

test('--pr refuses when a divergent file was kept (no silent overwrite in a PR)', () => {
  const dir = tmpdir();
  fs.writeFileSync(path.join(dir, '.ai-sdlc.yaml'), 'spec: "1.0.0"\ncustom: true\n');
  const calls = [];
  const fakeSpawn = (cmd, args) => { calls.push([cmd, ...args].join(' ')); return { status: 0, stdout: '' }; };
  assert.throws(() => silentInit({ dir, base: 'main', pr: true }, { spawnSync: fakeSpawn }), /--pr refused/);
  // Validation spawns bash; no git/gh call may happen before the refusal.
  assert.equal(calls.filter((c) => c.startsWith('git ') || c.startsWith('gh ')).length, 0);
});

// --- arg parsing -------------------------------------------------------------

test('parseArgs rejects invalid level/archetype/language and unknown flags', () => {
  assert.throws(() => init.parseArgs(['--level', 'godmode']), /--level/);
  assert.throws(() => init.parseArgs(['--archetype', 'moonbase']), /--archetype/);
  assert.throws(() => init.parseArgs(['--wat']), /unknown flag/);
  assert.throws(() => init.parseArgs(['--level']), /missing value/);
  assert.throws(() => init.parseArgs(['a', 'b']), /unexpected arguments/);
  const o = init.parseArgs(['x', '--dry-run', '--ref', 'v1.2.3']);
  assert.equal(o.dir, 'x');
  assert.equal(o.dryRun, true);
  assert.equal(o.ref, 'v1.2.3');
});

// --- next steps / PR body ----------------------------------------------------

test('nextSteps prints the secrets checklist and shadow-mode note', () => {
  const s = init.nextSteps({ wrote: true, level: 'shadow', runtime: 'os-santiago/ai-sdlc', ref: 'main' });
  for (const frag of ['AI_SDLC_APP_ID', 'AI_SDLC_APP_PRIVATE_KEY', 'MODEL_API_KEY', 'AI_SDLC_PROVIDER_URL', 'labels-bootstrap', 'ready-to-implement']) {
    assert.ok(s.includes(frag), frag);
  }
});

test('prBody carries the secrets checklist and the secretless end-state', () => {
  const b = init.prBody({ level: 'shadow' });
  for (const frag of ['AI_SDLC_APP_ID', 'MODEL_API_KEY', 'AI_SDLC_PROVIDER_URL', 'OIDC-minted', 'zero']) {
    assert.ok(b.includes(frag), frag);
  }
});
