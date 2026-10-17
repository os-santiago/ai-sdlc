// scripts/release-manager.test.js — unit + mocked-exec e2e tests for the
// semver release driver (issue #178). Every test runs in dry-run or against
// a fake exec seam — no real tags, pushes, or GitHub releases are created.
'use strict';

const { describe, it } = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const os = require('os');
const path = require('path');

const {
  SPEC_GLOB,
  parseConventional,
  classifyCommit,
  computeBump,
  parseTag,
  compareVersions,
  nextVersion,
  parseGitLog,
  renderChangelog,
  latestTag,
  planRelease,
  publishRelease,
  parseArgs,
  main,
} = require('./release-manager');

const commit = (sha, subject, body = '') => ({ sha, subject, body });
const logText = (commits) =>
  commits.map((c) => `${c.sha}\x00${c.subject}\x00${c.body}\x1e`).join('');

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

// --- Pure parsing / semver ----------------------------------------------------

describe('parseConventional / classifyCommit', () => {
  it('parses type, scope, breaking marker, subject', () => {
    const c = parseConventional('feat(runtime)!: drop legacy intake');
    assert.deepEqual(c, { type: 'feat', scope: 'runtime', breaking: true, subject: 'drop legacy intake' });
  });

  it('non-conventional subjects classify as other', () => {
    assert.equal(classifyCommit(commit('a', 'misc cleanup')), 'other');
    assert.equal(classifyCommit(commit('a', 'feat: x')), 'feat');
    assert.equal(classifyCommit(commit('a', 'fix(api): y')), 'fix');
  });

  it('BREAKING CHANGE / BREAKING-CHANGE in body forces breaking', () => {
    assert.equal(classifyCommit(commit('a', 'fix: x', 'BREAKING CHANGE: drops v0 input')), 'breaking');
    assert.equal(classifyCommit(commit('a', 'docs: y', 'BREAKING-CHANGE: see migration')), 'breaking');
  });
});

describe('computeBump', () => {
  it('breaking → major, feat → minor, else patch', () => {
    assert.equal(computeBump([commit('a', 'feat!: x')]), 'major');
    assert.equal(computeBump([commit('a', 'feat: x'), commit('b', 'fix: y')]), 'minor');
    assert.equal(computeBump([commit('a', 'fix: x'), commit('b', 'chore: y')]), 'patch');
  });

  it('explicit override wins over history', () => {
    assert.equal(computeBump([commit('a', 'feat!: x')], 'patch'), 'patch');
    assert.equal(computeBump([], 'major'), 'major');
  });
});

describe('tag streams', () => {
  it('parseTag separates runtime v* from spec/v*', () => {
    assert.deepEqual(parseTag('v1.2.3'), { spec: false, version: [1, 2, 3] });
    assert.deepEqual(parseTag('spec/v2.0.1'), { spec: true, version: [2, 0, 1] });
    assert.equal(parseTag('release-2026'), null);
    assert.equal(parseTag('v1.2'), null);
  });

  it('nextVersion bumps the right digit', () => {
    assert.deepEqual(nextVersion([1, 2, 3], 'major'), [2, 0, 0]);
    assert.deepEqual(nextVersion([1, 2, 3], 'minor'), [1, 3, 0]);
    assert.deepEqual(nextVersion([1, 2, 3], 'patch'), [1, 2, 4]);
    assert.deepEqual(nextVersion(null, 'patch'), [0, 0, 1]);
  });

  it('latestTag picks max semver and ignores non-semver names', () => {
    const exec = fakeExec([
      { match: isTagList('v*.*.*'), out: 'v0.9.9\nv0.10.0\nnightly-v1\nv0.2.0\n' },
      { match: isTagList('spec/v*.*.*'), out: 'spec/v1.0.0\n' },
    ]);
    assert.equal(latestTag(exec, '/r', 'v*.*.*', /^v(\d+)\.(\d+)\.(\d+)$/).name, 'v0.10.0');
    assert.equal(latestTag(exec, '/r', 'spec/v*.*.*', /^spec\/v(\d+)\.(\d+)\.(\d+)$/).name, 'spec/v1.0.0');
    assert.ok(compareVersions([0, 10, 0], [0, 9, 9]) > 0);
  });
});

describe('parseGitLog', () => {
  it('decodes NUL fields and RS records', () => {
    const commits = parseGitLog(logText([
      commit('abc123', 'feat: a', 'body\nline2'),
      commit('def456', 'fix: b'),
    ]));
    assert.equal(commits.length, 2);
    assert.equal(commits[0].sha, 'abc123');
    assert.equal(commits[0].subject, 'feat: a');
    assert.match(commits[0].body, /line2/);
  });
});

// --- Plan computation (fake exec — no mutations) --------------------------------

function planEnv({ tags = '', specTags = '', commits = [], specShas = [] } = {}) {
  const exec = fakeExec([
    { match: isTagList('v*.*.*'), out: tags },
    { match: isTagList('spec/v*.*.*'), out: specTags },
    {
      match: (a) => a.includes('log') && a.some((x) => x.startsWith('--format=%H%x00')),
      out: logText(commits),
    },
    {
      match: (a) => a.includes('log') && a.includes('--format=%H') && a.includes(SPEC_GLOB),
      out: specShas.join('\n'),
    },
    { match: has('rev-parse'), out: '' },
  ]);
  return { exec, opts: { cwd: '/r', repo: 'o/name', ref: 'HEAD' } };
}

describe('planRelease', () => {
  it('computes minor bump + tag from conventional commits since base', () => {
    const { exec, opts } = planEnv({
      tags: 'v1.2.0\n',
      commits: [commit('a1', 'feat(runtime): release manager'), commit('a2', 'fix: typo')],
    });
    const plan = planRelease(exec, opts);
    assert.equal(plan.status, 'planned');
    assert.equal(plan.base_tag, 'v1.2.0');
    assert.equal(plan.bump, 'minor');
    assert.equal(plan.tag, 'v1.3.0');
    assert.equal(plan.commits, 2);
    assert.equal(plan.spec.touched, false);
    assert.match(plan.changelog, /feat\(runtime\): release manager/);
  });

  it('first release (no base tag) starts at 0.1.0/0.0.1 and covers all history', () => {
    const { exec, opts } = planEnv({ commits: [commit('a1', 'feat: bootstrap')] });
    const plan = planRelease(exec, opts);
    assert.equal(plan.tag, 'v0.1.0');
    assert.equal(plan.base_tag, null);
  });

  it('no commits → no-changes unless force', () => {
    const { exec, opts } = planEnv({ tags: 'v1.0.0\n' });
    assert.equal(planRelease(exec, opts).status, 'no-changes');
    assert.equal(planRelease(exec, { ...opts, force: true }).status, 'planned');
  });

  it('spec-touching commits drive a separate spec/v* bump', () => {
    const { exec, opts } = planEnv({
      tags: 'v1.2.0\n',
      specTags: 'spec/v1.0.0\n',
      commits: [
        commit('a1', 'feat(spec)!: rename contract field', 'BREAKING CHANGE: field renamed'),
        commit('a2', 'fix(docs): typo'),
      ],
      specShas: ['a1'],
    });
    const plan = planRelease(exec, opts);
    assert.equal(plan.bump, 'major'); // runtime sees the breaking commit too
    assert.equal(plan.tag, 'v2.0.0');
    assert.equal(plan.spec.touched, true);
    assert.equal(plan.spec.bump, 'major');
    assert.equal(plan.spec.tag, 'spec/v2.0.0');
    assert.match(plan.changelog, /spec\/v2\.0\.0/);
    assert.match(plan.changelog, /migration note/i);
  });

  it('spec bump is computed from spec-touching commits only', () => {
    const { exec, opts } = planEnv({
      tags: 'v1.0.0\n',
      specTags: 'spec/v3.0.0\n',
      commits: [
        commit('a1', 'feat(runtime): big thing'), // runtime feat — must not inflate spec
        commit('a2', 'docs(spec): clarify wording'),
      ],
      specShas: ['a2'],
    });
    const plan = planRelease(exec, opts);
    assert.equal(plan.bump, 'minor');
    assert.equal(plan.spec.bump, 'patch'); // docs → patch, not minor
    assert.equal(plan.spec.tag, 'spec/v3.0.1');
  });

  it('--spec-bump none suppresses the spec tag even when spec/ changed', () => {
    const { exec, opts } = planEnv({
      commits: [commit('a1', 'feat(spec): new field')],
      specShas: ['a1'],
    });
    const plan = planRelease(exec, { ...opts, specBump: 'none' });
    assert.equal(plan.spec.touched, true);
    assert.equal(plan.spec.tag, null);
  });
});

// --- Publish path (fake exec — asserts the mutation sequence) --------------------

describe('publishRelease', () => {
  const planned = () => ({
    status: 'planned', repo: 'o/name', base_tag: 'v1.0.0', ref: 'HEAD',
    commits: 1, bump: 'minor', tag: 'v1.1.0',
    spec: { touched: true, base_tag: 'spec/v1.0.0', bump: 'patch', tag: 'spec/v1.0.1' },
    changelog: '## What\'s Changed\n\n* feat: x (abc1234)\n',
  });

  it('tag → push → spec tag → push → gh api release, in order', () => {
    const exec = fakeExec([
      { match: has('rev-parse'), out: '' },   // tag does not exist yet
      { match: has('repos/o/name/releases'), out: '{"html_url":"https://x/r/1"}' },
    ]);
    const res = publishRelease(exec, { cwd: '/r', repo: 'o/name', remote: 'origin', ref: 'HEAD' }, planned());
    assert.equal(res.status, 'released');
    assert.equal(res.url, 'https://x/r/1');

    const seq = exec.calls.map((a) => a.join(' '));
    const iTag = seq.findIndex((s) => s.includes('tag -a v1.1.0'));
    const iPush = seq.findIndex((s) => s.includes('push origin refs/tags/v1.1.0'));
    const iSpec = seq.findIndex((s) => s.includes('tag -a spec/v1.0.1'));
    const iSpecPush = seq.findIndex((s) => s.includes('push origin refs/tags/spec/v1.0.1'));
    const iRel = seq.findIndex((s) => s.includes('POST') && s.includes('releases'));
    assert.ok(iTag > -1 && iPush > iTag && iSpec > iPush && iSpecPush > iSpec && iRel > iSpecPush);
    const relCall = exec.calls[iRel].join(' ');
    assert.match(relCall, /tag_name=v1\.1\.0/);
    assert.match(relCall, /draft=false/);
  });

  it('refuses to re-release an existing tag', () => {
    const exec = fakeExec([{ match: has('rev-parse'), out: 'deadbeef\n' }]);
    assert.throws(
      () => publishRelease(exec, { cwd: '/r', repo: 'o/name', remote: 'origin' }, planned()),
      /already exists/,
    );
    // no tag/push/release mutations were attempted
    assert.equal(exec.calls.filter((a) => a.includes('tag') && a.includes('-a')).length, 0);
  });

  it('skips an existing spec tag but still releases', () => {
    const exec = fakeExec([
      { match: (a) => a.join(' ').includes('refs/tags/spec/'), out: 'deadbeef\n' },
      { match: has('rev-parse'), out: '' },
      { match: has('releases'), out: '{}' },
    ]);
    const plan = planned();
    publishRelease(exec, { cwd: '/r', repo: 'o/name', remote: 'origin' }, plan);
    assert.equal(plan.spec.pushed, false);
    assert.equal(exec.calls.filter((a) => a.join(' ').includes('tag -a spec/')).length, 0);
  });

  it('requires --repo for the GitHub release', () => {
    const exec = fakeExec([{ match: has('rev-parse'), out: '' }]);
    assert.throws(
      () => publishRelease(exec, { cwd: '/r', remote: 'origin' }, planned()),
      /--repo/,
    );
  });
});

// --- main() end-to-end via injected exec -----------------------------------------

describe('main — dry-run is the default and mutates nothing', () => {
  function cliEnv({ commits = [commit('a1', 'feat: x')], specShas = [] } = {}) {
    const exec = fakeExec([
      { match: has('rev-parse --show-toplevel'), out: '/r\n' },
      { match: isTagList('v*.*.*'), out: 'v1.0.0\n' },
      { match: isTagList('spec/v*.*.*'), out: '' },
      { match: (a) => a.some((x) => x.startsWith('--format=%H%x00')), out: logText(commits) },
      { match: (a) => a.includes('--format=%H') && a.includes(SPEC_GLOB), out: specShas.join('\n') },
      { match: has('rev-parse'), out: '' },
    ]);
    const out = [];
    return { exec, stdout: (s) => out.push(s), out };
  }

  it('default invocation prints a plan and never calls tag/push/gh', () => {
    const { exec, stdout, out } = cliEnv();
    const res = main(['--json'], { exec, stdout, env: {} });
    assert.equal(res.dry_run, true);
    assert.equal(res.tag, 'v1.1.0');
    assert.ok(!exec.calls.some((a) => a.includes('-a')));
    assert.ok(!exec.calls.some((a) => a.includes('push')));
    assert.ok(!exec.calls.some((a) => a[0] === 'gh'));
    assert.ok(out.length === 1);
  });

  it('--dry-run and --publish both parse; unknown args reject', () => {
    const { exec, stdout } = cliEnv();
    const res = main(['--dry-run'], { exec, stdout, env: {} });
    assert.equal(res.dry_run, true);
    assert.throws(() => parseArgs(['--bogus'], {}), /unknown argument/);
    assert.throws(() => parseArgs(['--bump', 'huge'], {}), /invalid --bump/);
  });

  it('--publish end-to-end performs tag+release mutations', () => {
    const { exec, stdout } = cliEnv();
    const res = main(['--publish', '--repo', 'o/name', '--json'], { exec, stdout, env: {} });
    assert.equal(res.status, 'released');
    assert.ok(exec.calls.some((a) => a.join(' ').includes('tag -a v1.1.0')));
    assert.ok(exec.calls.some((a) => a.join(' ').includes('push origin refs/tags/v1.1.0')));
    assert.ok(exec.calls.some((a) => a[0] === 'gh' && a.some((x) => x.includes('releases'))));
  });

  it('no-changes under --publish does not mutate', () => {
    const { exec, stdout } = cliEnv({ commits: [] });
    const res = main(['--publish', '--repo', 'o/name', '--json'], { exec, stdout, env: {} });
    assert.equal(res.status, 'no-changes');
    assert.ok(!exec.calls.some((a) => a[0] === 'gh'));
  });

  it('--changelog-file writes the rendered notes', () => {
    const { exec, stdout } = cliEnv();
    const f = path.join(fs.mkdtempSync(path.join(os.tmpdir(), 'rel-')), 'notes.md');
    main(['--changelog-file', f], { exec, stdout, env: {} });
    assert.match(fs.readFileSync(f, 'utf8'), /What's Changed/);
  });

  it('--github-output appends release_* keys', () => {
    const { exec, stdout } = cliEnv();
    const f = path.join(fs.mkdtempSync(path.join(os.tmpdir(), 'rel-')), 'out');
    main(['--github-output'], { exec, stdout, env: { GITHUB_OUTPUT: f } });
    const text = fs.readFileSync(f, 'utf8');
    assert.match(text, /release_status=planned/);
    assert.match(text, /release_tag=v1\.1\.0/);
  });
});
