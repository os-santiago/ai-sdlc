// scripts/apply-patch.test.js — unit + e2e tests for the deterministic
// apply gate (issue #212, credential isolation). Covers the issue's
// negative-test acceptance criterion: malicious paths, workflow paths,
// oversized diffs, non-patch input, symlink/gitlink/binary payloads and
// forged context are all rejected; only validated output can apply.
'use strict';

const { describe, it } = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { execFileSync } = require('child_process');

const {
  globToRegExp,
  unquoteGitPath,
  pathViolation,
  stripDiffPrefix,
  splitDiffHeader,
  parsePatch,
  validate,
  main,
} = require('./apply-patch');

// --- fixtures -----------------------------------------------------------------
const hdr = (a, b) => `diff --git a/${a} b/${b}`;
// Hunks match what `git apply` expects per op: added files have no '-' side,
// deleted files no '+' side — the e2e tests run real `git apply --check`.
const BODIES = {
  modified: ['@@ -1,1 +1,1 @@', '-old', '+new'],
  added: ['@@ -0,0 +1 @@', '+new'],
  deleted: ['@@ -1 +0,0 @@', '-old'],
};
const patch = (file, { op = 'modified', mode = '100644', body } = {}) => {
  const lines = [hdr(file, file)];
  if (op === 'added') lines.push(`new file mode ${mode}`, 'index 0000000..1111111');
  else if (op === 'deleted') lines.push(`deleted file mode ${mode}`, 'index 1111111..0000000');
  else lines.push('index 1111111..2222222 100644');
  lines.push(`--- ${op === 'added' ? '/dev/null' : 'a/' + file}`);
  lines.push(`+++ ${op === 'deleted' ? '/dev/null' : 'b/' + file}`);
  lines.push(...(body || BODIES[op]));
  return lines.join('\n') + '\n';
};

const reasons = (r) => r.rejects.map((x) => x.reason);

// --- path hygiene -------------------------------------------------------------
describe('pathViolation — malicious path rejection', () => {
  const bad = [
    ['../escape.sh', 'dotdot-traversal'],
    ['a/../../etc/passwd', 'dotdot-traversal'],
    ['/abs/path.sh', 'absolute-path'],
    ['C:/win/evil', 'windows-drive-path'],
    ['.git/hooks/pre-push', 'git-internal-path'],
    ['a/.GIT/config', 'git-internal-path'],
    ['x\\..\\evil', 'backslash-in-path'],
    ['a//b', 'empty-path-segment'],
    ['./x', 'dot-path-segment'],
    ['tab\tname', 'control-character-in-path'],
  ];
  for (const [p, why] of bad) {
    it(`rejects ${JSON.stringify(p)} (${why})`, () => {
      assert.equal(pathViolation(p), why);
    });
  }

  it('accepts ordinary repo paths', () => {
    for (const p of ['src/x.js', 'docs/a-b_c.md', '.github/actions/x/action.yml', '.gitignore', '.gitattributes']) {
      assert.equal(pathViolation(p), null, p);
    }
  });
});

// --- header parsing -------------------------------------------------------------
describe('header parsing', () => {
  it('globToRegExp: ** spans dirs, * does not cross /', () => {
    assert.ok(globToRegExp('.github/workflows/**').test('.github/workflows/ci.yml'));
    assert.ok(globToRegExp('.github/workflows/**').test('.github/workflows/a/b.yml'));
    assert.ok(!globToRegExp('*.js').test('a/b.js'));
    // case-insensitive — case tricks must not bypass the workflow deny
    assert.ok(globToRegExp('.github/workflows/**').test('.GitHub/Workflows/ci.yml'));
  });

  it('unquoteGitPath decodes C-style escapes', () => {
    assert.equal(unquoteGitPath('"a/x\\ty"'), 'a/x\ty');
    assert.equal(unquoteGitPath('"a/\\303\\251"'), 'a/é');
    assert.throws(() => unquoteGitPath('"unterminated'));
  });

  it('stripDiffPrefix strips a//b/ and returns null for /dev/null', () => {
    assert.equal(stripDiffPrefix('a/src/x.js'), 'src/x.js');
    assert.equal(stripDiffPrefix('"b/q\\tuoted.js"'), 'q\tuoted.js');
    assert.equal(stripDiffPrefix('/dev/null'), null);
    assert.throws(() => stripDiffPrefix('no-prefix'));
  });

  it('splitDiffHeader handles plain and quoted operands', () => {
    assert.deepEqual(splitDiffHeader('a/x b/y'), ['a/x', 'b/y']);
    assert.deepEqual(splitDiffHeader('"a/x\\ty" b/z'), ['"a/x\\ty"', 'b/z']);
    // a quoted old path containing ' b/' must not split early
    assert.deepEqual(splitDiffHeader('"a/p b/q" "b/p b/q"'), ['"a/p b/q"', '"b/p b/q"']);
    assert.throws(() => splitDiffHeader('a/x'));
  });
});

// --- validation: forbidden surfaces ---------------------------------------------
describe('validate — forbidden surfaces rejected', () => {
  it('rejects workflow-file writes (builtin deny)', () => {
    const r = validate(patch('.github/workflows/evil.yml', { op: 'added' }));
    assert.equal(r.verdict, 'rejected');
    assert.ok(reasons(r).some((x) => x.startsWith('forbidden-path')));
  });

  it('rejects workflow paths case-insensitively', () => {
    const r = validate(patch('.GitHub/Workflows/evil.yml', { op: 'added' }));
    assert.equal(r.verdict, 'rejected');
  });

  it('rejects dotdot escape into workflows', () => {
    const r = validate(patch('docs/../../.github/workflows/e.yml', { op: 'added' }));
    assert.equal(r.verdict, 'rejected');
    assert.ok(reasons(r).includes('dotdot-traversal'));
  });

  it('rejects symlink payloads (mode 120000)', () => {
    const p = [
      hdr('link', 'link'),
      'new file mode 120000',
      'index 0000000..1111111',
      '--- /dev/null',
      '+++ b/link',
      '@@ -0,0 +1 @@',
      '+/etc/shadow',
    ].join('\n') + '\n';
    const r = validate(p);
    assert.equal(r.verdict, 'rejected');
    assert.ok(reasons(r).some((x) => x.includes('symlink')));
  });

  it('rejects gitlink/submodule payloads (mode 160000)', () => {
    const p = [hdr('sub', 'sub'), 'new file mode 160000', 'index 0000000..1111111'].join('\n') + '\n';
    const r = validate(p);
    assert.equal(r.verdict, 'rejected');
    assert.ok(reasons(r).some((x) => x.includes('gitlink')));
  });

  it('rejects binary patches — opaque bytes are not auditable', () => {
    for (const marker of ['GIT binary patch\nliteral 4\nabcd\n', 'Binary files a/x.png and b/x.png differ']) {
      const p = [hdr('x.png', 'x.png'), 'index 1111111..2222222', marker].join('\n') + '\n';
      const r = validate(p);
      assert.equal(r.verdict, 'rejected', marker.split('\n')[0]);
      assert.ok(reasons(r).includes('binary-patch-not-allowed'));
    }
  });

  it('rejects non-patch input', () => {
    for (const input of ['', 'hello world\n', 'From abc Mon Sep 17 00:00:00 2001\nSubject: x\n---\n', '  diff --git a/x b/x\n']) {
      const r = validate(input);
      assert.equal(r.verdict, 'rejected', JSON.stringify(input.slice(0, 20)));
    }
  });

  it('rejects empty patch', () => {
    const r = validate('');
    assert.equal(r.verdict, 'rejected');
  });

  it('rejects malformed hunks and count mismatch', () => {
    const bad = [
      hdr('x', 'x'), 'index 1..2 100644', '--- a/x', '+++ b/x',
      '@@ -1,1 +1,1 @@', '-a', '+b', '+stray',
    ].join('\n') + '\n';
    const r = validate(bad);
    assert.equal(r.verdict, 'rejected');
    assert.ok(reasons(r).some((x) => x.startsWith('hunk-count-mismatch')));
  });

  it('rejects trailing garbage after a clean section', () => {
    const r = validate(patch('x.js') + 'EVIL TRAILING LINE\n');
    assert.equal(r.verdict, 'rejected');
  });

  it('rejects header/--- operand mismatch (smuggling attempt)', () => {
    const p = [
      hdr('docs/a.md', 'docs/a.md'),
      'index 1..2 100644',
      '--- a/.github/workflows/evil.yml',   // mismatch vs diff --git header
      '+++ b/docs/a.md',
      '@@ -1,1 +1,1 @@', '-a', '+b',
    ].join('\n') + '\n';
    const r = validate(p);
    assert.equal(r.verdict, 'rejected');
    assert.ok(reasons(r).includes('old-path-mismatch'));
    // ...and the smuggled path is still caught by hygiene/deny
    assert.ok(reasons(r).some((x) => x.startsWith('forbidden-path')));
  });

  it('enforces --allow allowlist when provided', () => {
    const ok = validate(patch('docs/a.md'), { allow: ['docs/**'] });
    assert.equal(ok.verdict, 'validated');
    const no = validate(patch('src/x.js'), { allow: ['docs/**'] });
    assert.equal(no.verdict, 'rejected');
    assert.ok(reasons(no).includes('path-not-in-allowlist'));
  });

  it('enforces --max-files bound', () => {
    const p = Array.from({ length: 5 }, (_, i) => patch(`f${i}.js`, { op: 'added' })).join('');
    const r = validate(p, { maxFiles: 3 });
    assert.equal(r.verdict, 'rejected');
    assert.ok(reasons(r).some((x) => x.startsWith('too-many-files')));
  });

  it('extra --deny globs compose with the builtin workflow deny', () => {
    const r = validate(patch('secrets/prod.pem'), { deny: ['secrets/**'] });
    assert.equal(r.verdict, 'rejected');
  });
});

// --- validation: clean patches pass ---------------------------------------------
describe('validate — clean engine output accepted', () => {
  it('validates a multi-file diff and reports per-file stats', () => {
    const p = patch('src/a.js') + patch('docs/b.md', { op: 'added' }) + patch('old/c.txt', { op: 'deleted' });
    const r = validate(p);
    assert.equal(r.verdict, 'validated');
    assert.equal(r.files.length, 3);
    assert.deepEqual(r.files.map((f) => f.op), ['modified', 'added', 'deleted']);
    assert.equal(r.files[0].added, 1);
  });

  it('accepts a pure rename (no hunks)', () => {
    const p = [
      hdr('old.js', 'new.js'),
      'similarity index 100%',
      'rename from old.js',
      'rename to new.js',
    ].join('\n') + '\n';
    const r = validate(p);
    assert.equal(r.verdict, 'validated');
    assert.equal(r.files[0].op, 'renamed');
    assert.equal(r.files[0].oldPath, 'old.js');
    assert.equal(r.files[0].path, 'new.js');
  });

  it('accepts a rename out of a denied dir when the destination is clean', () => {
    const p = [
      hdr('.github/workflows/old.yml', 'docs/old.md'),
      'similarity index 90%',
      'rename from .github/workflows/old.yml',
      'rename to docs/old.md',
    ].join('\n') + '\n';
    const r = validate(p);
    // the denied source path is still touched → rejected (can't smuggle
    // workflow content out through the rename vector either)
    assert.equal(r.verdict, 'rejected');
  });

  it('accepts exec-bit mode changes on regular files', () => {
    const p = [hdr('run.sh', 'run.sh'), 'old mode 100644', 'new mode 100755'].join('\n') + '\n';
    const r = validate(p);
    assert.equal(r.verdict, 'validated');
  });

  it('accepts deleting a symlink (only forward-state modes are gated)', () => {
    const p = [hdr('link', 'link'), 'deleted file mode 120000', 'index 1..0'].join('\n') + '\n';
    const r = validate(p);
    assert.equal(r.verdict, 'validated');
  });
});

// --- e2e: real git repo, real git apply -------------------------------------------
describe('e2e — validated patch applies; rejected patch never touches the tree', () => {
  function repo() {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'ap-'));
    execFileSync('git', ['init', '-q'], { cwd: dir });
    execFileSync('git', ['config', 'user.email', 't@t'], { cwd: dir });
    execFileSync('git', ['config', 'user.name', 't'], { cwd: dir });
    fs.writeFileSync(path.join(dir, 'x.js'), 'old\n');
    execFileSync('git', ['add', '.'], { cwd: dir });
    execFileSync('git', ['commit', '-qm', 'init'], { cwd: dir });
    return dir;
  }
  const write = (dir, name, text) => {
    const f = path.join(dir, name);
    fs.writeFileSync(f, text);
    return f;
  };

  it('applies a clean patch and reports the audit', () => {
    const dir = repo();
    const pf = write(dir, 'e.patch', patch('x.js') + patch('new.md', { op: 'added' }));
    const af = path.join(dir, 'audit.json');
    let out = '';
    const r = main(['--patch', pf, '--repo', dir, '--audit', af, '--json'], { stdout: (s) => { out = s; } });
    assert.equal(r.verdict, 'applied');
    assert.equal(fs.readFileSync(path.join(dir, 'x.js'), 'utf8'), 'new\n');
    assert.equal(fs.readFileSync(path.join(dir, 'new.md'), 'utf8'), 'new\n');
    const audit = JSON.parse(fs.readFileSync(af, 'utf8'));
    assert.equal(audit.files.length, 2);
    assert.ok(audit.sha256);
    assert.deepEqual(JSON.parse(out).verdict, 'applied');
  });

  it('--check-only validates without mutating the worktree', () => {
    const dir = repo();
    const pf = write(dir, 'e.patch', patch('x.js'));
    const r = main(['--patch', pf, '--repo', dir, '--check-only']);
    assert.equal(r.verdict, 'validated');
    assert.equal(fs.readFileSync(path.join(dir, 'x.js'), 'utf8'), 'old\n');
  });

  it('rejected patch leaves the worktree untouched', () => {
    const dir = repo();
    const pf = write(dir, 'e.patch', patch('.github/workflows/evil.yml', { op: 'added' }));
    const r = main(['--patch', pf, '--repo', dir]);
    assert.equal(r.verdict, 'rejected');
    assert.ok(!fs.existsSync(path.join(dir, '.github')));
  });

  it('patch failing git apply --check is a rejection (stale/forged context)', () => {
    const dir = repo();
    // valid structure, but the 'old' context doesn't match the worktree
    const pf = write(dir, 'e.patch', [
      hdr('x.js', 'x.js'), 'index 1..2 100644', '--- a/x.js', '+++ b/x.js',
      '@@ -1,1 +1,1 @@', '-different', '+new',
    ].join('\n') + '\n');
    const r = main(['--patch', pf, '--repo', dir]);
    assert.equal(r.verdict, 'rejected');
    assert.ok(reasons(r).some((x) => x.startsWith('apply-check-failed')));
    assert.equal(fs.readFileSync(path.join(dir, 'x.js'), 'utf8'), 'old\n');
  });

  it('oversized patch is rejected before parsing', () => {
    const dir = repo();
    const pf = write(dir, 'big.patch', 'x'.repeat(3000));
    const r = main(['--patch', pf, '--repo', dir, '--max-bytes', '100']);
    assert.equal(r.verdict, 'rejected');
    assert.ok(reasons(r).some((x) => x.startsWith('patch-too-large')));
  });
});

// --- CLI exit surface --------------------------------------------------------------
describe('cli exit surface', () => {
  it('missing --patch is a usage error', () => {
    assert.throws(() => main([]), /--patch required/);
  });
});
