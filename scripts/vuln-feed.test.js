// scripts/vuln-feed.test.js — unit tests for the vulnerability feed worker
// (issue #99). All registry/OSV payloads and the GitHub client surface are
// injected fixtures — tests never touch the network (spec/injection-defense.md
// egress posture).
'use strict';

const { describe, it } = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const os = require('os');
const path = require('path');

const {
  vulnMarker,
  markersOf,
  parseVersion,
  cmpVersion,
  compileSpec,
  satisfies,
  maxSatisfying,
  parseLockfileDeps,
  parseManifestDeps,
  resolveManifestDeps,
  severityOf,
  fixedVersionsOf,
  groupFindings,
  dedupeFindings,
  buildTitle,
  buildIssueBody,
  runScan,
  parseArgs,
  main,
} = require('./vuln-feed');

// --- semver -----------------------------------------------------------------

describe('parseVersion / cmpVersion', () => {
  it('parses full, partial and prerelease versions', () => {
    assert.deepEqual(parseVersion('1.2.3'), { major: 1, minor: 2, patch: 3, pre: '' });
    assert.equal(parseVersion('v2.0.0').major, 2);
    assert.equal(parseVersion('1.2').patch, null);
    assert.equal(parseVersion('1.2.3-rc.1').pre, 'rc.1');
    assert.equal(parseVersion('1.2.3+build').pre, '');
    assert.equal(parseVersion('nope'), null);
  });

  it('orders releases and prereleases per semver precedence', () => {
    assert.equal(cmpVersion(parseVersion('1.2.3'), parseVersion('1.2.4')), -1);
    assert.equal(cmpVersion(parseVersion('2.0.0'), parseVersion('1.9.9')), 1);
    assert.equal(cmpVersion(parseVersion('1.2.3'), parseVersion('1.2.3')), 0);
    assert.equal(cmpVersion(parseVersion('1.2.3-rc.1'), parseVersion('1.2.3')), -1);
    assert.equal(cmpVersion(parseVersion('1.2.3-rc.2'), parseVersion('1.2.3-rc.10')), -1);
    assert.equal(cmpVersion(parseVersion('1.2.3-alpha'), parseVersion('1.2.3-alpha.1')), -1);
  });
});

describe('compileSpec / satisfies / maxSatisfying', () => {
  const vers = ['1.0.0', '1.2.0', '1.2.3', '1.3.0', '2.0.0', '2.1.0-beta.1', '3.0.0'];

  it('caret ranges obey npm semantics incl. 0.x', () => {
    assert.equal(satisfies('^1.2.0', '1.9.9'), true);
    assert.equal(satisfies('^1.2.0', '2.0.0'), false);
    assert.equal(satisfies('^0.2.3', '0.2.9'), true);
    assert.equal(satisfies('^0.2.3', '0.3.0'), false);
    assert.equal(satisfies('^0.0.3', '0.0.4'), false);
    assert.equal(satisfies('^1.2', '1.5.0'), true);
  });

  it('tilde ranges stay inside the minor', () => {
    assert.equal(satisfies('~1.2.0', '1.2.9'), true);
    assert.equal(satisfies('~1.2.0', '1.3.0'), false);
    assert.equal(satisfies('~1', '1.9.0'), true);
    assert.equal(satisfies('~1', '2.0.0'), false);
  });

  it('comparators, exacts, wildcards, hyphen and union ranges', () => {
    assert.equal(satisfies('>=1.2.0 <2.0.0', '1.9.0'), true);
    assert.equal(satisfies('1.2.3', '1.2.3'), true);
    assert.equal(satisfies('1.2.3', '1.2.4'), false);
    assert.equal(satisfies('1.x', '1.9.0'), true);
    assert.equal(satisfies('1.x', '2.0.0'), false);
    assert.equal(satisfies('1.2.3 - 2.0.0', '2.0.0'), true);
    assert.equal(satisfies('1.2.3 - 2.0.0', '2.0.1'), false);
    assert.equal(satisfies('1.2 - 2.3', '2.3.9'), true);
    assert.equal(satisfies('1.2 - 2.3', '2.4.0'), false);
    assert.equal(satisfies('^1.0.0 || ^2.0.0', '2.0.0'), true);
    assert.equal(satisfies('*', '3.0.0'), true);
  });

  it('prereleases are excluded unless the spec names their triple', () => {
    assert.equal(satisfies('^2.0.0', '2.1.0-beta.1'), false);
    assert.equal(satisfies('>=2.1.0-beta.1', '2.1.0-beta.1'), true);
  });

  it('unparseable/registry specs fail closed', () => {
    for (const s of ['npm:other@1.0.0', 'file:../x', 'git+ssh://x', 'https://x/t.tgz', 'workspace:*']) {
      assert.equal(compileSpec(s), null, s);
      assert.equal(maxSatisfying(s, vers), null, s);
    }
  });

  it('maxSatisfying picks the highest in-range published version', () => {
    assert.equal(maxSatisfying('^1.0.0', vers), '1.3.0');
    assert.equal(maxSatisfying('~1.2.0', vers), '1.2.3');
    assert.equal(maxSatisfying('*', vers), '3.0.0');
    assert.equal(maxSatisfying('^9.0.0', vers), null);
  });
});

// --- dependency surface -------------------------------------------------------

const LOCKFILE = JSON.stringify({
  lockfileVersion: 3,
  packages: {
    '': { name: 'x' },
    'node_modules/lodash': { version: '4.17.20' },
    'node_modules/app/node_modules/lodash': { version: '4.17.20' },
    'node_modules/minimist': { version: '1.2.5', dev: true },
  },
});

describe('parseLockfileDeps / parseManifestDeps', () => {
  it('extracts deduped name@version pairs incl. nested node_modules', () => {
    const deps = parseLockfileDeps(LOCKFILE);
    assert.deepEqual(deps.map((d) => `${d.name}@${d.version}`).sort(),
      ['lodash@4.17.20', 'minimist@1.2.5']);
    assert.equal(deps.find((d) => d.name === 'minimist').dev, true);
  });

  it('walks legacy v1 dependencies trees', () => {
    const j = JSON.stringify({ dependencies: { a: { version: '1.0.0', dependencies: { b: { version: '2.0.0' } } } } });
    assert.deepEqual(parseLockfileDeps(j).map((d) => d.name).sort(), ['a', 'b']);
  });

  it('returns [] on malformed input', () => {
    assert.deepEqual(parseLockfileDeps('{'), []);
  });

  it('parses manifest dep blocks with dev flags', () => {
    const deps = parseManifestDeps(JSON.stringify({
      dependencies: { lodash: '^4.17.0' },
      optionalDependencies: { fsevents: '~2.3.0' },
      devDependencies: { jest: '^29.0.0' },
    }));
    assert.equal(deps.find((d) => d.name === 'jest').dev, true);
    assert.equal(deps.find((d) => d.name === 'lodash').spec, '^4.17.0');
  });
});

describe('resolveManifestDeps', () => {
  it('resolves ranges via the injected registry and reports unresolved', async () => {
    const fetchVersions = async (name) => ({ lodash: ['4.17.20', '4.17.21'], jest: ['29.7.0'] }[name] || []);
    const { deps, unresolved } = await resolveManifestDeps([
      { name: 'lodash', spec: '^4.17.0', dev: false },
      { name: 'jest', spec: '^29.0.0', dev: true },
      { name: 'local', spec: 'file:../local', dev: false },
      { name: 'gone', spec: '^1.0.0', dev: false },
    ], fetchVersions);
    assert.deepEqual(deps.map((d) => `${d.name}@${d.version}`), ['lodash@4.17.21', 'jest@29.7.0']);
    assert.deepEqual(unresolved.map((u) => [u.name, u.reason]), [
      ['local', 'unparseable-spec'], ['gone', 'registry-unavailable']]);
  });
});

// --- findings, markers, dedup --------------------------------------------------

describe('groupFindings', () => {
  it('groups dep×vuln hits by advisory+package across versions', () => {
    const deps = [
      { name: 'lodash', version: '4.17.20' },
      { name: 'lodash', version: '4.17.15' },
      { name: 'minimist', version: '1.2.5', dev: true },
    ];
    const osvMap = new Map([
      ['npm:lodash@4.17.20', [{ id: 'GHSA-aaa', summary: 'pollution' }]],
      ['npm:lodash@4.17.15', [{ id: 'GHSA-aaa', summary: 'pollution' }, { id: 'GHSA-bbb', summary: 'other' }]],
      ['npm:minimist@1.2.5', []],
    ]);
    const g = groupFindings(deps, osvMap);
    assert.equal(g.length, 2);
    const a = g.find((x) => x.id === 'GHSA-aaa');
    assert.deepEqual(a.versions.sort(), ['4.17.15', '4.17.20']);
  });
});

describe('marker + dedup idiom', () => {
  it('round-trips the filing marker', () => {
    const body = `text\n${vulnMarker('GHSA-xxxx-yyyy', '@scope/pkg')}\n`;
    assert.deepEqual(markersOf(body), [{ id: 'GHSA-xxxx-yyyy', pkg: '@scope/pkg' }]);
  });

  it('open canonical dupes; closed canonical suppresses; new files', () => {
    const findings = [
      { id: 'GHSA-1', name: 'a' }, { id: 'GHSA-2', name: 'b' }, { id: 'GHSA-3', name: 'c' },
    ];
    const open = [{ number: 10, body: `x ${vulnMarker('GHSA-1', 'a')}` }];
    const closed = [{ number: 7, body: `x ${vulnMarker('GHSA-2', 'b')}` }];
    const { file, dupes, suppressed } = dedupeFindings(findings, open, closed);
    assert.deepEqual(file.map((f) => f.id), ['GHSA-3']);
    assert.deepEqual(dupes.map((f) => f.id), ['GHSA-1']);
    assert.deepEqual(suppressed[0].issue, 7);
  });
});

describe('issue body', () => {
  it('is DoR-complete, escaped and carries the signature marker', () => {
    const f = {
      id: 'GHSA-1234-abcd', name: 'lodash', versions: ['4.17.20'],
      summary: 'Proto<p> `evil`', dev: false, source: 'package-lock.json',
      detail: {
        id: 'GHSA-1234-abcd',
        database_specific: { severity: 'HIGH' },
        aliases: ['CVE-2021-23337'],
        affected: [{ package: { name: 'lodash' }, ranges: [{ events: [{ introduced: '0' }, { fixed: '4.17.21' }] }] }],
      },
    };
    const body = buildIssueBody(f, { repo: 'o/r' });
    assert.match(body, /## Problem/);
    assert.match(body, /## Acceptance criteria/);
    assert.match(body, /- \[ \]/);
    assert.ok(body.includes(vulnMarker('GHSA-1234-abcd', 'lodash')));
    assert.ok(!body.includes('<p>'), 'untrusted summary is escaped');
    assert.ok(body.includes('4.17.21'), 'fixed version surfaced');
    assert.match(buildTitle(f), /^security\(deps\): GHSA-1234-abcd affects lodash@4\.17\.20$/);
  });

  it('severity + fixed extraction are defensive', () => {
    assert.equal(severityOf(null), 'unknown');
    assert.equal(severityOf({ database_specific: { severity: 'MODERATE' } }), 'moderate');
    assert.deepEqual(fixedVersionsOf({ affected: [{ ranges: [{ events: [{ fixed: '2.0.0' }] }] }] }, 'x'), ['2.0.0']);
    assert.deepEqual(fixedVersionsOf(null, 'x'), []);
  });
});

// --- runScan orchestration -----------------------------------------------------

function fakeClient(open = [], closed = []) {
  const calls = { created: [], labels: [] };
  return {
    calls,
    listIssues: ({ state }) => (state === 'open' ? open : closed),
    ensureLabel: ({ label }) => calls.labels.push(label),
    createIssue: ({ title, body, labels }) => {
      calls.created.push({ title, body, labels });
      return { number: 100 + calls.created.length, url: `https://x/${calls.created.length}` };
    },
  };
}

function tmpDir(files) {
  const d = fs.mkdtempSync(path.join(os.tmpdir(), 'vulnfeed-'));
  for (const [n, c] of Object.entries(files)) fs.writeFileSync(path.join(d, n), c);
  return d;
}

const OSV = new Map([
  ['npm:lodash@4.17.20', [{ id: 'GHSA-1', summary: 'pollution' }]],
  ['npm:minimist@1.2.5', [{ id: 'GHSA-2', summary: 'other' }]],
]);

describe('runScan', () => {
  it('files a bounded DoR issue per new advisory (lockfile surface)', async () => {
    const dir = tmpDir({ 'package-lock.json': LOCKFILE, 'package.json': '{}' });
    const client = fakeClient();
    const r = await runScan({ repo: 'o/r', dir, osvMap: OSV, fetchJson: async () => null }, client);
    assert.equal(r.action, 'filed');
    assert.equal(r.filed.length, 2);
    assert.equal(r.source, 'package-lock.json');
    assert.equal(client.calls.created.length, 2);
    assert.ok(client.calls.created[0].labels.includes('security'));
    assert.ok(client.calls.created[0].body.includes('ai-sdlc:vuln-feed'));
  });

  it('dedupes against open issues and suppresses closed canonicals', async () => {
    const dir = tmpDir({ 'package-lock.json': LOCKFILE });
    const open = [{ number: 5, body: vulnMarker('GHSA-1', 'lodash') }];
    const closed = [{ number: 3, body: vulnMarker('GHSA-2', 'minimist') }];
    const client = fakeClient(open, closed);
    const r = await runScan({ repo: 'o/r', dir, osvMap: OSV, fetchJson: async () => null }, client);
    assert.equal(r.action, 'deduped');
    assert.equal(client.calls.created.length, 0);
    assert.deepEqual(r.duped, [{ id: 'GHSA-1', pkg: 'lodash' }]);
    assert.deepEqual(r.suppressed, [{ id: 'GHSA-2', pkg: 'minimist', issue: 3 }]);
  });

  it('caps filings at --max-findings and reports deferred', async () => {
    const dir = tmpDir({ 'package-lock.json': LOCKFILE });
    const client = fakeClient();
    const r = await runScan({ repo: 'o/r', dir, osvMap: OSV, maxFindings: 1, fetchJson: async () => null }, client);
    assert.equal(r.filed.length, 1);
    assert.equal(r.deferred, 1);
  });

  it('dry-run never mutates', async () => {
    const dir = tmpDir({ 'package-lock.json': LOCKFILE });
    const client = fakeClient();
    const r = await runScan({ repo: 'o/r', dir, osvMap: OSV, dryRun: true, fetchJson: async () => null }, client);
    assert.equal(r.action, 'filed');
    assert.equal(r.filed.length, 2);
    assert.equal(client.calls.created.length, 0);
  });

  it('manifest fallback resolves ranges via injected registry versions', async () => {
    const dir = tmpDir({ 'package.json': JSON.stringify({ dependencies: { lodash: '^4.17.0' } }) });
    const fetchJson = async (url) => url.includes('registry.npmjs.org/lodash')
      ? { versions: { '4.17.20': {}, '4.17.21': {} } } : null;
    const osvMap = new Map([['npm:lodash@4.17.21', [{ id: 'GHSA-9', summary: 'x' }]]]);
    const client = fakeClient();
    const r = await runScan({ repo: 'o/r', dir, osvMap, fetchJson }, client);
    assert.equal(r.source, 'package.json');
    assert.equal(r.filed.length, 1);
    assert.equal(r.filed[0].pkg, 'lodash');
  });

  it('skips repos with no dependency surface', async () => {
    const dir = tmpDir({ 'README.md': 'x' });
    const r = await runScan({ repo: 'o/r', dir, fetchJson: async () => null }, fakeClient());
    assert.equal(r.action, 'skipped');
  });

  it('clean when the surface carries no findings', async () => {
    const dir = tmpDir({ 'package-lock.json': LOCKFILE });
    const r = await runScan({ repo: 'o/r', dir, osvMap: new Map(), fetchJson: async () => null }, fakeClient());
    assert.equal(r.action, 'clean');
    assert.equal(r.scanned, 2);
  });
});

// --- CLI ---------------------------------------------------------------------

describe('CLI', () => {
  it('parses args and rejects unknown flags', () => {
    const o = parseArgs(['scan', '--repo', 'o/r', '--max-findings', '3', '--dry-run', '--labels', 'security,ai-sdlc']);
    assert.equal(o.maxFindings, 3);
    assert.equal(o.dryRun, true);
    assert.throws(() => parseArgs(['--bogus']));
  });

  it('scan runs end-to-end offline with injected client', async () => {
    const dir = tmpDir({ 'package-lock.json': LOCKFILE });
    const client = fakeClient();
    let out = '';
    const r = await main(['scan', '--repo', 'o/r', '--dir', dir, '--offline', '--dry-run', '--json'],
      { stdout: (s) => { out += s; }, client });
    assert.equal(r.action, 'clean');   // offline → empty OSV map
    assert.ok(out.includes('"action"'));
  });

  it('rejects a malformed --repo', async () => {
    await assert.rejects(() => main(['scan', '--repo', 'bad repo'], { stdout: () => {} }), /--repo/);
  });
});
