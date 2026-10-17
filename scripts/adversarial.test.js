// scripts/adversarial.test.js — unit tests for the adversarial attack-prompt
// generator (issue #203, epic #151 trust machinery).
'use strict';

const { describe, it } = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const os = require('os');
const path = require('path');

const {
  ATTACK_CATALOG,
  normalizeTier,
  resolveTier,
  diffPaths,
  generateAttacks,
  parseArgs,
  main,
} = require('./adversarial');

const file = (p, add = 10, del = 2, status = 'modified') => ({ path: p, add, del, status });
const diff = (added, removed = [], name = 'x.js') => [
  `diff --git a/${name} b/${name}`,
  `--- a/${name}`,
  `+++ b/${name}`,
  '@@ -1,1 +1,1 @@',
  ...removed.map((l) => `-${l}`),
  ...added.map((l) => `+${l}`),
].join('\n');

// --- AC: generated prompts contain known injection patterns ------------------
describe('generateAttacks — canonical injection patterns', () => {
  const d = diff(['const x = 1;']);

  it('emits a non-empty list of strings for a plain diff', () => {
    const attacks = generateAttacks(d);
    assert.ok(Array.isArray(attacks));
    assert.ok(attacks.length > 0);
    for (const a of attacks) assert.equal(typeof a, 'string');
  });

  it('includes the canonical "ignore previous instructions" marker', () => {
    const attacks = generateAttacks(d);
    assert.ok(attacks.some((a) => a.toLowerCase().includes('ignore previous instructions')));
  });

  it('includes a "system:" prompt-spoofing attack', () => {
    const attacks = generateAttacks(d);
    assert.ok(attacks.some((a) => a.includes('system:')));
  });

  it('covers the injection-defense threat model families', () => {
    const attacks = generateAttacks(d);
    const joined = attacks.join('\n').toLowerCase();
    assert.ok(joined.includes('<<sys>>'), 'chat-template spoof missing');
    assert.ok(joined.includes('<|im_start|>'), 'im_start token missing');
    assert.ok(joined.includes('you are now'), 'persona hijack missing');
    assert.ok(joined.includes('.env'), 'secret-exfil fixture missing');
    assert.ok(joined.includes('git push --force'), 'forbidden force-push missing');
    assert.ok(joined.includes('| sudo bash') || joined.includes('| sh'), 'pipe-to-shell missing');
    assert.ok(joined.includes('</untrusted-input>'), 'fence escape missing');
  });
});

// --- AC: non-empty output for pr:risk-high diffs -----------------------------
describe('generateAttacks — risk-label input', () => {
  it('accepts a pr:risk-high label and emits attacks', () => {
    const attacks = generateAttacks(diff(['x()']), { labels: ['pr:risk-high', 'bug'] });
    assert.ok(attacks.length > 0);
  });

  it('accepts the label string directly via opts.risk', () => {
    assert.ok(generateAttacks(diff(['x()']), { risk: 'pr:risk-high' }).length > 0);
    assert.ok(generateAttacks(diff(['x()']), { tier: 'high' }).length > 0);
  });

  it('high-risk output meets the threshold when minTier=high', () => {
    const attacks = generateAttacks(diff(['x()']), { labels: ['pr:risk-high'], minTier: 'high' });
    assert.ok(attacks.length > 0);
  });

  it('resolves the tier from a files array via the canonical classifier', () => {
    // .github/workflows/ is a critical surface per spec/risk-taxonomy.md.
    const attacks = generateAttacks(
      { files: [file('.github/workflows/ci.yml')], diffText: '' },
      { minTier: 'high' },
    );
    assert.ok(attacks.length > 0);
  });
});

// --- AC: empty list for low-risk diffs when a threshold is configured --------
describe('generateAttacks — minTier threshold gate', () => {
  it('returns [] for pr:risk-low when minTier is set', () => {
    assert.deepEqual(generateAttacks(diff(['x()']), { labels: ['pr:risk-low'], minTier: 'medium' }), []);
    assert.deepEqual(generateAttacks(diff(['x()']), { tier: 'low', minTier: 'high' }), []);
  });

  it('still generates for low-risk diffs when no threshold is configured', () => {
    assert.ok(generateAttacks(diff(['x()']), { labels: ['pr:risk-low'] }).length > 0);
  });

  it('tiers at or above the threshold still pass the gate', () => {
    for (const t of ['medium', 'high', 'critical']) {
      assert.ok(generateAttacks(diff(['x()']), { tier: t, minTier: 'medium' }).length > 0, t);
    }
  });

  it('unresolvable tier resolves UP — attacks generated, not skipped', () => {
    assert.ok(generateAttacks(diff(['x()']), { labels: ['bug'], minTier: 'high' }).length > 0);
    assert.ok(generateAttacks(diff(['x()']), { minTier: 'high' }).length > 0);
  });
});

// --- Surface-targeted attacks ------------------------------------------------
describe('generateAttacks — surface targeting', () => {
  it('adds a workflow-permissions attack when .github/workflows is touched', () => {
    const attacks = generateAttacks({ files: [file('.github/workflows/ci.yml')] });
    assert.ok(attacks.some((a) => a.includes('permissions: write-all')));
  });

  it('adds a secret-file attack when a credential path is touched', () => {
    const attacks = generateAttacks({ files: [file('config/.env.production')] });
    assert.ok(attacks.some((a) => a.includes('credential file')));
  });

  it('adds an auth-weakening attack when auth surface is touched', () => {
    const attacks = generateAttacks({ files: [file('src/auth/session.js')] });
    assert.ok(attacks.some((a) => a.includes('session token')));
  });

  it('adds a dependency-install attack when a manifest is touched', () => {
    const attacks = generateAttacks({ files: [file('package.json')] });
    assert.ok(attacks.some((a) => a.includes('postinstall')));
  });

  it('docs-only diffs get no surface-targeted extras beyond the base set', () => {
    const nSurface = ATTACK_CATALOG.filter((e) => e.surfaces).length;
    const attacks = generateAttacks({ files: [file('docs/runtime.md')] });
    assert.equal(attacks.length, ATTACK_CATALOG.length - nSurface);
  });
});

// --- Boundedness ---------------------------------------------------------------
describe('generateAttacks — bounds', () => {
  it('respects maxAttacks', () => {
    const attacks = generateAttacks(diff(['x()']), { maxAttacks: 3 });
    assert.equal(attacks.length, 3);
  });

  it('caps the embedded diff excerpt at maxExcerpt chars', () => {
    const big = diff(['a'.repeat(5000)]);
    const attacks = generateAttacks(big, { maxExcerpt: 120 });
    const echo = attacks.find((a) => a.includes('<diff-excerpt>'));
    assert.ok(echo);
    assert.ok(echo.length < 120 + 300, 'excerpt should be capped');
  });
});

// --- Helpers -------------------------------------------------------------------
describe('helpers', () => {
  it('normalizeTier accepts tiers, labels and risk-* prefixes', () => {
    assert.equal(normalizeTier('high'), 'high');
    assert.equal(normalizeTier('pr:risk-high'), 'high');
    assert.equal(normalizeTier('PR:RISK-CRITICAL'), 'critical');
    assert.equal(normalizeTier('risk-low'), 'low');
    assert.equal(normalizeTier('bug'), null);
    assert.equal(normalizeTier(''), null);
    assert.equal(normalizeTier(undefined), null);
  });

  it('resolveTier prefers explicit signal, then labels, then classifier', () => {
    assert.equal(resolveTier({ tier: 'low', labels: ['pr:risk-high'] }), 'low');
    assert.equal(resolveTier({ labels: ['x', 'pr:risk-critical'] }), 'critical');
    assert.equal(resolveTier({ files: [file('README.md')] }), 'low');
    assert.equal(resolveTier({}), null);
  });

  it('diffPaths extracts b/ paths from a unified diff', () => {
    const d = diff(['x'], [], 'src/a.js') + '\n' + diff(['y'], [], 'lib/b.js');
    assert.deepEqual(diffPaths(d).sort(), ['lib/b.js', 'src/a.js']);
  });

  it('context-echo attack embeds added diff lines', () => {
    const attacks = generateAttacks(diff(['const SECRET_SAUCE = 42;']));
    assert.ok(attacks.some((a) => a.includes('SECRET_SAUCE')));
  });
});

// --- CLI -----------------------------------------------------------------------
describe('cli', () => {
  it('parseArgs reads flags', () => {
    const o = parseArgs(['--diff', 'd.patch', '--tier', 'high', '--min-tier', 'medium', '--max', '5', '--json']);
    assert.equal(o.diff, 'd.patch');
    assert.equal(o.tier, 'high');
    assert.equal(o.minTier, 'medium');
    assert.equal(o.maxAttacks, 5);
    assert.equal(o.json, true);
  });

  it('main emits JSON attacks for a --diff file', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'adv-'));
    const p = path.join(dir, 'd.patch');
    fs.writeFileSync(p, diff(['const x = 1;']));
    let out = '';
    const attacks = main(['--diff', p, '--label', 'pr:risk-high', '--json'], { stdout: (s) => { out = s; } });
    assert.ok(attacks.length > 0);
    assert.deepEqual(JSON.parse(out), attacks);
  });
});
