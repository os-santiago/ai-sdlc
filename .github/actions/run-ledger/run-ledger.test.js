/*
 * run-ledger.test.js — node --test coverage for the tamper-evident run
 * ledger (issue #196). Drives the CLI end-to-end (the same entry point the
 * composite action and auditors use) in a temp dir per test.
 */
'use strict';

const test = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const os = require('os');
const path = require('path');
const crypto = require('crypto');
const { execFileSync } = require('child_process');

const CLI = path.join(__dirname, 'run-ledger.js');

function tmpdir() {
  return fs.mkdtempSync(path.join(os.tmpdir(), 'run-ledger-test-'));
}

function run(args, opts = {}) {
  return execFileSync('node', [CLI, ...args], { encoding: 'utf8', env: { ...process.env, ...opts.env } });
}

function runFail(args, opts = {}) {
  try {
    run(args, opts);
  } catch (e) {
    return { code: e.status, stdout: e.stdout || '', stderr: e.stderr || '' };
  }
  return { code: 0, stdout: '', stderr: '' };
}

function readJsonl(file) {
  return fs.readFileSync(file, 'utf8').split('\n').filter(Boolean).map((l) => JSON.parse(l));
}

let keySeq = 0;
function keypair(dir) {
  const { privateKey, publicKey } = crypto.generateKeyPairSync('ed25519');
  const n = keySeq++;
  const priv = path.join(dir, `k${n}.pem`);
  const pub = path.join(dir, `k${n}.pub.pem`);
  fs.writeFileSync(priv, privateKey.export({ format: 'pem', type: 'pkcs8' }));
  fs.writeFileSync(pub, publicKey.export({ format: 'pem', type: 'spki' }));
  return { priv, pub };
}

test('append: genesis entry carries stage, ts, prev=genesis, data_sha256, hash', () => {
  const d = tmpdir();
  const ledger = path.join(d, 'ledger.jsonl');
  run(['append', '--ledger', ledger, '--stage', 'intake', '--repo', 'o/r', '--issue', '7',
    '--data', '{"dispatch":"true"}']);
  const [e] = readJsonl(ledger);
  assert.equal(e.seq, 1);
  assert.equal(e.stage, 'intake');
  assert.equal(e.prev, '0'.repeat(64));
  assert.ok(/^[0-9a-f]{64}$/.test(e.hash));
  assert.ok(/^[0-9a-f]{64}$/.test(e.data_sha256));
  assert.ok(!Number.isNaN(Date.parse(e.ts)));
});

test('append: second entry chains to the first via prev', () => {
  const d = tmpdir();
  const ledger = path.join(d, 'ledger.jsonl');
  run(['append', '--ledger', ledger, '--stage', 'a', '--data', '{}']);
  run(['append', '--ledger', ledger, '--stage', 'b', '--data', '{}']);
  const [e1, e2] = readJsonl(ledger);
  assert.equal(e2.prev, e1.hash);
  assert.equal(e2.seq, 2);
});

test('append: --prev seeds the first entry (cross-stage link)', () => {
  const d = tmpdir();
  const ledger = path.join(d, 'ledger.jsonl');
  const seed = 'a'.repeat(64);
  run(['append', '--ledger', ledger, '--stage', 'implement', '--prev', seed]);
  const [e] = readJsonl(ledger);
  assert.equal(e.prev, seed);
});

test('seal unsigned: ledger stays verifiable, signed=false', () => {
  const d = tmpdir();
  const ledger = path.join(d, 'ledger.jsonl');
  run(['append', '--ledger', ledger, '--stage', 'verify']);
  run(['seal', '--ledger', ledger]);
  const recs = readJsonl(ledger);
  const seal = recs[recs.length - 1];
  assert.equal(seal.type, 'seal');
  assert.equal(seal.signed, false);
  assert.equal(seal.entries, 1);
  const v = run(['verify', ledger]);
  assert.match(v, /OK/);
});

test('seal signed + verify --pubkey-file: signature verifies', () => {
  const d = tmpdir();
  const ledger = path.join(d, 'ledger.jsonl');
  const { priv, pub } = keypair(d);
  run(['append', '--ledger', ledger, '--stage', 'implement', '--data', '{"exit_code":"0"}']);
  run(['seal', '--ledger', ledger, '--key-file', priv]);
  const seal = readJsonl(ledger).pop();
  assert.equal(seal.signed, true);
  assert.equal(seal.alg, 'ed25519');
  assert.ok(seal.sig && seal.key_id);
  const v = run(['verify', ledger, '--pubkey-file', pub]);
  assert.match(v, /signed/);
  assert.match(v, /OK/);
});

test('tamper: modified stage data is detected by verify (exit 1)', () => {
  const d = tmpdir();
  const ledger = path.join(d, 'ledger.jsonl');
  const { priv, pub } = keypair(d);
  run(['append', '--ledger', ledger, '--stage', 'automerge', '--data', '{"action":"merged"}']);
  run(['seal', '--ledger', ledger, '--key-file', priv]);
  const recs = readJsonl(ledger);
  recs[0].data.action = 'escalated'; // tamper
  fs.writeFileSync(ledger, recs.map((r) => JSON.stringify(r)).join('\n') + '\n');
  const v = runFail(['verify', ledger, '--pubkey-file', pub]);
  assert.equal(v.code, 1);
  assert.match(v.stdout + v.stderr, /hash mismatch|data_sha256 mismatch|FAILED/);
});

test('tamper: removed entry breaks the seal entries count', () => {
  const d = tmpdir();
  const ledger = path.join(d, 'ledger.jsonl');
  const { priv, pub } = keypair(d);
  run(['append', '--ledger', ledger, '--stage', 'a']);
  run(['append', '--ledger', ledger, '--stage', 'b']);
  run(['seal', '--ledger', ledger, '--key-file', priv]);
  const recs = readJsonl(ledger);
  recs.splice(0, 1); // drop first entry
  fs.writeFileSync(ledger, recs.map((r) => JSON.stringify(r)).join('\n') + '\n');
  const v = runFail(['verify', ledger, '--pubkey-file', pub]);
  assert.equal(v.code, 1);
});

test('tamper: wrong public key fails signature verification', () => {
  const d = tmpdir();
  const ledger = path.join(d, 'ledger.jsonl');
  const { priv } = keypair(d);
  const other = keypair(d); // unrelated key
  run(['append', '--ledger', ledger, '--stage', 'ci-repair']);
  run(['seal', '--ledger', ledger, '--key-file', priv]);
  const v = runFail(['verify', ledger, '--pubkey-file', other.pub]);
  assert.equal(v.code, 1);
  assert.match(v.stdout + v.stderr, /INVALID|FAILED/);
});

test('verify: signed seal without a public key is a problem', () => {
  const d = tmpdir();
  const ledger = path.join(d, 'ledger.jsonl');
  const { priv } = keypair(d);
  run(['append', '--ledger', ledger, '--stage', 'intake']);
  run(['seal', '--ledger', ledger, '--key-file', priv]);
  const v = runFail(['verify', ledger]);
  assert.equal(v.code, 1);
  assert.match(v.stdout + v.stderr, /no public key/);
});

test('append after seal is refused (usage error, exit 2)', () => {
  const d = tmpdir();
  const ledger = path.join(d, 'ledger.jsonl');
  run(['append', '--ledger', ledger, '--stage', 'intake']);
  run(['seal', '--ledger', ledger]);
  const v = runFail(['append', '--ledger', ledger, '--stage', 'sneaky']);
  assert.equal(v.code, 2);
});

test('multi-file verify: seeded file chains from the parent head', () => {
  const d = tmpdir();
  const a = path.join(d, 'ledger-intake.jsonl');
  const b = path.join(d, 'ledger-implement.jsonl');
  const { priv, pub } = keypair(d);
  run(['append', '--ledger', a, '--stage', 'intake', '--issue', '5']);
  run(['seal', '--ledger', a, '--key-file', priv]);
  const head = run(['head', '--ledger', a]).trim();
  run(['append', '--ledger', b, '--stage', 'implement', '--issue', '5', '--prev', head]);
  run(['seal', '--ledger', b, '--key-file', priv]);
  const v = run(['verify', d, '--pubkey-file', pub]);
  assert.match(v, /chains from ledger-intake\.jsonl/);
  assert.match(v, /OK/);
});

test('entry cap bound: append refuses beyond AI_SDLC_LEDGER_MAX_ENTRIES', () => {
  const d = tmpdir();
  const ledger = path.join(d, 'ledger.jsonl');
  const env = { AI_SDLC_LEDGER_MAX_ENTRIES: '1' };
  run(['append', '--ledger', ledger, '--stage', 'a'], { env });
  const v = runFail(['append', '--ledger', ledger, '--stage', 'b'], { env });
  assert.equal(v.code, 2);
  assert.match(v.stderr, /cap/);
});

test('oversized stage data is rejected by the data cap', () => {
  const d = tmpdir();
  const ledger = path.join(d, 'ledger.jsonl');
  const env = { AI_SDLC_LEDGER_MAX_DATA_BYTES: '64' };
  const v = runFail(['append', '--ledger', ledger, '--stage', 'a', '--data', `{"big":"${'x'.repeat(200)}"}`], { env });
  assert.equal(v.code, 2);
});

test('data-files: file digests land under data.files and are covered by data_sha256', () => {
  const d = tmpdir();
  const ledger = path.join(d, 'ledger.jsonl');
  const evidence = path.join(d, 'scc-manifest.json');
  fs.writeFileSync(evidence, '{"tool_calls_total":12}');
  run(['append', '--ledger', ledger, '--stage', 'implement', '--data-file', evidence]);
  const [e] = readJsonl(ledger);
  assert.equal(e.data.files[0].path, 'scc-manifest.json');
  assert.equal(e.data.files[0].sha256, crypto.createHash('sha256').update('{"tool_calls_total":12}').digest('hex'));
});

test('keygen writes a usable ed25519 pair', () => {
  const d = tmpdir();
  const out = run(['keygen', '--out-dir', d]);
  assert.match(out, /key_id=/);
  const ledger = path.join(d, 'l.jsonl');
  run(['append', '--ledger', ledger, '--stage', 'verify']);
  run(['seal', '--ledger', ledger, '--key-file', path.join(d, 'ai-sdlc-ledger.key.pem')]);
  const v = run(['verify', ledger, '--pubkey-file', path.join(d, 'ai-sdlc-ledger.pub.pem')]);
  assert.match(v, /OK/);
});
