// scripts/dep-update.test.js — unit tests for the dependency update worker
// (issue #177). All registry/OSV metadata is injected fixture data — tests
// never touch the network (spec/injection-defense.md egress posture).
'use strict';

const { describe, it } = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const os = require('os');
const path = require('path');

const {
  parseSemverTriple,
  classifyVersionBump,
  parseNpmOutdated,
  classifyDepUpdateDiff,
  depUpdateVerdict,
  diffTouchesManifest,
  extractAddedDeps,
  findNameConfusion,
  vetUpdate,
  resolveEgress,
  makeFetcher,
  depSignature,
  buildPlan,
  buildPrBody,
  vetPrDeps,
  main,
  EGRESS_PROFILES,
} = require('./dep-update');

const NOW = Date.parse('2026-10-20T00:00:00Z');
const daysAgo = (n) => new Date(NOW - n * 86400000).toISOString();

const meta = (over = {}) => Object.assign(
  { exists: true, license: 'MIT', publishedAt: daysAgo(30), vulns: [] },
  over,
);

// --- semver classification --------------------------------------------------
describe('parseSemverTriple / classifyVersionBump', () => {
  it('parses plain and range-prefixed specs', () => {
    assert.deepEqual(parseSemverTriple('1.2.3'), { major: 1, minor: 2, patch: 3, suffix: '' });
    assert.deepEqual(parseSemverTriple('^1.2.3').major, 1);
    assert.deepEqual(parseSemverTriple('v2.0.0').major, 2);
    assert.deepEqual(parseSemverTriple('npm:pkg@1.2.3').patch, 3);
  });

  it('returns null for non-semver specs (fail-closed)', () => {
    for (const s of ['*', 'latest', 'next', 'https://x.test/t.tgz', 'git+ssh://x', 'workspace:*', '']) {
      assert.strictEqual(parseSemverTriple(s), null, s);
    }
  });

  it('classifies patch / minor / major / none / unknown', () => {
    assert.equal(classifyVersionBump('1.2.3', '1.2.4'), 'patch');
    assert.equal(classifyVersionBump('1.2.3', '1.3.0'), 'minor');
    assert.equal(classifyVersionBump('1.2.3', '2.0.0'), 'major');
    assert.equal(classifyVersionBump('^1.2.3', '~1.2.3'), 'none');
    assert.equal(classifyVersionBump('1.2.3', '1.2.3-rc.1'), 'patch');
    // Downgrades and unparseable specs are fail-closed unknown.
    assert.equal(classifyVersionBump('2.0.0', '1.9.9'), 'unknown');
    assert.equal(classifyVersionBump('1.2.3', 'latest'), 'unknown');
    assert.equal(classifyVersionBump('file:../x', '1.0.0'), 'unknown');
  });
});

// --- npm outdated parsing ----------------------------------------------------
describe('parseNpmOutdated', () => {
  const OUTDATED = JSON.stringify({
    'prod-lib': { current: '1.2.3', wanted: '1.2.9', latest: '2.0.0', type: 'dependencies' },
    'dev-lib': { current: '3.0.0', wanted: '3.1.0', latest: '3.1.0', type: 'devDependencies' },
    'gone': { current: 'missing', wanted: '1.0.0', latest: '1.0.0', type: 'dependencies' },
    'linked': { current: 'linked', wanted: '1.0.0', latest: '1.0.0', type: 'dependencies' },
    'same': { current: '1.0.0', wanted: '1.0.0', latest: '1.0.0', type: 'dependencies' },
  });

  it('parses candidates, drops missing/linked/unchanged, sorts prod-first', () => {
    const out = parseNpmOutdated(OUTDATED);
    assert.deepEqual(out.map(u => u.name), ['prod-lib', 'dev-lib']);
    assert.equal(out[0].target, '1.2.9'); // wanted default
    assert.equal(out[0].dev, false);
    assert.equal(out[1].dev, true);
  });

  it('target=latest crosses majors; includeDev=false drops devDeps', () => {
    const latest = parseNpmOutdated(OUTDATED, { target: 'latest' });
    assert.equal(latest[0].target, '2.0.0');
    const noDev = parseNpmOutdated(OUTDATED, { includeDev: false });
    assert.deepEqual(noDev.map(u => u.name), ['prod-lib']);
  });

  it('unparseable or empty input yields no candidates (fail-closed)', () => {
    assert.deepEqual(parseNpmOutdated('not json'), []);
    assert.deepEqual(parseNpmOutdated('{}'), []);
    assert.deepEqual(parseNpmOutdated('[1,2]'), []);
  });
});

// --- dep-diff classification (Hermes issue #54 semantics) --------------------
const DEP_DIFF = (pkgLines, extra = '') => [
  'diff --git a/package.json b/package.json',
  '--- a/package.json',
  '+++ b/package.json',
  '@@ -1,5 +1,5 @@',
  ' "dependencies": {',
  ...pkgLines,
  ' }',
  '}',
  extra,
].filter(Boolean).join('\n');

describe('classifyDepUpdateDiff + depUpdateVerdict', () => {
  it('classifies a package.json patch bump pair', () => {
    const d = DEP_DIFF(['-    "express": "^4.18.0",', '+    "express": "^4.18.2",']);
    const cls = classifyDepUpdateDiff(d);
    assert.deepEqual(cls.depFiles, ['package.json']);
    assert.equal(cls.updates[0].name, 'express');
    assert.equal(cls.updates[0].kind, 'patch');
    assert.equal(depUpdateVerdict(cls).eligible, true);
  });

  it('classifies minor and major bumps', () => {
    const minor = classifyDepUpdateDiff(DEP_DIFF(['-    "chalk": "^5.2.0"', '+    "chalk": "^5.3.0"']));
    assert.equal(minor.updates[0].kind, 'minor');
    const major = classifyDepUpdateDiff(DEP_DIFF(['-    "chalk": "^4.1.2"', '+    "chalk": "^5.3.0"']));
    assert.equal(major.updates[0].kind, 'major');
    const v = depUpdateVerdict(major);
    assert.equal(v.eligible, false);
    assert.equal(v.kind, 'major');
  });

  it('a new direct dep makes the diff ineligible (flag for human)', () => {
    const d = DEP_DIFF(['+    "leftpad": "^1.0.0",', '     "express": "^4.18.0",']);
    const cls = classifyDepUpdateDiff(d);
    assert.equal(cls.newDeps.length, 1);
    const v = depUpdateVerdict(cls);
    assert.equal(v.eligible, false);
    assert.equal(v.reason, 'new-direct-deps');
  });

  it('script/metadata lines inside package.json are not deps', () => {
    const d = DEP_DIFF(['+    "prepare": "npm run build",', '     "express": "^4.18.0",']);
    const cls = classifyDepUpdateDiff(d);
    assert.equal(cls.newDeps.length, 0);
    assert.equal(cls.updates.length, 0);
  });

  it('non-dep files disqualify the diff', () => {
    const d = DEP_DIFF(
      ['-    "express": "^4.18.0"', '+    "express": "^4.18.2"'],
      ['diff --git a/src/app.js b/src/app.js', '--- a/src/app.js', '+++ b/src/app.js',
        '@@ -1 +1 @@', '-const a = 1;', '+const a = 2;'].join('\n'));
    const cls = classifyDepUpdateDiff(d);
    assert.deepEqual(cls.nonDepFiles, ['src/app.js']);
    assert.equal(depUpdateVerdict(cls).reason, 'non-dep-files');
  });

  it('package-lock.json name/version pairs classify', () => {
    const d = [
      'diff --git a/package-lock.json b/package-lock.json',
      '--- a/package-lock.json',
      '+++ b/package-lock.json',
      '@@ -10,7 +10,7 @@',
      '     "node_modules/express": {',
      '-      "version": "4.18.0",',
      '+      "version": "4.18.2",',
      '       "resolved": "https://registry.npmjs.org/express/-/express-4.18.2.tgz",',
    ].join('\n');
    const cls = classifyDepUpdateDiff(d);
    assert.equal(cls.updates[0].name, 'express');
    assert.equal(cls.updates[0].kind, 'patch');
    assert.equal(depUpdateVerdict(cls).eligible, true);
  });

  it('unparseable specs classify unknown — never auto-eligible', () => {
    const d = DEP_DIFF(['-    "x": "^1.0.0"', '+    "x": "git+ssh://git@x.test/r.git"']);
    const cls = classifyDepUpdateDiff(d);
    assert.equal(cls.updates[0].kind, 'unknown');
    assert.equal(depUpdateVerdict(cls).eligible, false);
  });

  it('requirements.txt bumps classify pip-ecosystem', () => {
    const d = [
      'diff --git a/requirements.txt b/requirements.txt',
      '--- a/requirements.txt',
      '+++ b/requirements.txt',
      '@@ -1,2 +1,2 @@',
      '-requests==2.28.0',
      '+requests==2.28.2',
    ].join('\n');
    const cls = classifyDepUpdateDiff(d);
    assert.equal(cls.updates[0].ecosystem, 'pip');
    assert.equal(cls.updates[0].kind, 'patch');
  });

  it('empty and foreign diffs produce no-dep-files verdicts', () => {
    assert.equal(depUpdateVerdict(classifyDepUpdateDiff('')).reason, 'no-dep-files');
    const code = ['diff --git a/a.js b/a.js', '--- a/a.js', '+++ b/a.js', '@@ -1 +1 @@', '-x', '+y'].join('\n');
    const cls = classifyDepUpdateDiff(code);
    assert.deepEqual(cls.nonDepFiles, ['a.js']);
    // no dep files at all → 'no-dep-files' outranks 'non-dep-files'
    assert.equal(depUpdateVerdict(cls).reason, 'no-dep-files');
  });
});

// --- added-dep extraction + manifest detection -------------------------------
describe('extractAddedDeps / diffTouchesManifest', () => {
  it('detects manifest diffs and extracts added npm deps', () => {
    const d = DEP_DIFF(['+    "leftpad": "^1.0.0",', '     "express": "^4.18.0",']);
    assert.equal(diffTouchesManifest(d), true);
    const deps = extractAddedDeps(d);
    assert.deepEqual(deps, [{ ecosystem: 'npm', name: 'leftpad', version: '1.0.0' }]);
  });

  it('ignores non-spec values (scripts, metadata)', () => {
    const d = DEP_DIFF(['+    "test": "node --test",', '+    "version": "2.0.0",']);
    assert.deepEqual(extractAddedDeps(d), []);
  });

  it('no manifest → untouched', () => {
    const d = ['diff --git a/x.js b/x.js', '--- a/x.js', '+++ b/x.js', '@@ -1 +1 @@', '-a', '+b'].join('\n');
    assert.equal(diffTouchesManifest(d), false);
  });
});

// --- typosquat distance ------------------------------------------------------
describe('findNameConfusion', () => {
  it('flags 1-2 edit distance from popular names', () => {
    assert.ok(findNameConfusion('expres', 'npm').some(h => h.similarTo === 'express'));
    assert.ok(findNameConfusion('lodas', 'npm').some(h => h.similarTo === 'lodash'));
  });
  it('exact names and distant names are clean', () => {
    assert.deepEqual(findNameConfusion('express', 'npm'), []);
    assert.deepEqual(findNameConfusion('totally-different-pkg', 'npm'), []);
  });
});

// --- vetUpdate — supply-chain gate -------------------------------------------
describe('vetUpdate', () => {
  const dep = { ecosystem: 'npm', name: 'express', version: '4.18.2' };

  it('clean metadata passes with no findings', () => {
    assert.deepEqual(vetUpdate(dep, meta(), { now: NOW }), []);
  });

  it('release age gate: <7d-old target is a major finding', () => {
    const f = vetUpdate(dep, meta({ publishedAt: daysAgo(2) }), { now: NOW, minAgeDays: 7 });
    assert.ok(f.some(x => x.kind === 'fresh-release' && x.severity === 'major'));
    assert.ok(f[0].detail.includes('< 7d'));
  });

  it('≥7d passes the age gate', () => {
    assert.deepEqual(vetUpdate(dep, meta({ publishedAt: daysAgo(8) }), { now: NOW, minAgeDays: 7 }), []);
  });

  it('nonexistent package is a blocker', () => {
    const f = vetUpdate(dep, meta({ exists: false }), { now: NOW });
    assert.ok(f.some(x => x.kind === 'nonexistent' && x.severity === 'blocker'));
  });

  it('license outside the allowlist is a major finding', () => {
    const f = vetUpdate(dep, meta({ license: 'SSPL-1.0' }), { now: NOW });
    assert.ok(f.some(x => x.kind === 'license' && x.severity === 'major'));
  });

  it('OSV vulnerabilities are blockers', () => {
    const f = vetUpdate(dep, meta({ vulns: [{ id: 'GHSA-1', summary: 'rce' }] }), { now: NOW });
    assert.ok(f.some(x => x.kind === 'vulnerability' && x.severity === 'blocker'));
  });

  it('typosquat names draw a major finding', () => {
    const f = vetUpdate({ ecosystem: 'npm', name: 'expres', version: '1.0.0' }, meta(), { now: NOW });
    assert.ok(f.some(x => x.kind === 'typosquat' && x.severity === 'major'));
  });

  it('unavailable metadata is a minor finding (flagged, never trusted)', () => {
    const f = vetUpdate(dep, null, { now: NOW });
    assert.deepEqual(f.map(x => x.kind), ['metadata-unavailable']);
    assert.equal(f[0].severity, 'minor');
  });
});

// --- egress profiles ----------------------------------------------------------
describe('resolveEgress / makeFetcher', () => {
  it('npm-vet profile allows registry + OSV hosts only', () => {
    const e = resolveEgress('npm-vet');
    assert.ok(e.hosts.has('registry.npmjs.org'));
    assert.ok(e.hosts.has('api.osv.dev'));
    assert.ok(!e.hosts.has('evil.example'));
  });

  it('off profile denies everything; unknown profiles throw (fail-closed)', () => {
    assert.equal(resolveEgress('off').hosts.size, 0);
    assert.throws(() => resolveEgress('wide-open'), /unknown egress profile/);
  });

  it('extra hosts extend the allowlist', () => {
    const e = resolveEgress('npm-vet', ['npm.corp.example']);
    assert.ok(e.hosts.has('npm.corp.example'));
  });

  it('makeFetcher blocks hosts outside the profile', async () => {
    const fetchJson = makeFetcher(resolveEgress('off'));
    assert.strictEqual(await fetchJson('https://registry.npmjs.org/express'), null);
    const offMeta = await fetchJson('https://api.osv.dev/v1/querybatch');
    assert.strictEqual(offMeta, null);
  });
});

// --- buildPlan — candidates → updates/flagged/deferred -------------------------
const cand = (name, current, target, dev = false) =>
  ({ ecosystem: 'npm', name, current, wanted: target, latest: target, target, dev });

describe('buildPlan', () => {
  it('vets candidates into updates; majors and findings are flagged', async () => {
    const metas = {
      'patch-lib': meta(), 'minor-lib': meta(), 'major-lib': meta(),
      'fresh-lib': meta({ publishedAt: daysAgo(1) }),
    };
    const plan = await buildPlan(
      [cand('patch-lib', '1.0.0', '1.0.1'), cand('minor-lib', '1.0.0', '1.1.0'),
        cand('major-lib', '1.0.0', '2.0.0'), cand('fresh-lib', '1.0.0', '1.0.1')],
      { fetchMeta: async (d) => metas[d.name] || null, now: NOW },
    );
    assert.deepEqual(plan.updates.map(u => u.name), ['patch-lib', 'minor-lib']);
    assert.deepEqual(plan.flagged.map(u => u.name), ['major-lib', 'fresh-lib']);
    assert.equal(plan.flagged[0].kind, 'major');
    assert.ok(plan.flagged[1].findings.some(f => f.kind === 'fresh-release'));
    assert.equal(plan.riskTier, 'low');
  });

  it('auto_kinds=[patch] flags minor bumps too (risk threshold per repo)', async () => {
    const plan = await buildPlan(
      [cand('minor-lib', '1.0.0', '1.1.0')],
      { fetchMeta: async () => meta(), now: NOW, autoKinds: ['patch'] },
    );
    assert.equal(plan.updates.length, 0);
    assert.equal(plan.flagged[0].kind, 'minor');
    assert.equal(plan.riskTier, 'none');
  });

  it('unavailable metadata flags rather than lands (fail-closed)', async () => {
    const plan = await buildPlan([cand('safe-widget', '1.0.0', '1.0.1')],
      { fetchMeta: async () => null, now: NOW });
    assert.equal(plan.updates.length, 0);
    assert.ok(plan.flagged[0].findings.some(f => f.kind === 'metadata-unavailable'));
  });

  it('OSV vulns injected via osvMap flag the candidate', async () => {
    const osvMap = new Map([['npm:vuln-lib@1.0.1', [{ id: 'GHSA-x', summary: 'bad' }]]]);
    const plan = await buildPlan([cand('vuln-lib', '1.0.0', '1.0.1')],
      { fetchMeta: async () => meta(), osvMap, now: NOW });
    assert.equal(plan.updates.length, 0);
    assert.equal(plan.flagged[0].findings[0].kind, 'vulnerability');
  });

  it('maxUpdates bounds the batch; the rest defers (never dropped)', async () => {
    const cands = Array.from({ length: 5 }, (_, i) => cand(`wdep-${i}`, '1.0.0', '1.0.1'));
    const plan = await buildPlan(cands, { fetchMeta: async () => meta(), now: NOW, maxUpdates: 2 });
    assert.equal(plan.updates.length, 2);
    assert.equal(plan.deferred.length, 3);
  });

  it('the batch signature changes when versions drift', async () => {
    const a = await buildPlan([cand('safe-widget', '1.0.0', '1.0.1')], { fetchMeta: async () => meta(), now: NOW });
    const b = await buildPlan([cand('safe-widget', '1.0.0', '1.0.2')], { fetchMeta: async () => meta(), now: NOW });
    assert.notEqual(a.sig, b.sig);
    assert.equal(depSignature(a.updates), a.sig);
  });
});

// --- PR body ------------------------------------------------------------------
describe('buildPrBody', () => {
  it('carries the vetting table, flagged section and dedup marker', async () => {
    const metas = { 'patch-lib': meta(), 'major-lib': meta() };
    const plan = await buildPlan(
      [cand('patch-lib', '1.0.0', '1.0.1'), cand('major-lib', '1.0.0', '2.0.0')],
      { fetchMeta: async (d) => metas[d.name], now: NOW });
    const body = buildPrBody(plan, { minAgeDays: 7 });
    assert.match(body, /\| Package \| From \| To \| Kind \| Published \| Vet \|/);
    assert.match(body, /`patch-lib` \| 1\.0\.0 \| 1\.0\.1 \| patch/);
    assert.match(body, /Flagged for human review/);
    assert.match(body, /`major-lib` 1\.0\.0 → 2\.0\.0 — major bump/);
    assert.match(body, /release-age ≥ 7d/);
    assert.match(body, new RegExp(`ai-sdlc:dep-update sig=${plan.sig}`));
    assert.match(body, /untrusted input/);
  });

  it('escapes markdown metacharacters in registry-sourced strings', async () => {
    const plan = await buildPlan([cand('lib|bad', '1.0.0', '1.0.1')],
      { fetchMeta: async () => meta({ license: 'MIT</script>' }), now: NOW });
    const body = buildPrBody(plan);
    assert.ok(!body.includes('MIT</script>'));
    assert.ok(body.includes('lib\\|bad'));
  });
});

// --- vetPrDeps — vet added direct deps in a PR diff ----------------------------
describe('vetPrDeps', () => {
  it('no manifest diff → untouched', async () => {
    const d = ['diff --git a/x.js b/x.js', '--- a/x.js', '+++ b/x.js', '@@ -1 +1 @@', '-a', '+b'].join('\n');
    const r = await vetPrDeps(d, { fetchMeta: async () => meta(), now: NOW });
    assert.equal(r.touched, false);
    assert.equal(r.tier, null);
  });

  it('clean vetted new deps → medium tier', async () => {
    const d = DEP_DIFF(['+    "newlib": "^1.0.0",']);
    const r = await vetPrDeps(d, { fetchMeta: async () => meta(), now: NOW });
    assert.equal(r.checked.length, 1);
    assert.equal(r.violations.length, 0);
    assert.equal(r.tier, 'medium');
    assert.equal(r.flagHuman, false);
  });

  it('vulnerable/typosquat new deps → high + human flag', async () => {
    const d = DEP_DIFF(['+    "expres": "^1.0.0",']);
    const r = await vetPrDeps(d, { fetchMeta: async () => meta({ vulns: [{ id: 'GHSA-9' }] }), now: NOW });
    assert.ok(r.violations.length >= 1);
    assert.equal(r.tier, 'high');
    assert.equal(r.flagHuman, true);
  });
});

// --- CLI ---------------------------------------------------------------------
describe('cli modes (injected fixtures, no network)', () => {
  const tmpFile = (name, content) => {
    const p = path.join(fs.mkdtempSync(path.join(os.tmpdir(), 'du-')), name);
    fs.writeFileSync(p, content);
    return p;
  };

  it('classify mode emits the verdict for a fixture diff', async () => {
    const df = tmpFile('d.patch', DEP_DIFF(['-    "express": "^4.18.0"', '+    "express": "^4.18.2"']));
    let out = '';
    const r = await main(['classify', '--diff', df], { stdout: (s) => { out = s; } });
    assert.equal(r.verdict.eligible, true);
    assert.equal(JSON.parse(out).classification.updates[0].name, 'express');
  });

  it('plan mode --offline flags every candidate (egress off → metadata unavailable)', async () => {
    const of = tmpFile('outdated.json', JSON.stringify({
      'safe-widget': { current: '1.0.0', wanted: '1.0.1', latest: '1.0.1', type: 'dependencies' },
    }));
    const r = await main(['plan', '--outdated', of, '--offline', '--min-age-days', '7'], { stdout: () => {} });
    assert.equal(r.action, 'flagged-only');
    assert.equal(r.egress, 'off');
    assert.ok(r.flagged[0].findings.some(f => f.kind === 'metadata-unavailable'));
  });

  it('plan mode --egress-profile off behaves like --offline', async () => {
    const of = tmpFile('outdated.json', JSON.stringify({
      'safe-widget': { current: '1.0.0', wanted: '1.0.1', latest: '1.0.1', type: 'dependencies' },
    }));
    const r = await main(['plan', '--outdated', of, '--egress-profile', 'off'], { stdout: () => {} });
    assert.equal(r.action, 'flagged-only');
  });

  it('empty outdated scan → action none', async () => {
    const of = tmpFile('outdated.json', '{}');
    const r = await main(['plan', '--outdated', of, '--offline'], { stdout: () => {} });
    assert.equal(r.action, 'none');
    assert.equal(r.updates.length, 0);
  });

  it('--body-file writes the PR body', async () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'du-'));
    const of = path.join(dir, 'o.json');
    const bf = path.join(dir, 'body.md');
    fs.writeFileSync(of, '{}');
    await main(['plan', '--outdated', of, '--offline', '--body-file', bf], { stdout: () => {} });
    assert.ok(fs.readFileSync(bf, 'utf8').includes('ai-sdlc:dep-update'));
  });
});
