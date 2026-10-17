// scripts/repair-stall-guard.test.js — unit + mocked-API tests for the
// repair-stall guard (issue #190): per-signature repair counting,
// configurable threshold, needs-human escalation, reset on signature
// change.
'use strict';

const { describe, it } = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const os = require('os');
const path = require('path');

const {
  signatureFor,
  markerSignatureLine,
  evaluateStall,
  applyNeedsHuman,
  main,
} = require('./repair-stall-guard');

const marker = (attempt, sha, sig) => ({ attempt: String(attempt), sha, sig });
const SIG_A = signatureFor(['build', 'test']);
const SIG_B = signatureFor(['lint']);

// --- signature shape (Hermes failingCheckSignature port) ----------------------
describe('signatureFor', () => {
  it('is order-insensitive and dedupes names', () => {
    assert.equal(signatureFor(['test', 'build']), SIG_A);
    assert.equal(signatureFor(['build', 'test', 'build']), SIG_A);
  });

  it('is a bounded 16-hex marker-safe value', () => {
    assert.match(SIG_A, /^[0-9a-f]{16}$/);
  });

  it('returns empty for no failing checks (never escalates)', () => {
    assert.equal(signatureFor([]), '');
    assert.equal(signatureFor(undefined), '');
  });
});

// --- AC: counter increments per attempt on the same signature -----------------
describe('evaluateStall — counter', () => {
  it('counts only markers carrying the identical signature', () => {
    const markers = [marker(1, 'aaa', SIG_A), marker(2, 'bbb', SIG_A), marker(3, 'ccc', SIG_B)];
    const r = evaluateStall({ markers, checkNames: ['build', 'test'], maxStall: 5 });
    assert.equal(r.signature, SIG_A);
    assert.equal(r.count, 2);
    assert.equal(r.escalated, false);
  });

  it('increments with each recorded attempt', () => {
    const checkNames = ['build', 'test'];
    for (const [n, expected] of [[0, 0], [1, 1], [2, 2]]) {
      const markers = Array.from({ length: n }, (_, i) => marker(i + 1, `sha${i}`, SIG_A));
      assert.equal(evaluateStall({ markers, checkNames, maxStall: 9 }).count, expected);
    }
  });

  it('legacy markers without sig are unattributable and never count', () => {
    const markers = [marker(1, 'aaa', null), marker(2, 'bbb')];
    const r = evaluateStall({ markers, checkNames: ['build', 'test'], maxStall: 1 });
    assert.equal(r.count, 0);
    assert.equal(r.escalated, false);
  });
});

// --- AC: configurable threshold, default 3 ------------------------------------
describe('evaluateStall — threshold', () => {
  const markers3 = [marker(1, 'a', SIG_A), marker(2, 'b', SIG_A), marker(3, 'c', SIG_A)];

  it('escalates at the default threshold of 3', () => {
    const r = evaluateStall({ markers: markers3, checkNames: ['build', 'test'] });
    assert.equal(r.maxStall, 3);
    assert.equal(r.escalated, true);
    assert.equal(r.reason, `repair-stall:${SIG_A}`);
  });

  it('repairs while below the threshold', () => {
    const r = evaluateStall({ markers: markers3.slice(0, 2), checkNames: ['build', 'test'] });
    assert.equal(r.escalated, false);
    assert.equal(r.reason, 'below-threshold');
  });

  it('honors a custom threshold (1 = Hermes identical-signature semantics)', () => {
    const r = evaluateStall({ markers: markers3.slice(0, 1), checkNames: ['build', 'test'], maxStall: 1 });
    assert.equal(r.escalated, true);
  });

  it('maxStall <= 0 disables the guard (never escalates)', () => {
    for (const cap of [0, -1]) {
      const r = evaluateStall({ markers: markers3, checkNames: ['build', 'test'], maxStall: cap });
      assert.equal(r.enabled, false);
      assert.equal(r.escalated, false);
      assert.equal(r.reason, 'stall-guard-disabled');
    }
  });
});

// --- AC: reset on signature change --------------------------------------------
describe('evaluateStall — signature change resets the counter', () => {
  it('a different failing surface starts at zero regardless of past burns', () => {
    const markers = [marker(1, 'a', SIG_A), marker(2, 'b', SIG_A), marker(3, 'c', SIG_A)];
    const r = evaluateStall({ markers, checkNames: ['lint'], maxStall: 3 });
    assert.equal(r.signature, SIG_B);
    assert.equal(r.count, 0);
    assert.equal(r.escalated, false);
  });

  it('progress then regression: sig B burns while sig A is already over cap', () => {
    const markers = [
      marker(1, 'a', SIG_A), marker(2, 'b', SIG_A), marker(3, 'c', SIG_A),
      marker(4, 'd', SIG_B),
    ];
    assert.equal(evaluateStall({ markers, checkNames: ['lint'], maxStall: 3 }).escalated, false);
    assert.equal(evaluateStall({ markers, checkNames: ['build', 'test'], maxStall: 3 }).escalated, true);
  });
});

// --- marker format stays parseable by the workflow jq capture ------------------
describe('markerSignatureLine', () => {
  const JQ_RE = /<!-- ai-sdlc:ci-repair attempt=(?<attempt>[0-9]+) sha=(?<sha>[0-9a-f]+)( sig=(?<sig>[0-9a-f]+))? -->/;

  it('stamps attempt, sha and sig in the canonical marker shape', () => {
    const line = markerSignatureLine({ attempt: 2, sha: 'abc1234', signature: SIG_A });
    const m = JQ_RE.exec(line);
    assert.ok(m, `marker not parseable: ${line}`);
    assert.equal(m.groups.attempt, '2');
    assert.equal(m.groups.sha, 'abc1234');
    assert.equal(m.groups.sig, SIG_A);
  });

  it('omits sig when empty (legacy-compatible)', () => {
    const line = markerSignatureLine({ attempt: 1, sha: 'abc1234', signature: '' });
    assert.equal(JQ_RE.exec(line).groups.sig, undefined);
  });
});

// --- AC: label application -----------------------------------------------------
describe('applyNeedsHuman (mocked gh)', () => {
  function mockGh() {
    const calls = [];
    return { calls, exec: (argv) => { calls.push(argv); return ''; } };
  }

  it('creates/forces the label and adds it to the PR in bounded calls', () => {
    const { calls, exec } = mockGh();
    const r = applyNeedsHuman({ repo: 'o/r', pr: 7, exec });
    assert.equal(r.label, 'needs-human');
    assert.equal(r.applied, true);
    assert.equal(r.commented, false);
    const create = calls.find((c) => c.slice(0, 2).join(' ') === 'label create');
    const edit = calls.find((c) => c.slice(0, 2).join(' ') === 'pr edit');
    assert.ok(create.includes('needs-human') && create.includes('--force'));
    assert.ok(edit.includes('--add-label') && edit.includes('needs-human'));
    assert.ok(calls.length <= 3, `expected <=3 gh calls, got ${calls.length}`);
  });

  it('still applies the label when label create is not permitted', () => {
    const calls = [];
    const exec = (argv) => {
      calls.push(argv);
      if (argv[0] === 'label') throw new Error('forbidden');
      return '';
    };
    const r = applyNeedsHuman({ repo: 'o/r', pr: 7, exec });
    assert.equal(r.applied, true);
    assert.ok(calls.some((c) => c.slice(0, 2).join(' ') === 'pr edit'));
  });

  it('posts the escalation cause when a comment is provided', () => {
    const { calls, exec } = mockGh();
    const r = applyNeedsHuman({ repo: 'o/r', pr: 7, comment: 'stalled', exec });
    assert.equal(r.commented, true);
    assert.ok(calls.some((c) => c.slice(0, 2).join(' ') === 'pr comment'));
  });
});

// --- CLI end-to-end -------------------------------------------------------------
describe('cli', () => {
  it('evaluates from --markers + --failing files and escalates + applies', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'rsg-'));
    const mf = path.join(dir, 'markers.json');
    const ff = path.join(dir, 'failing.json');
    fs.writeFileSync(mf, JSON.stringify([marker(1, 'a', SIG_A), marker(2, 'b', SIG_A), marker(3, 'c', SIG_A)]));
    fs.writeFileSync(ff, JSON.stringify([{ name: 'build' }, { name: 'test' }]));
    const calls = [];
    let out = '';
    const r = main(
      ['--markers', mf, '--failing', ff, '--max-stall', '3', '--repo', 'o/r', '--pr', '9', '--apply', '--json'],
      { exec: (argv) => { calls.push(argv); return ''; }, stdout: (s) => { out = s; } },
    );
    assert.equal(r.escalated, true);
    assert.equal(r.escalation.label, 'needs-human');
    assert.ok(calls.some((c) => c.slice(0, 2).join(' ') === 'pr edit'));
    assert.ok(calls.some((c) => c.slice(0, 2).join(' ') === 'pr comment'));
    assert.equal(JSON.parse(out).signature, SIG_A);
  });

  it('does not touch the API below the threshold even with --apply', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'rsg-'));
    const mf = path.join(dir, 'markers.json');
    fs.writeFileSync(mf, JSON.stringify([marker(1, 'a', SIG_A)]));
    const calls = [];
    const r = main(
      ['--markers', mf, '--checks', 'build,test', '--repo', 'o/r', '--pr', '9', '--apply'],
      { exec: (argv) => { calls.push(argv); return ''; } },
    );
    assert.equal(r.escalated, false);
    assert.equal(r.escalation, undefined);
    assert.equal(calls.length, 0);
  });
});
