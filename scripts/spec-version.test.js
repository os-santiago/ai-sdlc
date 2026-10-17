// scripts/spec-version.test.js — unit + mocked-exec e2e tests for the
// spec/v* tagging machinery (issue #134). Every test runs in dry-run or
// against a fake exec seam — no real tags, pushes, or releases are created.
'use strict';

const { describe, it } = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const os = require('os');
const path = require('path');

const {
  diffSchema,
  classifyFileChange,
  parseNameStatus,
  readDeclared,
  planSpec,
  renderSpecNotes,
  publishSpec,
  parseArgs,
  main,
} = require('./spec-version');

// Fake exec: canned responses per matcher + a call log for assertions.
function fakeExec(handlers) {
  const calls = [];
  const fn = (argv) => {
    const args = argv.map(String);
    calls.push(args);
    for (const h of handlers) {
      if (h.match(args)) {
        if (h.throw) throw new Error(h.throw);
        return typeof h.out === 'function' ? h.out(args) : (h.out || '');
      }
    }
    return '';
  };
  fn.calls = calls;
  return fn;
}

const has = (needle) => (args) => args.join(' ').includes(needle);
const isTagList = (glob) => (args) =>
  args[0] === 'git' && args.includes('tag') && args.includes('--list') && args.includes(glob);

const collect = () => ({ reasons: [] });

// --- Schema diff classification --------------------------------------------------

describe('diffSchema — JSON-schema semantic classification', () => {
  it('annotation-only edits (description/title/examples) are editorial → patch', () => {
    const o = { type: 'object', properties: { a: { type: 'string', description: 'x' } } };
    const n = { type: 'object', properties: { a: { type: 'string', description: 'y' } } };
    assert.equal(diffSchema(o, n, '#', collect()), 1);
  });

  it('added optional property → minor', () => {
    const o = { type: 'object', properties: { a: { type: 'string' } } };
    const n = { type: 'object', properties: { a: { type: 'string' }, b: { type: 'number' } } };
    assert.equal(diffSchema(o, n, '#', collect()), 2);
  });

  it('added required property → major', () => {
    const o = { type: 'object', properties: { a: { type: 'string' } }, required: ['a'] };
    const n = {
      type: 'object',
      properties: { a: { type: 'string' }, b: { type: 'number' } },
      required: ['a', 'b'],
    };
    assert.equal(diffSchema(o, n, '#', collect()), 3);
  });

  it('removed or renamed property → major', () => {
    const o = { type: 'object', properties: { a: { type: 'string' }, b: { type: 'number' } } };
    const n = { type: 'object', properties: { a: { type: 'string' }, c: { type: 'number' } } };
    const out = collect();
    assert.equal(diffSchema(o, n, '#', out), 3);
    assert.ok(out.reasons.some((r) => r.path.includes('/b: removed')));
  });

  it('tightened constraints → major', () => {
    const cases = [
      [{ type: 'string' }, { type: 'string', minLength: 3 }],           // bound added
      [{ type: 'string', minLength: 1 }, { type: 'string', minLength: 5 }], // raised
      [{ type: 'string', maxLength: 9 }, { type: 'string', maxLength: 4 }], // lowered
      [{ type: 'string' }, { type: 'string', pattern: '^a' }],          // pattern added
      [{ type: 'string', enum: ['a', 'b', 'c'] }, { type: 'string', enum: ['a', 'b'] }],
      [{ type: ['string', 'null'] }, { type: 'string' }],               // type narrowed
      [{ type: 'object' }, { type: 'object', additionalProperties: false }],
      [{ type: 'array' }, { type: 'array', uniqueItems: true }],
      [{ anyOf: [{ type: 'string' }, { type: 'number' }] }, { anyOf: [{ type: 'string' }] }],
      [{ allOf: [{ type: 'object' }] }, { allOf: [{ type: 'object' }, { required: ['x'] }] }],
    ];
    for (const [o, n] of cases) {
      assert.equal(diffSchema(o, n, '#', collect()), 3, JSON.stringify({ o, n }));
    }
  });

  it('loosened constraints → minor', () => {
    const cases = [
      [{ type: 'string', minLength: 5 }, { type: 'string', minLength: 1 }],
      [{ type: 'string', maxLength: 4 }, { type: 'string', maxLength: 9 }],
      [{ type: 'string', pattern: '^a' }, { type: 'string' }],
      [{ type: 'string', enum: ['a'] }, { type: 'string', enum: ['a', 'b'] }],
      [{ type: 'string' }, { type: ['string', 'null'] }],
      [{ type: 'object', additionalProperties: false }, { type: 'object', additionalProperties: true }],
      [{ anyOf: [{ type: 'string' }] }, { anyOf: [{ type: 'string' }, { type: 'number' }] }],
      [{ allOf: [{ type: 'object' }, { required: ['x'] }] }, { allOf: [{ type: 'object' }] }],
    ];
    for (const [o, n] of cases) {
      assert.equal(diffSchema(o, n, '#', collect()), 2, JSON.stringify({ o, n }));
    }
  });

  it('newly required existing property → major', () => {
    const o = { type: 'object', properties: { a: {} }, required: [] };
    const n = { type: 'object', properties: { a: {} }, required: ['a'] };
    assert.equal(diffSchema(o, n, '#', collect()), 3);
  });

  it('nested diffs recurse into properties/$defs', () => {
    const o = { $defs: { x: { type: 'object', properties: { f: { type: 'string' } } } } };
    const n = { $defs: { x: { type: 'object', properties: {} } } };
    assert.equal(diffSchema(o, n, '#', collect()), 3);
  });

  it('identical schemas → none', () => {
    const s = { type: 'object', properties: { a: { type: 'string' } }, required: ['a'] };
    assert.equal(diffSchema(s, s, '#', collect()), 0);
  });
});

// --- File-level classification ---------------------------------------------------

describe('classifyFileChange', () => {
  const exec = fakeExec([]);

  it('deleted spec file → major; renamed → major; added → minor', () => {
    assert.equal(classifyFileChange(exec, '/r', 'b', 'h', { status: 'D', path: 'spec/x.md' }, collect()).level, 'major');
    assert.equal(classifyFileChange(exec, '/r', 'b', 'h', { status: 'R', oldPath: 'spec/a.md', path: 'spec/b.md' }, collect()).level, 'major');
    assert.equal(classifyFileChange(exec, '/r', 'b', 'h', { status: 'A', path: 'spec/new.md' }, collect()).level, 'minor');
  });

  it('modified markdown → patch (editorial heuristic)', () => {
    const r = classifyFileChange(exec, '/r', 'b', 'h', { status: 'M', path: 'spec/README.md' }, collect());
    assert.equal(r.level, 'patch');
  });

  it('modified JSON parses both sides via git show and diffs the schema', () => {
    const exec2 = fakeExec([
      { match: has('b:spec/s.json'), out: '{"type":"object","properties":{"a":{"type":"string"}}}' },
      { match: has('h:spec/s.json'), out: '{"type":"object","properties":{}}' },
    ]);
    const r = classifyFileChange(exec2, '/r', 'b', 'h', { status: 'M', path: 'spec/s.json' }, collect());
    assert.equal(r.level, 'major');
  });
});

describe('parseNameStatus', () => {
  it('parses M/A/D/R records', () => {
    const rows = parseNameStatus('M\tspec/a.md\nA\tspec/b.json\nD\tspec/c.md\nR100\tspec/d.md\tspec/e.md\n');
    assert.deepEqual(rows.map((r) => r.status), ['M', 'A', 'D', 'R']);
    assert.equal(rows[3].oldPath, 'spec/d.md');
    assert.equal(rows[3].path, 'spec/e.md');
  });
});

// --- Declared version --------------------------------------------------------------

describe('readDeclared', () => {
  it('parses the declared marker and ignores other spec/v* mentions', () => {
    const f = path.join(fs.mkdtempSync(path.join(os.tmpdir(), 'sv-')), 'README.md');
    fs.writeFileSync(f, 'semver-tagged (`spec/vX.Y.Z` tags).\n\n**Declared version:** `spec/v1.2.3` — bumped per PR.\n');
    assert.deepEqual(readDeclared(f), { version: [1, 2, 3], tag: 'spec/v1.2.3' });
  });

  it('returns null when the marker is absent', () => {
    const f = path.join(fs.mkdtempSync(path.join(os.tmpdir(), 'sv-')), 'README.md');
    fs.writeFileSync(f, 'no marker here, only spec/vX.Y.Z prose\n');
    assert.equal(readDeclared(f), null);
  });
});

// --- Plan computation ---------------------------------------------------------------

function readmeFile(declared) {
  const f = path.join(fs.mkdtempSync(path.join(os.tmpdir(), 'sv-')), 'README.md');
  fs.writeFileSync(f, `**Declared version:** \`spec/v${declared}\`\n`);
  return f;
}

function planEnv({ tags = '', diff = '', lsTree = '', shas = '', log = '', shows = {} } = {}) {
  const exec = fakeExec([
    { match: isTagList('spec/v*.*.*'), out: tags },
    { match: (a) => a.includes('diff') && a.includes('--name-status'), out: diff },
    { match: (a) => a.includes('ls-tree'), out: lsTree },
    { match: (a) => a.includes('log') && a.includes('--format=%H') && !a.includes('%x00'), out: shas },
    { match: (a) => a.includes('log') && a.some((x) => x.startsWith('--format=%H%x00')), out: log },
    {
      match: (a) => a.includes('show'),
      out: (a) => shows[a[a.length - 1]] || '',
    },
  ]);
  return exec;
}

describe('planSpec', () => {
  it('bootstrap: no spec tag → tag = declared version (initial release)', () => {
    const exec = planEnv({ lsTree: 'spec/README.md\nspec/ai-sdlc.schema.json\n' });
    const plan = planSpec(exec, { cwd: '/r', ref: 'HEAD', readme: readmeFile('1.0.0') });
    assert.equal(plan.status, 'planned');
    assert.equal(plan.bump, 'initial');
    assert.equal(plan.tag, 'spec/v1.0.0');
    assert.equal(plan.base_tag, null);
  });

  it('bootstrap without a declared version fails closed', () => {
    const f = path.join(fs.mkdtempSync(path.join(os.tmpdir(), 'sv-')), 'README.md');
    fs.writeFileSync(f, 'no marker\n');
    const exec = planEnv({ lsTree: 'spec/README.md\n' });
    assert.throws(() => planSpec(exec, { cwd: '/r', ref: 'HEAD', readme: f }), /declares no/);
  });

  it('docs-only spec diff → patch; declared must equal computed next', () => {
    const exec = planEnv({ tags: 'spec/v1.0.0\n', diff: 'M\tspec/README.md\n' });
    const plan = planSpec(exec, { cwd: '/r', ref: 'HEAD', readme: readmeFile('1.0.1') });
    assert.equal(plan.bump, 'patch');
    assert.equal(plan.tag, 'spec/v1.0.1');
    assert.equal(plan.verified, true);
    assert.equal(plan.status, 'planned');
  });

  it('schema breaking diff → major + declared-mismatch when README lags', () => {
    const exec = planEnv({
      tags: 'spec/v1.0.0\n',
      diff: 'M\tspec/ai-sdlc.schema.json\n',
      shows: {
        'spec/v1.0.0:spec/ai-sdlc.schema.json': '{"type":"object","properties":{"a":{}}}',
        'HEAD:spec/ai-sdlc.schema.json': '{"type":"object","properties":{}}',
      },
    });
    const ok = planSpec(exec, { cwd: '/r', ref: 'HEAD', readme: readmeFile('2.0.0') });
    assert.equal(ok.bump, 'major');
    assert.equal(ok.tag, 'spec/v2.0.0');
    assert.equal(ok.status, 'planned');

    const bad = planSpec(exec, { cwd: '/r', ref: 'HEAD', readme: readmeFile('1.1.0') });
    assert.equal(bad.status, 'declared-mismatch');
    assert.equal(bad.verified, false);
  });

  it('BREAKING CHANGE commit touching spec/ forces major over an additive diff', () => {
    const exec = planEnv({
      tags: 'spec/v1.0.0\n',
      diff: 'A\tspec/new-doc.md\n',
      shas: 'deadbeef\n',
      log: 'deadbeef\x00fix(spec): tweak\x00BREAKING CHANGE: renames label state\x1e',
    });
    const plan = planSpec(exec, { cwd: '/r', ref: 'HEAD', readme: readmeFile('2.0.0') });
    assert.equal(plan.diff_bump, 'minor');
    assert.equal(plan.commit_bump, 'major');
    assert.equal(plan.bump, 'major');
    assert.equal(plan.tag, 'spec/v2.0.0');
  });

  it('--bump override wins; --no-verify-declared skips the gate', () => {
    const exec = planEnv({ tags: 'spec/v1.0.0\n', diff: 'M\tspec/README.md\n' });
    const plan = planSpec(exec, {
      cwd: '/r', ref: 'HEAD', bump: 'major', verifyDeclared: false,
    });
    assert.equal(plan.bump, 'major');
    assert.equal(plan.tag, 'spec/v2.0.0');
    assert.equal(plan.verified, null);
  });

  it('empty spec diff → no-changes', () => {
    const exec = planEnv({ tags: 'spec/v1.0.0\n', diff: '' });
    const plan = planSpec(exec, { cwd: '/r', ref: 'HEAD', readme: readmeFile('1.0.1') });
    assert.equal(plan.status, 'no-changes');
    assert.equal(plan.tag, null);
  });
});

// --- Publish ------------------------------------------------------------------------

describe('publishSpec / renderSpecNotes', () => {
  const plan = (bump) => ({
    status: 'planned', repo: 'o/name', base_tag: 'spec/v1.0.0', ref: 'HEAD',
    bump, tag: 'spec/v2.0.0',
    files: [{ file: 'spec/ai-sdlc.schema.json', level: 'major', detail: '#/properties/a: removed' }],
  });

  it('tag → push → gh api release, in order; MAJOR notes carry the migration template', () => {
    const exec = fakeExec([
      { match: has('rev-parse'), out: '' },
      { match: has('releases'), out: '{"html_url":"https://x/spec/1"}' },
    ]);
    const res = publishSpec(exec, { cwd: '/r', repo: 'o/name', remote: 'origin', ref: 'HEAD' }, plan('major'));
    assert.equal(res.status, 'released');
    assert.equal(res.url, 'https://x/spec/1');
    const seq = exec.calls.map((a) => a.join(' '));
    assert.ok(seq.findIndex((s) => s.includes('tag -a spec/v2.0.0')) > -1);
    assert.ok(seq.findIndex((s) => s.includes('push origin refs/tags/spec/v2.0.0')) > -1);
    assert.ok(seq.some((s) => s.includes('Migration note') && s.includes('Consumer action required')));
  });

  it('existing tag → status exists, no mutations', () => {
    const exec = fakeExec([{ match: has('rev-parse'), out: 'deadbeef\n' }]);
    const res = publishSpec(exec, { cwd: '/r', repo: 'o/name' }, plan('minor'));
    assert.equal(res.status, 'exists');
    assert.ok(!exec.calls.some((a) => a.includes('-a')));
    assert.ok(!exec.calls.some((a) => a[0] === 'gh'));
  });
});

// --- main() end-to-end --------------------------------------------------------------

describe('main', () => {
  function cliEnv({ tags = '', diff = '', lsTree = '' } = {}) {
    const exec = fakeExec([
      { match: has('rev-parse --show-toplevel'), out: '/r\n' },
      { match: isTagList('spec/v*.*.*'), out: tags },
      { match: (a) => a.includes('--name-status'), out: diff },
      { match: (a) => a.includes('ls-tree'), out: lsTree },
      { match: (a) => a.includes('log'), out: '' },
      { match: has('rev-parse'), out: '' },
    ]);
    const out = [];
    return { exec, stdout: (s) => out.push(s), out };
  }

  it('default dry-run prints the plan and never mutates', () => {
    const { exec, stdout } = cliEnv({ tags: 'spec/v1.0.0\n', diff: 'M\tspec/README.md\n' });
    const res = main(['--json', '--readme', readmeFile('1.0.1')], { exec, stdout, env: {} });
    assert.equal(res.dry_run, true);
    assert.equal(res.tag, 'spec/v1.0.1');
    assert.ok(!exec.calls.some((a) => a.includes('-a') || a.includes('push')));
  });

  it('declared mismatch exits through the error path', () => {
    const { exec, stdout } = cliEnv({ tags: 'spec/v1.0.0\n', diff: 'M\tspec/README.md\n' });
    assert.throws(
      () => main(['--readme', readmeFile('9.9.9')], { exec, stdout, env: {} }),
      /declared version gate failed/,
    );
  });

  it('--publish tags, pushes and releases end-to-end', () => {
    const { exec, stdout } = cliEnv({ tags: 'spec/v1.0.0\n', diff: 'M\tspec/README.md\n' });
    const res = main(
      ['--publish', '--repo', 'o/name', '--json', '--readme', readmeFile('1.0.1')],
      { exec, stdout, env: {} },
    );
    assert.equal(res.status, 'released');
    assert.ok(exec.calls.some((a) => a.join(' ').includes('tag -a spec/v1.0.1')));
    assert.ok(exec.calls.some((a) => a.join(' ').includes('push origin refs/tags/spec/v1.0.1')));
  });

  it('--github-output appends spec_* keys', () => {
    const { exec, stdout } = cliEnv({ tags: 'spec/v1.0.0\n', diff: 'M\tspec/README.md\n' });
    const f = path.join(fs.mkdtempSync(path.join(os.tmpdir(), 'sv-')), 'out');
    main(['--github-output', '--readme', readmeFile('1.0.1')], { exec, stdout, env: { GITHUB_OUTPUT: f } });
    const text = fs.readFileSync(f, 'utf8');
    assert.match(text, /spec_status=planned/);
    assert.match(text, /spec_tag=spec\/v1\.0\.1/);
    assert.match(text, /spec_verified=true/);
  });

  it('bad args reject', () => {
    assert.throws(() => parseArgs(['--bogus'], {}), /unknown argument/);
    assert.throws(() => parseArgs(['--bump', 'huge'], {}), /invalid --bump/);
  });
});
