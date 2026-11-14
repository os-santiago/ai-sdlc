/*
 * security-log.test.js — node --test coverage for the tamper-evident
 * security event log (issue #106). Drives the CLI end-to-end in a temp dir
 * per test, including the tamper-detection contract: editing, deleting, or
 * reordering a historical event must fail `verify`, and a signed seal must
 * catch a rewritten chain tip.
 */
'use strict';

const test = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const os = require('os');
const path = require('path');
const crypto = require('crypto');
const { execFileSync } = require('child_process');

const CLI = path.join(__dirname, 'security-log.js');
const GENESIS = '0'.repeat(64);

function tmpdir() {
  return fs.mkdtempSync(path.join(os.tmpdir(), 'security-log-test-'));
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

function lines(file) {
  return fs.readFileSync(file, 'utf8').split('\n').filter((l) => l.trim());
}

function keypair(dir) {
  const { privateKey, publicKey } = crypto.generateKeyPairSync('ed25519');
  const priv = path.join(dir, 'sk.pem');
  const pub = path.join(dir, 'sk.pub.pem');
  fs.writeFileSync(priv, privateKey.export({ format: 'pem', type: 'pkcs8' }));
  fs.writeFileSync(pub, publicKey.export({ format: 'pem', type: 'spki' }));
  return { priv, pub };
}

function seedLog(dir, count = 3) {
  const log = path.join(dir, 'SECURITY_LOG.md');
  const types = ['sanitization-block', 'kill-switch-engage', 'escalation', 'policy-override', 'kill-switch-release'];
  for (let i = 0; i < count; i++) {
    run(['log', '--log', log, '--type', types[i % types.length], '--actor', 'tester',
      '--description', `event ${i + 1}`]);
  }
  return log;
}

test('log: genesis event has GENESIS prev and contract pipe format', () => {
  const d = tmpdir();
  const log = path.join(d, 'SECURITY_LOG.md');
  run(['log', '--log', log, '--type', 'sanitization-block', '--actor', 'bot', '--description', 'blocked prompt injection']);
  const [line] = lines(log);
  const parts = line.split(' | ');
  assert.equal(parts.length, 5);
  assert.equal(parts[0], GENESIS);
  assert.ok(!Number.isNaN(Date.parse(parts[1])));
  assert.equal(parts[2], 'bot');
  assert.equal(parts[3], 'sanitization-block');
  assert.equal(parts[4], 'blocked prompt injection');
});

test('log: each event stores sha256 of the previous raw line', () => {
  const d = tmpdir();
  const log = seedLog(d, 3);
  const ls = lines(log);
  assert.equal(ls.length, 3);
  const sha = (s) => crypto.createHash('sha256').update(s).digest('hex');
  assert.equal(ls[1].split(' | ')[0], sha(ls[0]));
  assert.equal(ls[2].split(' | ')[0], sha(ls[1]));
});

test('verify: intact chain exits 0', () => {
  const d = tmpdir();
  const log = seedLog(d, 4);
  const out = run(['verify', '--log', log]);
  assert.match(out, /OK — hash chain/);
});

test('tamper: editing a historical event fails verify', () => {
  const d = tmpdir();
  const log = seedLog(d, 3);
  const ls = lines(log);
  ls[1] = ls[1].replace('event 2', 'event two — rewritten');
  fs.writeFileSync(log, ls.join('\n') + '\n');
  const r = runFail(['verify', '--log', log]);
  assert.equal(r.code, 1);
  assert.match(r.stdout + r.stderr, /TAMPER/);
});

test('tamper: deleting an event fails verify', () => {
  const d = tmpdir();
  const log = seedLog(d, 3);
  const ls = lines(log);
  ls.splice(1, 1);
  fs.writeFileSync(log, ls.join('\n') + '\n');
  const r = runFail(['verify', '--log', log]);
  assert.equal(r.code, 1);
});

test('tamper: reordering events fails verify', () => {
  const d = tmpdir();
  const log = seedLog(d, 3);
  const ls = lines(log);
  [ls[1], ls[2]] = [ls[2], ls[1]];
  fs.writeFileSync(log, ls.join('\n') + '\n');
  const r = runFail(['verify', '--log', log]);
  assert.equal(r.code, 1);
});

test('tamper: forging a genesis prev fails verify', () => {
  const d = tmpdir();
  const log = seedLog(d, 2);
  const ls = lines(log);
  ls[0] = ls[0].replace(GENESIS, 'f'.repeat(64));
  fs.writeFileSync(log, ls.join('\n') + '\n');
  const r = runFail(['verify', '--log', log]);
  assert.equal(r.code, 1);
});

test('seal: unsigned seal attests head + count; verify warns', () => {
  const d = tmpdir();
  const log = seedLog(d, 2);
  run(['seal', '--log', log]);
  const r = run(['verify', '--log', log]);
  assert.match(r, /unsigned/);
  const sealLine = lines(log).find((l) => l.startsWith('seal | '));
  assert.ok(sealLine.includes('entries=2'));
  const r2 = runFail(['verify', '--log', log, '--strict']);
  assert.equal(r2.code, 1); // unsigned seal is a warning; --strict fails on warnings
});

test('seal: rewriting the chain tip after a seal fails verify', () => {
  const d = tmpdir();
  const log = seedLog(d, 2);
  run(['seal', '--log', log]);
  const ls = lines(log);
  // Rewrite the last event (the one the seal attested) keeping line count.
  const lastEventIdx = ls.length - 2;
  ls[lastEventIdx] = ls[lastEventIdx].replace('event 2', 'event two — tail rewrite');
  fs.writeFileSync(log, ls.join('\n') + '\n');
  const r = runFail(['verify', '--log', log]);
  assert.equal(r.code, 1);
  assert.match(r.stdout + r.stderr, /head mismatch/);
});

test('seal: events may continue after a seal (periodic attestation)', () => {
  const d = tmpdir();
  const log = seedLog(d, 2);
  run(['seal', '--log', log]);
  run(['log', '--log', log, '--type', 'escalation', '--actor', 'gate', '--description', 'post-seal event']);
  run(['seal', '--log', log]);
  const out = run(['verify', '--log', log]);
  assert.match(out, /OK — hash chain/);
  assert.match(out, /3 events · 2 seals/);
});

test('signed event: verifies with public key; tamper detected by signature', () => {
  const d = tmpdir();
  const { priv, pub } = keypair(d);
  const log = path.join(d, 'SECURITY_LOG.md');
  run(['log', '--log', log, '--type', 'policy-override', '--actor', 'admin',
    '--description', 'overrode risk gate', '--key-file', priv]);
  const line = lines(log)[0];
  assert.equal(line.split(' | ').length, 6);
  assert.ok(line.split(' | ')[5].startsWith('sig='));
  const out = run(['verify', '--log', log, '--pubkey-file', pub]);
  assert.match(out, /OK — hash chain/);
  // Flip a bit in the description — signature must catch it.
  const tampered = line.replace('overrode', 'never overrode');
  fs.writeFileSync(log, tampered + '\n');
  const r = runFail(['verify', '--log', log, '--pubkey-file', pub]);
  assert.equal(r.code, 1);
  assert.match(r.stdout + r.stderr, /signature INVALID/);
});

test('signed seal: verifies and catches forged seal', () => {
  const d = tmpdir();
  const { priv, pub } = keypair(d);
  const log = seedLog(d, 2);
  run(['seal', '--log', log, '--key-file', priv]);
  const sealLine = lines(log).find((l) => l.startsWith('seal | '));
  assert.ok(sealLine.includes('alg=ed25519'));
  const out = run(['verify', '--log', log, '--pubkey-file', pub]);
  assert.match(out, /OK — hash chain/);
  // Forge the seal's entry count without re-signing.
  const forged = sealLine.replace('entries=2', 'entries=1');
  fs.writeFileSync(log, lines(log).map((l) => (l === sealLine ? forged : l)).join('\n') + '\n');
  const r = runFail(['verify', '--log', log, '--pubkey-file', pub]);
  assert.equal(r.code, 1);
});

test('keygen: produces ed25519 pair usable by log/verify', () => {
  const d = tmpdir();
  run(['keygen', '--out-dir', d]);
  const priv = path.join(d, 'ai-sdlc-security-log.key.pem');
  const pub = path.join(d, 'ai-sdlc-security-log.pub.pem');
  assert.ok(fs.existsSync(priv) && fs.existsSync(pub));
  const log = path.join(d, 'SECURITY_LOG.md');
  run(['log', '--log', log, '--type', 'log-initialized', '--actor', 't', '--description', 'genesis', '--key-file', priv]);
  const out = run(['verify', '--log', log, '--pubkey-file', pub]);
  assert.match(out, /OK — hash chain/);
});

test('head: prints sha256 of last event line', () => {
  const d = tmpdir();
  const log = seedLog(d, 2);
  const head = run(['head', '--log', log]).trim();
  const last = lines(log).pop();
  const sha = crypto.createHash('sha256').update(last).digest('hex');
  assert.equal(head, sha);
});

test('comments and blank lines are ignored by verify', () => {
  const d = tmpdir();
  const log = seedLog(d, 2);
  const ls = lines(log);
  fs.writeFileSync(log, `# security log\n\n${ls.join('\n')}\n\n`);
  const out = run(['verify', '--log', log]);
  assert.match(out, /OK — hash chain/);
});

test('verify: missing log file exits 2', () => {
  const d = tmpdir();
  const r = runFail(['verify', '--log', path.join(d, 'nope.md')]);
  assert.equal(r.code, 2);
});

test('verify: malformed line fails', () => {
  const d = tmpdir();
  const log = seedLog(d, 1);
  fs.appendFileSync(log, 'not a valid line\n');
  const r = runFail(['verify', '--log', log]);
  assert.equal(r.code, 1);
  assert.match(r.stdout + r.stderr, /malformed|expected 5 or 6/);
});

test('log: rejects fields containing pipe or newline', () => {
  const d = tmpdir();
  const log = path.join(d, 'SECURITY_LOG.md');
  const r = runFail(['log', '--log', log, '--type', 'escalation', '--actor', 'a|b', '--description', 'x']);
  assert.equal(r.code, 2);
  const r2 = runFail(['log', '--log', log, '--type', 'escalation', '--actor', 'a', '--description', 'x\ny']);
  assert.equal(r2.code, 2);
});

test('log: invalid type slug exits 2; non-canonical slug accepted with notice', () => {
  const d = tmpdir();
  const log = path.join(d, 'SECURITY_LOG.md');
  const r = runFail(['log', '--log', log, '--type', 'Bad Type', '--actor', 'a', '--description', 'x']);
  assert.equal(r.code, 2);
  const out = run(['log', '--log', log, '--type', 'custom-class', '--actor', 'a', '--description', 'x']);
  assert.match(out, /non-canonical event type/);
});

test('log: appending to a corrupt log is refused', () => {
  const d = tmpdir();
  const log = seedLog(d, 1);
  fs.appendFileSync(log, 'garbage line\n');
  const r = runFail(['log', '--log', log, '--type', 'escalation', '--actor', 'a', '--description', 'x']);
  assert.equal(r.code, 2);
  assert.match(r.stderr, /corrupt log/);
});
