// scripts/alert-router.test.js — unit + mocked-exec tests for the
// failure-class → notification-route resolver (issue #175).
'use strict';

const { describe, it } = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const os = require('os');
const path = require('path');

const {
  FAILURE_CLASSES,
  classifyFailure,
  normalizeSignature,
  dedupKey,
  resolveRoute,
  markerFor,
  findPriorAlert,
  renderNotification,
  applyAlert,
  decide,
  main,
} = require('./alert-router');

const tmpdir = () => fs.mkdtempSync(path.join(os.tmpdir(), 'alert-router-'));
const write = (dir, name, data) => {
  const p = path.join(dir, name);
  fs.writeFileSync(p, typeof data === 'string' ? data : JSON.stringify(data));
  return p;
};

// --- AC: every canonical failure class maps to a declared route ---------------
describe('classifyFailure — canonical classes', () => {
  const cases = [
    ['conflict', 'conflict'],
    ['checks-stalled', 'pending-timeout'],
    ['repair-stalled', 'repair-stalled:ci,build'],
    ['repairs-exhausted', 'repairs-exhausted:ci'],
    ['repairs-exhausted', 'repair budget exhausted (3/3 attempts) and checks are still red'],
    ['repair-failed', 'repair attempt 2/3 produced no pushable fix (outcome: engine-exit-20)'],
    ['repair-failed', 'engine-exit-22'],
    ['repair-failed', 'push-rejected'],
    ['review-blocked', 'external-review-blocking'],
    ['review-fix-failed', 'review-fix-stalled'],
    ['review-fix-failed', 'review-fix-not-dispatched'],
    ['supply-chain', 'dep violations on manifest'],
    ['supply-chain', 'supply-chain violation'],
    ['contract-error', 'base-contract-error'],
    ['contract-error', 'scc:config-error'],
    ['verify-failure', 'post-merge verify commands failed on abc123'],
    ['security', 'secret-scan flagged gitleaks finding'],
    ['provenance', 'provenance:fork-head'],
    ['provenance', 'provenance:untrusted-author'],
    ['checks-stalled', 'no-checks'],
    ['policy-hold', 'risk-critical-unaccepted'],
    ['unknown', 'some totally novel failure mode'],
    ['unknown', ''],
  ];
  for (const [expected, input] of cases) {
    it(`classifies ${JSON.stringify(input).slice(0, 60)} → ${expected}`, () => {
      assert.equal(classifyFailure({ reason: input }), expected);
    });
  }

  it('explicit --class wins over pattern matching', () => {
    assert.equal(classifyFailure({ reason: 'conflict', explicitClass: 'security' }), 'security');
  });

  it('explicit class may be a contract-declared id outside the canonical list', () => {
    assert.equal(classifyFailure({ explicitClass: 'custom-flake' }), 'custom-flake');
  });
});

// --- AC: route resolution precedence -----------------------------------------
describe('resolveRoute', () => {
  const escalation = {
    label: 'needs-human',
    contacts: ['@Axel-DaMage'],
    notify_channels: ['ops-channel'],
    routes: {
      'contract-error': { mentions: ['@spec-owner'], labels: ['priority:P0'], channel: 'issue-comment' },
      default: { mentions: ['@team-lead'], labels: ['ai-sdlc'], channel: 'issue-comment' },
    },
  };

  it('a declared class routes to its own entry', () => {
    const r = resolveRoute('contract-error', escalation);
    assert.equal(r.matched, 'contract-error');
    assert.equal(r.source, 'routes.contract-error');
    assert.deepEqual(r.route.mentions, ['@spec-owner']);
    assert.deepEqual(r.route.labels, ['priority:P0']);
  });

  it('an undeclared class falls back to routes.default — never silent', () => {
    const r = resolveRoute('unknown', escalation);
    assert.equal(r.matched, 'default');
    assert.equal(r.source, 'routes.default');
    assert.deepEqual(r.route.mentions, ['@team-lead']);
  });

  it('every canonical class resolves to a non-empty route', () => {
    for (const cls of FAILURE_CLASSES) {
      const r = resolveRoute(cls, escalation);
      assert.ok(r.route.channel, `${cls} has no channel`);
      assert.ok(r.route.mentions.length, `${cls} route has no mentions`);
    }
  });

  it('no routes map → synthesized default from contacts/notify_channels', () => {
    const r = resolveRoute('conflict', { contacts: ['maintainer'], notify_channels: ['alerts'] });
    assert.equal(r.matched, 'contacts');
    assert.equal(r.source, 'escalation.contacts');
    assert.deepEqual(r.route.mentions, ['@maintainer']);
    assert.equal(r.route.channel, 'alerts');
  });

  it('empty escalation still emits a route — the issue-comment floor (never silent)', () => {
    const r = resolveRoute('conflict', {});
    assert.equal(r.source, 'escalation.fallback');
    assert.equal(r.route.channel, 'issue-comment');
    assert.deepEqual(r.route.mentions, []);
  });

  it('mention normalization prefixes @ on bare handles and team slugs', () => {
    const r = resolveRoute('x', { routes: { x: { mentions: ['alice', 'org/team'] } } });
    assert.deepEqual(r.route.mentions, ['@alice', '@org/team']);
  });
});

// --- Dedup --------------------------------------------------------------------
describe('dedup', () => {
  const key = 'abcd1234abcd1234';
  const marker = markerFor('conflict', key);

  it('same key in a prior comment deduplicates the alert', () => {
    const prior = findPriorAlert([{ body: `x\n${marker}\n`, created_at: '2026-01-01T00:00:00Z' }], { key });
    assert.ok(prior);
    assert.equal(prior.class, 'conflict');
  });

  it('a different key does not dedup', () => {
    const prior = findPriorAlert([{ body: markerFor('conflict', 'ffff0000ffff0000'), created_at: '2026-01-01T00:00:00Z' }], { key });
    assert.equal(prior, null);
  });

  it('plain string comments are scanned too', () => {
    const prior = findPriorAlert([`noise ${marker}`], { key });
    assert.ok(prior);
  });

  it('dedup window: markers older than the window do not suppress re-notification', () => {
    const now = Date.parse('2026-02-01T00:00:00Z');
    const stale = findPriorAlert(
      [{ body: marker, created_at: '2026-01-01T00:00:00Z' }],
      { key, dedupWindowMinutes: 60, now },
    );
    assert.equal(stale, null);
    const fresh = findPriorAlert(
      [{ body: marker, created_at: '2026-01-31T23:30:00Z' }],
      { key, dedupWindowMinutes: 60, now },
    );
    assert.ok(fresh);
  });

  it('dedupKey is stable across SHAs/numbers in the signature', () => {
    const a = dedupKey({ repo: 'o/r', target: 7, cls: 'repairs-exhausted', signature: 'ci failed at sha 8f9aa2b on line 42' });
    const b = dedupKey({ repo: 'o/r', target: 7, cls: 'repairs-exhausted', signature: 'ci failed at sha 11bb33c on line 99' });
    assert.equal(a, b);
    const c = dedupKey({ repo: 'o/r', target: 8, cls: 'repairs-exhausted', signature: 'ci failed at sha 8f9aa2b on line 42' });
    assert.notEqual(a, c); // different target = different alert
  });

  it('normalizeSignature collapses hex + digits', () => {
    assert.equal(normalizeSignature('err at deadbeef line 123'), 'err at H line N');
  });
});

// --- Notification render + audit ----------------------------------------------
describe('renderNotification', () => {
  const resolved = resolveRoute('conflict', {
    routes: { conflict: { mentions: ['@oncall'], labels: ['p1'] } },
  });

  it('carries mentions, class, reason and the audit marker', () => {
    const body = renderNotification({
      cls: 'conflict', route: resolved, title: 'AI-SDLC merge gate escalated',
      reason: 'conflict', signature: 'sig1',
    }) + `\n${markerFor('conflict', 'k')}`;
    assert.match(body, /cc @oncall/);
    assert.match(body, /failure class: `conflict`/);
    assert.match(body, /<!-- ai-sdlc:alert-route class=conflict key=k -->/);
  });

  it('deduped notifications suppress mentions and say so', () => {
    const body = renderNotification({ cls: 'conflict', route: resolved, deduped: true });
    assert.doesNotMatch(body, /cc @oncall/);
    assert.match(body, /deduplicated/);
  });
});

// --- decide() end-to-end + mocked gh -------------------------------------------
describe('decide + applyAlert', () => {
  it('full path: reason → class → route → comment + labels via mocked gh', () => {
    const dir = tmpdir();
    const contract = write(dir, 'contract.json', {
      escalation: {
        label: 'needs-human',
        contacts: ['@fallback'],
        routes: { conflict: { mentions: ['@merge-owner'], labels: ['priority:P1'] } },
      },
    });
    const calls = [];
    const exec = (argv) => { calls.push(argv); return ''; };
    const d = decide({
      repo: 'o/r', target: 42, reason: 'conflict',
      contract, apply: true,
    }, { exec });
    assert.equal(d.class, 'conflict');
    assert.equal(d.source, 'routes.conflict');
    assert.equal(d.deduped, false);
    assert.ok(d.applied.commented);
    assert.deepEqual(d.applied.labels, ['priority:P1']);
    assert.ok(calls.some((c) => c[0] === 'issue' && c[1] === 'comment'));
  });

  it('deduped alert applies labels but posts no comment', () => {
    const dir = tmpdir();
    const escalation = write(dir, 'esc.json', {
      routes: { default: { mentions: ['@a'], labels: ['p2'] } },
    });
    // Pre-seed the thread with a marker for this exact decision.
    const probe = decide({ repo: 'o/r', target: 9, reason: 'conflict', escalation });
    const comments = write(dir, 'comments.json', [
      { body: `prior\n${markerFor(probe.class, probe.key)}`, created_at: '2026-01-01T00:00:00Z' },
    ]);
    const calls = [];
    const d = decide({
      repo: 'o/r', target: 9, reason: 'conflict',
      escalation, comments, apply: true,
    }, { exec: (argv) => { calls.push(argv); return ''; } });
    assert.equal(d.deduped, true);
    assert.equal(d.applied.commented, false);
    assert.deepEqual(d.applied.labels, ['p2']);
    assert.ok(!calls.some((c) => c[0] === 'issue' && c[1] === 'comment'));
  });

  it('unknown class → default route, mentions still delivered (never silent)', () => {
    const dir = tmpdir();
    const escalation = write(dir, 'esc.json', {
      contacts: ['@oncall'],
      routes: { default: { mentions: ['@oncall'], labels: [] } },
    });
    const d = decide({ repo: 'o/r', target: 1, reason: 'a novel failure nobody declared', escalation });
    assert.equal(d.class, 'unknown');
    assert.equal(d.source, 'routes.default');
    assert.deepEqual(d.route.mentions, ['@oncall']);
  });

  it('contract.json shape (full resolved contract) reads .escalation', () => {
    const dir = tmpdir();
    const contract = write(dir, 'contract.json', {
      spec: '1.0.0',
      escalation: { contacts: ['@x'], routes: { 'verify-failure': { mentions: ['@ops'] } } },
    });
    const d = decide({ repo: 'o/r', target: 2, reason: 'post-merge verify failed', contract });
    assert.equal(d.class, 'verify-failure');
    assert.equal(d.source, 'routes.verify-failure');
    assert.deepEqual(d.route.mentions, ['@ops']);
  });

  it('audit file gets one JSON line per decision', () => {
    const dir = tmpdir();
    const auditFile = path.join(dir, 'audit.jsonl');
    decide({ repo: 'o/r', target: 3, reason: 'conflict', auditFile });
    decide({ repo: 'o/r', target: 3, reason: 'pending-timeout', auditFile });
    const lines = fs.readFileSync(auditFile, 'utf8').trim().split('\n').map((l) => JSON.parse(l));
    assert.equal(lines.length, 2);
    assert.equal(lines[0].class, 'conflict');
    assert.equal(lines[1].class, 'checks-stalled');
    for (const l of lines) {
      assert.ok(l.at && l.key && l.source, 'audit record is complete');
    }
  });

  it('applyAlert survives label failures and still comments', () => {
    const calls = [];
    const exec = (argv) => {
      calls.push(argv);
      if (argv[0] === 'label') throw new Error('no admin');
      if (argv[0] === 'issue' && argv[1] === 'edit') throw new Error('unknown label');
      return '';
    };
    const r = applyAlert({ repo: 'o/r', target: 5, labels: ['p0'], body: 'b', exec });
    assert.equal(r.commented, true);
    assert.deepEqual(r.labels, []); // label failed; comment still landed
  });
});

// --- main() CLI surface --------------------------------------------------------
describe('main', () => {
  it('--json emits the decision; --github-output writes alert_* keys', () => {
    const dir = tmpdir();
    process.env.GITHUB_OUTPUT = path.join(dir, 'out.txt');
    try {
      const out = [];
      const d = main([
        '--repo', 'o/r', '--pr', '11', '--reason', 'repairs-exhausted:ci',
        '--json', '--github-output',
      ], { stdout: (s) => out.push(s) });
      assert.equal(d.class, 'repairs-exhausted');
      const emitted = JSON.parse(out[0]);
      assert.equal(emitted.key, d.key);
      const gho = fs.readFileSync(process.env.GITHUB_OUTPUT, 'utf8');
      assert.match(gho, /alert_class=repairs-exhausted/);
      assert.match(gho, /alert_marker=<!-- ai-sdlc:alert-route/);
    } finally {
      delete process.env.GITHUB_OUTPUT;
    }
  });
});
