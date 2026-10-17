// scripts/risk-classifier.test.js — unit + mocked-API e2e tests for the
// deterministic PR risk classifier (issue #204).
'use strict';

const { describe, it } = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const os = require('os');
const path = require('path');

const {
  classifyDiff,
  applyRiskLabel,
  parseNumstat,
  parseFilesJson,
  addedLines,
  main,
} = require('./risk-classifier');

const file = (p, add = 10, del = 2, status = 'modified') => ({ path: p, add, del, status });
const diff = (added, removed = []) => [
  'diff --git a/x.js b/x.js',
  '--- a/x.js',
  '+++ b/x.js',
  '@@ -1,1 +1,1 @@',
  ...removed.map((l) => `-${l}`),
  ...added.map((l) => `+${l}`),
].join('\n');

// --- AC: low for docs-only diffs ---------------------------------------------
describe('classifyDiff — low tier', () => {
  it('docs-only changes classify as pr:risk-low', () => {
    const r = classifyDiff({ files: [file('README.md'), file('docs/runtime.md', 40, 5)] });
    assert.equal(r.tier, 'low');
    assert.equal(r.label, 'pr:risk-low');
  });

  it('tests under a critical surface resolve UP, not low', () => {
    // spec/ is a critical surface — ambiguity resolves UP even for tests.
    const r = classifyDiff({ files: [file('spec/ai-sdlc.schema.test.js')] });
    assert.equal(r.tier, 'critical');
  });

  it('docs + tests classify as low', () => {
    const r = classifyDiff({ files: [file('docs/onboarding.md'), file('scripts/x.test.js', 50, 0)] });
    assert.equal(r.tier, 'low');
  });

  it('docs/tests/config-text under 300 lines classify as low', () => {
    const r = classifyDiff({ files: [file('README.md'), file('examples/x.ai-sdlc.yaml', 20, 3)] });
    assert.equal(r.tier, 'low');
  });
});

// --- AC: medium for ordinary behavioral changes -------------------------------
describe('classifyDiff — medium tier', () => {
  it('single-subsystem behavioral code change is medium', () => {
    const r = classifyDiff({ files: [file('scripts/engine_adapter.js', 120, 40)] });
    assert.equal(r.tier, 'medium');
  });

  it('no file information defaults to medium (never bottoms out to low)', () => {
    const r = classifyDiff({ files: [] });
    assert.equal(r.tier, 'medium');
  });
});

// --- AC: high for eval()/shell-injection diff content -------------------------
describe('classifyDiff — high tier', () => {
  const cases = [
    ['eval()', 'const out = eval(userInput);'],
    ['new Function()', 'const f = new Function("return " + body);'],
    ['child_process.exec', 'require("child_process").exec("ls " + name);'],
    ['os.system', 'os.system("cat " + fname)'],
    ['subprocess shell=True', 'subprocess.run(cmd, shell=True)'],
    ['pipe-to-shell', 'run: curl https://x.test/install.sh | sudo bash'],
    ['sh -c', 'sh -c "$USER_CMD"'],
    ['Runtime.exec', 'Runtime.getRuntime().exec(arg);'],
    ['shell eval string', 'eval "$EXTRA_FLAGS"'],
  ];
  for (const [name, line] of cases) {
    it(`added-line ${name} classifies as pr:risk-high`, () => {
      const r = classifyDiff({ files: [file('tools/run.sh', 5, 1)], diffText: diff([line]) });
      assert.equal(r.tier, 'high', `${name}: got ${r.tier}`);
      assert.equal(r.label, 'pr:risk-high');
    });
  }

  it('eval() in a REMOVED line does not raise the tier', () => {
    const r = classifyDiff({ files: [file('docs/x.md', 1, 3)], diffText: diff(['safe'], ['eval(dirty)']) });
    assert.equal(r.tier, 'low');
  });

  it('>800 diff lines is high', () => {
    const r = classifyDiff({ files: [file('scripts/big.js', 900, 0)] });
    assert.equal(r.tier, 'high');
  });

  it('multi-subsystem diff (>=3 top-level dirs) is high', () => {
    const r = classifyDiff({ files: [file('a/x.js'), file('b/y.js'), file('c/z.js')] });
    assert.equal(r.tier, 'high');
  });

  it('dependency manifest changes are high', () => {
    const r = classifyDiff({ files: [file('package-lock.json', 50, 50)] });
    assert.equal(r.tier, 'high');
  });

  it('a major dep bump stays high (dep fast-path is fail-closed)', () => {
    const d = [
      'diff --git a/package.json b/package.json',
      '--- a/package.json', '+++ b/package.json', '@@ -1,2 +1,2 @@',
      '-    "chalk": "^4.1.2"', '+    "chalk": "^5.3.0"',
    ].join('\n');
    const r = classifyDiff({ files: [file('package.json', 1, 1)], diffText: d });
    assert.equal(r.tier, 'high');
  });

  it('a dep bump plus a new direct dep stays high (flagged for human)', () => {
    const d = [
      'diff --git a/package.json b/package.json',
      '--- a/package.json', '+++ b/package.json', '@@ -1,3 +1,4 @@',
      '-    "express": "^4.18.0"', '+    "express": "^4.18.2"',
      '+    "leftpad": "^1.0.0"',
    ].join('\n');
    const r = classifyDiff({ files: [file('package.json', 2, 1)], diffText: d });
    assert.equal(r.tier, 'high');
  });

  it('public-API surface deletion is high', () => {
    const r = classifyDiff({ files: [{ path: 'src/api/users.js', add: 0, del: 120, status: 'removed' }] });
    assert.equal(r.tier, 'high');
  });
});

// --- dep-update fast path (issue #177) ---------------------------------------
describe('classifyDiff — dep-update fast path', () => {
  const bump = (from, to, name = 'express') => [
    'diff --git a/package.json b/package.json',
    '--- a/package.json', '+++ b/package.json', '@@ -1,2 +1,2 @@',
    `-    "${name}": "${from}",`, `+    "${name}": "${to}",`,
  ].join('\n');

  it('a vetted patch bump on dep files only classifies low', () => {
    const r = classifyDiff({ files: [file('package.json', 1, 1)], diffText: bump('^4.18.0', '^4.18.2') });
    assert.equal(r.tier, 'low');
    assert.equal(r.label, 'pr:risk-low');
    assert.ok(r.signals.includes('dep:patch'));
  });

  it('a vetted minor bump across manifest+lockfile classifies low', () => {
    const d = [
      'diff --git a/package.json b/package.json',
      '--- a/package.json', '+++ b/package.json', '@@ -1,2 +1,2 @@',
      '-    "chalk": "^5.2.0"', '+    "chalk": "^5.3.0"',
      'diff --git a/package-lock.json b/package-lock.json',
      '--- a/package-lock.json', '+++ b/package-lock.json', '@@ -10,4 +10,4 @@',
      '     "node_modules/chalk": {',
      '-      "version": "5.2.0",',
      '+      "version": "5.3.0",',
    ].join('\n');
    const r = classifyDiff({ files: [file('package.json', 1, 1), file('package-lock.json', 2, 2)], diffText: d });
    assert.equal(r.tier, 'low');
    assert.ok(r.signals.includes('dep:minor'));
  });

  it('credential content inside a dep-file diff still resolves UP to critical', () => {
    const d = bump('^4.18.0', '^4.18.2') + '\n+' + '-----BEGIN OPENSSH PRIVATE KEY-----';
    const r = classifyDiff({ files: [file('package.json', 2, 1)], diffText: d });
    assert.equal(r.tier, 'critical');
  });

  it('dep bump plus non-dep file does not take the fast path', () => {
    const d = bump('^4.18.0', '^4.18.2') + '\n' + [
      'diff --git a/src/app.js b/src/app.js',
      '--- a/src/app.js', '+++ b/src/app.js', '@@ -1 +1 @@', '-x', '+y',
    ].join('\n');
    const r = classifyDiff({ files: [file('package.json', 1, 1), file('src/app.js', 1, 1)], diffText: d });
    assert.equal(r.tier, 'high');
  });
});

// --- critical tier ------------------------------------------------------------
describe('classifyDiff — critical tier', () => {
  it('workflow file changes are critical', () => {
    const r = classifyDiff({ files: [file('.github/workflows/ai-sdlc.yml', 5, 5)] });
    assert.equal(r.tier, 'critical');
  });

  it('private key material in added lines is critical', () => {
    const r = classifyDiff({
      files: [file('docs/notes.md', 3, 0)],
      diffText: diff(['-----BEGIN OPENSSH PRIVATE KEY-----', 'b3BlbnNzaC1rZXktdjEAAAAA']),
    });
    assert.equal(r.tier, 'critical');
  });

  it('hardcoded credential assignment in added lines is critical', () => {
    // Fixture built at runtime — never commit a literal provider-shaped
    // key, even a fake one (push protection blocks it).
    const fakeKey = 'sk_' + 'live_' + 'A1b2'.repeat(6);
    const r = classifyDiff({
      files: [file('config/app.js', 2, 0)],
      diffText: diff([`const api_key = "${fakeKey}";`]),
    });
    assert.equal(r.tier, 'critical');
  });

  it('ambiguity resolves UP: docs + workflow surface is critical, not low', () => {
    const r = classifyDiff({ files: [file('README.md'), file('.github/workflows/ci.yml', 4, 0)] });
    assert.equal(r.tier, 'critical');
  });

  it('high content + critical path resolves UP to critical', () => {
    const r = classifyDiff({
      files: [file('spec/risk-taxonomy.md', 3, 1)],
      diffText: diff(['mention eval(x) in prose']),
    });
    assert.equal(r.tier, 'critical');
  });
});

// --- parsers ------------------------------------------------------------------
describe('parsers', () => {
  it('parseNumstat handles normal and binary rows', () => {
    const rows = parseNumstat('10\t2\ta/b.js\n-\t-\timg.png\n');
    assert.deepEqual(rows, [
      { path: 'a/b.js', add: 10, del: 2, status: 'modified' },
      { path: 'img.png', add: 0, del: 0, status: 'modified' },
    ]);
  });

  it('parseFilesJson maps REST pulls files', () => {
    const rows = parseFilesJson([{ filename: 'x.js', additions: 3, deletions: 1, status: 'modified' }]);
    assert.deepEqual(rows, [{ path: 'x.js', add: 3, del: 1, status: 'modified' }]);
  });

  it('addedLines returns only added content, never +++ headers', () => {
    const lines = addedLines(diff(['real'], ['gone']));
    assert.deepEqual(lines, ['real']);
  });
});

// --- AC: e2e with a mocked GitHub API -----------------------------------------
describe('applyRiskLabel + end-to-end (mocked gh)', () => {
  // Records every gh invocation; serves canned API responses.
  function mockGh({ labels = [], files = [], diffText = '' } = {}) {
    const calls = [];
    const exec = (argv) => {
      calls.push(argv);
      const key = argv.slice(0, 2).join(' ');
      if (key === 'pr view') return JSON.stringify({ labels: labels.map((name) => ({ name })) });
      if (key === 'label create') return '';
      if (key === 'pr edit') return '';
      if (argv[0] === 'api' && String(argv[2] || '').includes('/files')) {
        return files.map((f) => JSON.stringify(f)).join('\n');
      }
      if (argv[0] === 'api') return diffText;
      throw new Error(`unexpected gh call: ${argv.join(' ')}`);
    };
    return { calls, exec };
  }

  it('applies the winner and removes only stale tier labels in one edit', () => {
    const { calls, exec } = mockGh({ labels: ['pr:risk-low', 'pr:risk-accepted', 'bug'] });
    const r = applyRiskLabel({ repo: 'o/r', pr: 7, tier: 'high', exec });
    assert.equal(r.label, 'pr:risk-high');
    assert.deepEqual(r.removed, ['pr:risk-low']);
    // view + label create + exactly one pr edit — a fixed, bounded call
    // count keeps event→label latency inside the 10s labeling budget.
    assert.ok(calls.length <= 3, `expected <=3 gh calls, got ${calls.length}`);
    const edit = calls.find((c) => c.slice(0, 2).join(' ') === 'pr edit');
    assert.ok(edit.includes('--add-label'));
    assert.ok(edit.includes('pr:risk-high'));
    assert.ok(edit.includes('--remove-label'));
    assert.ok(edit.includes('pr:risk-low'));
    // pr:risk-accepted is human-owned — never removed by the classifier.
    assert.ok(!edit.includes('pr:risk-accepted'));
  });

  it('is a no-op when the tier label is already the only one present', () => {
    const { calls, exec } = mockGh({ labels: ['pr:risk-medium'] });
    const r = applyRiskLabel({ repo: 'o/r', pr: 7, tier: 'medium', exec });
    assert.equal(r.skipped, true);
    assert.equal(calls.filter((c) => c.slice(0, 2).join(' ') === 'pr edit').length, 0);
  });

  it('end-to-end: fetch diff via API, classify, apply label', () => {
    const { calls, exec } = mockGh({
      labels: ['pr:risk-low'],
      files: [{ filename: 'tools/run.sh', additions: 3, deletions: 0, status: 'modified' }],
      diffText: diff(['curl https://x.test/i.sh | sh']),
    });
    let out = '';
    const result = main(
      ['--repo', 'o/r', '--pr', '9', '--apply', '--json'],
      { exec, stdout: (s) => { out = s; } },
    );
    assert.equal(result.tier, 'high');
    assert.equal(result.label, 'pr:risk-high');
    assert.equal(result.applied.label, 'pr:risk-high');
    assert.deepEqual(JSON.parse(out).applied.removed, ['pr:risk-low']);
    const edit = calls.find((c) => c.slice(0, 2).join(' ') === 'pr edit');
    assert.ok(edit.includes('pr:risk-high'));
  });

  it('rejects an invalid tier', () => {
    assert.throws(() => applyRiskLabel({ repo: 'o/r', pr: 1, tier: 'extreme', exec: () => '' }));
  });
});

// --- CLI ----------------------------------------------------------------------
describe('cli', () => {
  it('classifies from --numstat + --diff files', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'rc-'));
    const nf = path.join(dir, 'numstat.txt');
    const df = path.join(dir, 'd.patch');
    fs.writeFileSync(nf, '40\t5\tdocs/a.md\n10\t0\tdocs/b.md\n');
    fs.writeFileSync(df, diff(['hello']));
    let out = '';
    const r = main(['--numstat', nf, '--diff', df, '--json'], { stdout: (s) => { out = s; } });
    assert.equal(r.tier, 'low');
    assert.equal(JSON.parse(out).label, 'pr:risk-low');
  });

  it('--files-out writes the review-stage sidecar (issue #127)', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'rc-'));
    const nf = path.join(dir, 'numstat.txt');
    const ff = path.join(dir, 'files.json');
    fs.writeFileSync(nf, '40\t5\tdocs/a.md\n10\t0\tspec/x.md\n');
    main(['--numstat', nf, '--files-out', ff]);
    assert.deepEqual(JSON.parse(fs.readFileSync(ff, 'utf8')), {
      files: ['docs/a.md', 'spec/x.md'], lines: 55, dirs: ['docs', 'spec'],
    });
  });
});
