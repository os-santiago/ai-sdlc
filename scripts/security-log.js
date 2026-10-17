#!/usr/bin/env node
/*
 * AI-SDLC security event log — tamper-evident, hash-chained audit trail of
 * security-relevant events (issue #106; epic #89 "EU CRA alignment").
 *
 * Every security-relevant event — sanitization blocks, kill-switch toggles,
 * escalations, policy overrides, vulnerability-handling actions — appends
 * one line to SECURITY_LOG.md in the contract format:
 *
 *   <prev-sha256> | <ISO-8601 ts> | <actor> | <type> | <description>[ | sig=<b64>]
 *
 * `prev-sha256` is the SHA-256 of the previous event line verbatim (raw
 * bytes, no trailing newline); the first event chains from GENESIS
 * ('0'*64). Editing, deleting, or reordering any historical event breaks
 * every successor's stored `prev` — `verify` recomputes the chain and
 * fails. This is the same chaining discipline as the run ledger
 * (.github/actions/run-ledger/run-ledger.js): canonical/sha256/GENESIS and
 * the Ed25519 seal pattern are imported from it, not reimplemented.
 *
 * Because an event's own hash is only ever stored in a *successor*, the
 * tail of an unsealed, unsigned log can be rewritten without breaking the
 * chain. Two controls close that gap:
 *
 *   - per-event signatures (`log --key-...`) append `sig=<b64>`: an
 *     Ed25519 signature over the event's canonical payload — every event
 *     becomes self-authenticating;
 *   - periodic `seal` lines (`seal --key-...`) attest the current chain
 *     head + event count, optionally Ed25519-signed. Seals are NOT
 *     terminal: the log continues after a seal, and a later seal attests
 *     the new tip. The weekly workflow
 *     (.github/workflows/security-log-sign.yml) signs the whole file with
 *     cosign and stores the bundle as a release asset, which also pins
 *     the tail.
 *
 * Line grammar (blank lines and `#` comments are ignored):
 *   event: <64-hex prev> | <ts> | <actor> | <type> | <description>[ | sig=<b64>]
 *   seal:  seal | <ts> | head=<64-hex> | entries=<n> | unsigned
 *        | seal | <ts> | head=<64-hex> | entries=<n> | alg=<alg> | key_id=<16hex> | sig=<b64>
 * Fields may not contain '|' or newlines. `type` is a lowercase slug
 * (^[a-z0-9][a-z0-9-]{0,63}$); EVENT_TYPES lists the canonical vocabulary
 * but non-canonical slugs are accepted with a notice — callers own their
 * event classes.
 *
 * Ops:
 *   log    --type T --description D [--actor A] [--log PATH]
 *          [--key FILE | --key-env VAR | --key PEM]
 *   seal   [--log PATH] [--key FILE | --key-env VAR | --key PEM]
 *   verify [--log PATH] [--pubkey-file F | --pubkey-env VAR] [--strict]
 *   head   [--log PATH]          # print sha256 of last event line
 *   keygen [--out-dir DIR]       # Ed25519 pair for --key / --pubkey-file
 *
 * Bounds (env-overridable): AI_SDLC_SECLOG_MAX_ENTRIES=10000,
 *   AI_SDLC_SECLOG_MAX_BYTES=8MiB (log file),
 *   AI_SDLC_SECLOG_MAX_FIELD_BYTES=16KiB (per field).
 *
 * Exit codes: 0 ok · 1 verification/tamper failure · 2 usage or I/O error.
 */
'use strict';

const fs = require('fs');
const path = require('path');
const crypto = require('crypto');

const { canonical, sha256, GENESIS } =
  require('../.github/actions/run-ledger/run-ledger.js');

const FORMAT = 'ai-sdlc-security-log';
const FORMAT_V = 1;
const DEFAULT_LOG = 'SECURITY_LOG.md';

const MAX_ENTRIES = intEnv('AI_SDLC_SECLOG_MAX_ENTRIES', 10000);
const MAX_BYTES = intEnv('AI_SDLC_SECLOG_MAX_BYTES', 8 * 1024 * 1024);
const MAX_FIELD_BYTES = intEnv('AI_SDLC_SECLOG_MAX_FIELD_BYTES', 16 * 1024);

// Canonical event vocabulary (not exhaustive — callers may emit other
// slug-valid types; these are the classes the runtime itself writes).
const EVENT_TYPES = [
  'sanitization-block',   // injection-defense scrubber rejected/redacted content
  'kill-switch-engage',   // ai-sdlc:off engaged (scripts/kill-switch.js)
  'kill-switch-release',  // ai-sdlc:off released
  'escalation',           // needs-human escalation (automerge, repair stall, …)
  'policy-override',      // org-policy / contract override applied or rejected
  'vulnerability-report', // incoming vulnerability disclosure (epic #89)
  'disclosure-action',    // advisory published, fix shipped, CVE coordinated
  'log-initialized',      // genesis event — the log file was created
];

function intEnv(name, dflt) {
  const v = Number(process.env[name]);
  return Number.isFinite(v) && v > 0 ? Math.floor(v) : dflt;
}

function die(msg, code = 2) {
  console.error(`security-log: ${msg}`);
  process.exit(code);
}

/* --------------------------------- parsing ---------------------------------- */

// Strict reader: appending onto a corrupt log would bury the corruption
// evidence, so a malformed line is fatal here (verify reports it instead).
function readLog(file) {
  const out = { file, lines: [], events: [], seals: [] };
  if (!fs.existsSync(file)) return out;
  const st = fs.statSync(file);
  if (st.size > MAX_BYTES) throw new Error(`log exceeds cap (${st.size} > ${MAX_BYTES} bytes): ${file}`);
  out.lines = fs.readFileSync(file, 'utf8').split('\n');
  let sealedAt = 0;
  for (const [i, line] of out.lines.entries()) {
    const n = i + 1;
    if (!line.trim() || line.startsWith('#')) continue;
    if (line.startsWith('seal | ')) {
      out.seals.push({ lineNo: n, ...parseSealLine(line, n) });
      sealedAt = out.events.length;
      continue;
    }
    const ev = parseEventLine(line, n);
    ev.sealedBefore = sealedAt; // events counted so far when the next seal lands
    out.events.push(ev);
  }
  return out;
}

function parseEventLine(line, lineNo) {
  const where = `line ${lineNo}`;
  const parts = line.split(' | ');
  if (parts.length !== 5 && parts.length !== 6) {
    throw new Error(`${where}: expected 5 or 6 pipe-separated fields, got ${parts.length}`);
  }
  const [prev, ts, actor, etype, description] = parts;
  if (!/^[0-9a-f]{64}$/.test(prev)) throw new Error(`${where}: prev is not a sha256 hex`);
  if (Number.isNaN(Date.parse(ts))) throw new Error(`${where}: bad timestamp "${ts}"`);
  if (!actor) throw new Error(`${where}: empty actor`);
  if (!/^[a-z0-9][a-z0-9-]{0,63}$/.test(etype)) throw new Error(`${where}: invalid event type "${etype}"`);
  if (!description) throw new Error(`${where}: empty description`);
  let sig = null;
  if (parts.length === 6) {
    if (!parts[5].startsWith('sig=')) throw new Error(`${where}: sixth field must be sig=<base64>`);
    sig = parts[5].slice(4);
    if (!sig) throw new Error(`${where}: empty signature`);
  }
  return { lineNo, raw: line, prev, ts, actor, etype, description, sig, hash: sha256(line) };
}

function parseSealLine(line, lineNo) {
  const where = `seal at line ${lineNo}`;
  const parts = line.split(' | ');
  const kv = {};
  for (const p of parts.slice(2)) {
    const eq = p.indexOf('=');
    if (eq < 0) {
      if (p === 'unsigned') { kv.unsigned = true; continue; }
      throw new Error(`${where}: malformed field "${p}"`);
    }
    kv[p.slice(0, eq)] = p.slice(eq + 1);
  }
  if (!/^[0-9a-f]{64}$/.test(kv.head || '')) throw new Error(`${where}: missing/invalid head=`);
  const entries = Number(kv.entries);
  if (!Number.isInteger(entries) || entries < 0) throw new Error(`${where}: missing/invalid entries=`);
  const signed = !kv.unsigned;
  if (signed) {
    if (!kv.alg || !kv.sig) throw new Error(`${where}: signed seal missing alg=/sig=`);
  } else if (parts[parts.length - 1] !== 'unsigned') {
    throw new Error(`${where}: unsigned seal must end with "unsigned"`);
  }
  return { raw: line, ts: parts[1], head: kv.head, entries, signed, alg: kv.alg || null, key_id: kv.key_id || null, sig: kv.sig || null };
}

/* --------------------------------- payloads --------------------------------- */

function eventPayload(ev) {
  return canonical({
    v: FORMAT_V, type: `${FORMAT}-event`,
    prev: ev.prev, ts: ev.ts, actor: ev.actor,
    etype: ev.etype, description: ev.description,
  });
}

function sealPayload(head, entries) {
  return canonical({ v: FORMAT_V, type: `${FORMAT}-seal`, head, entries });
}

function loadPrivateKey(opts) {
  const pem = opts.key || (opts['key-env'] ? process.env[opts['key-env']] : null)
    || (opts['key-file'] && fs.existsSync(opts['key-file']) ? fs.readFileSync(opts['key-file'], 'utf8') : null)
    || (opts['key-file'] ? die(`key file not found: ${opts['key-file']}`) : null);
  if (!pem || !pem.trim()) return null;
  try { return crypto.createPrivateKey(pem); } catch (e) { die(`invalid signing key: ${e.message}`); }
  return null;
}

function signPayload(payload, key) {
  const sigAlg = key.asymmetricKeyType === 'ed25519' ? null : 'sha256';
  return {
    alg: sigAlg === null ? 'ed25519' : `${key.asymmetricKeyType}-sha256`,
    sigAlg,
    keyId: sha256(crypto.createPublicKey(key).export({ format: 'der', type: 'spki' })).slice(0, 16),
    sig: crypto.sign(sigAlg, Buffer.from(payload), key).toString('base64'),
  };
}

/* ------------------------------------ log ------------------------------------ */

function checkField(name, v) {
  if (v === undefined || v === null || v === '') die(`--${name} is required`);
  if (v.includes('|') || /[\r\n]/.test(v)) die(`--${name} must not contain '|' or newlines`);
  if (Buffer.byteLength(v) > MAX_FIELD_BYTES) die(`--${name} exceeds cap (${MAX_FIELD_BYTES} bytes)`);
  return v;
}

function cmdLog(opts) {
  const file = opts.log || DEFAULT_LOG;
  const etype = checkField('type', (opts.type || '').trim());
  if (!/^[a-z0-9][a-z0-9-]{0,63}$/.test(etype)) die(`invalid event type: ${etype}`);
  const description = checkField('description', opts.description);
  const actor = checkField('actor',
    opts.actor || process.env.GITHUB_ACTOR || process.env.USER || 'local');
  if (!EVENT_TYPES.includes(etype)) {
    console.log(`security-log: notice — non-canonical event type "${etype}" (canonical: ${EVENT_TYPES.join(', ')})`);
  }

  let prior;
  try { prior = readLog(file); } catch (e) { die(`refusing to append to a corrupt log — ${e.message}`); }
  if (prior.events.length >= MAX_ENTRIES) die(`entry cap reached (${MAX_ENTRIES}): ${file}`);

  const prev = prior.events.length ? prior.events[prior.events.length - 1].hash : GENESIS;
  const ev = { v: FORMAT_V, seq: prior.events.length + 1, prev, ts: new Date().toISOString(), actor, etype, description };

  const key = loadPrivateKey(opts);
  let line = `${ev.prev} | ${ev.ts} | ${ev.actor} | ${ev.etype} | ${ev.description}`;
  if (key) {
    const s = signPayload(eventPayload(ev), key);
    line += ` | sig=${s.sig}`;
    ev.alg = s.alg;
  }

  fs.mkdirSync(path.dirname(file) === '.' ? '.' : path.dirname(file), { recursive: true });
  fs.appendFileSync(file, line + '\n');
  console.log(`security-log: event seq=${ev.seq} type=${etype} actor=${actor}${key ? ' signed' : ''} prev=${prev.slice(0, 16)}…`);
  return ev;
}

/* ------------------------------------ seal ----------------------------------- */

function cmdSeal(opts) {
  const file = opts.log || DEFAULT_LOG;
  if (!fs.existsSync(file)) die(`log not found: ${file}`);
  let prior;
  try { prior = readLog(file); } catch (e) { die(e.message); }
  if (!prior.events.length) die(`refusing to seal an empty log: ${file}`);

  const head = prior.events[prior.events.length - 1].hash;
  const count = prior.events.length;
  const key = loadPrivateKey(opts);

  let tail;
  if (key) {
    const s = signPayload(sealPayload(head, count), key);
    tail = `alg=${s.alg} | key_id=${s.keyId} | sig=${s.sig}`;
  } else {
    tail = 'unsigned';
    console.log('security-log: no signing key provided — seal recorded unsigned (hash chain still tamper-evident)');
  }
  const line = `seal | ${new Date().toISOString()} | head=${head} | entries=${count} | ${tail}`;
  fs.appendFileSync(file, line + '\n');
  console.log(`security-log: sealed head=${head.slice(0, 16)}… entries=${count} signed=${!!key}`);
  return { head, entries: count, signed: !!key };
}

/* ----------------------------------- verify ---------------------------------- */

function verifyLog(file, pub) {
  const problems = [];
  const warnings = [];
  let parsed;
  try {
    parsed = readLog(file);
  } catch (e) {
    return { file, events: 0, seals: 0, problems: [`malformed log — ${e.message}`], warnings };
  }
  const { events, seals } = parsed;
  if (!events.length) problems.push('no events');

  events.forEach((ev, i) => {
    const where = `event ${i + 1} (line ${ev.lineNo}, type=${ev.etype})`;
    const expectedPrev = i === 0 ? GENESIS : events[i - 1].hash;
    if (ev.prev !== expectedPrev) {
      problems.push(`${where}: prev != previous event hash — chain break (inserted, deleted, reordered, or edited entry)`);
    }
    if (ev.sig) {
      if (!pub) {
        warnings.push(`${where}: signed but no public key provided (--pubkey-file/--pubkey-env)`);
      } else {
        try {
          const sigAlg = 'sha256'; // Ed25519 signs with null algorithm
          const key = crypto.createPublicKey(pub);
          const alg = key.asymmetricKeyType === 'ed25519' ? null : sigAlg;
          const ok = crypto.verify(alg, Buffer.from(eventPayload(ev)), key, Buffer.from(ev.sig, 'base64'));
          if (!ok) problems.push(`${where}: signature INVALID — event content modified or re-signed`);
        } catch (e) {
          problems.push(`${where}: signature check failed: ${e.message}`);
        }
      }
    }
  });

  // Seals attest (head, count) at their position: the head must equal the
  // hash of the last event line BEFORE the seal, and entries must equal
  // the number of events before it. Multiple periodic seals are fine.
  seals.forEach((seal) => {
    const where = `seal at line ${seal.lineNo}`;
    const before = events.filter((e) => e.lineNo < seal.lineNo);
    const tip = before.length ? before[before.length - 1] : null;
    if (seal.entries !== before.length) {
      problems.push(`${where}: entries mismatch (${seal.entries} != ${before.length}) — events appended/removed around seal`);
    }
    if (!tip || seal.head !== tip.hash) {
      problems.push(`${where}: head mismatch — chain tip at seal time does not match (tail rewritten)`);
    }
    if (seal.signed) {
      if (!pub) {
        warnings.push(`${where}: signed but no public key provided (--pubkey-file/--pubkey-env)`);
      } else {
        try {
          const key = crypto.createPublicKey(pub);
          const sigAlg = seal.alg === 'ed25519' ? null : 'sha256';
          const ok = crypto.verify(sigAlg, Buffer.from(sealPayload(seal.head, seal.entries)), key, Buffer.from(seal.sig, 'base64'));
          if (!ok) problems.push(`${where}: seal signature INVALID — seal forged or modified`);
          const fp = sha256(key.export({ format: 'der', type: 'spki' })).slice(0, 16);
          if (seal.key_id && seal.key_id !== fp) {
            warnings.push(`${where}: key_id ${seal.key_id} != provided key fingerprint ${fp} — signed by a different key`);
          }
        } catch (e) {
          problems.push(`${where}: seal signature check failed: ${e.message}`);
        }
      }
    } else {
      warnings.push(`${where}: unsigned — head/count attested, attribution not possible`);
    }
  });

  return { file, events: events.length, seals: seals.length, head: events.length ? events[events.length - 1].hash : null, problems, warnings };
}

function cmdVerify(opts) {
  const file = opts.log || DEFAULT_LOG;
  if (!fs.existsSync(file)) die(`log not found: ${file}`);

  let pem = opts['pubkey-env'] ? process.env[opts['pubkey-env']] : null;
  if (!pem && opts['pubkey-file']) {
    if (!fs.existsSync(opts['pubkey-file'])) die(`pubkey file not found: ${opts['pubkey-file']}`);
    pem = fs.readFileSync(opts['pubkey-file'], 'utf8');
  }

  const r = verifyLog(file, pem && pem.trim() ? pem : null);
  const flags = [`${r.events} events`, `${r.seals} seals`, `head=${r.head ? r.head.slice(0, 16) : '—'}…`];
  let exit = 0;
  console.log(`${r.problems.length === 0 ? '✓' : '✗'} ${r.file} — ${flags.join(' · ')}`);
  for (const w of r.warnings) {
    console.log(`    warning: ${w}`);
    if (opts.strict) exit = 1;
  }
  for (const p of r.problems) console.log(`    TAMPER: ${p}`);
  if (r.problems.length) exit = 1;
  if (exit === 0) console.log('security-log verify: OK — hash chain and signatures intact');
  else console.error('security-log verify: FAILED — tampering or structural violation detected');
  process.exit(exit);
}

/* ---------------------------------- keygen ----------------------------------- */

function cmdKeygen(opts) {
  const dir = opts['out-dir'] || '.';
  fs.mkdirSync(dir, { recursive: true });
  const { privateKey, publicKey } = crypto.generateKeyPairSync('ed25519');
  const privPath = path.join(dir, 'ai-sdlc-security-log.key.pem');
  const pubPath = path.join(dir, 'ai-sdlc-security-log.pub.pem');
  fs.writeFileSync(privPath, privateKey.export({ format: 'pem', type: 'pkcs8' }), { mode: 0o600 });
  fs.writeFileSync(pubPath, publicKey.export({ format: 'pem', type: 'spki' }));
  const fp = sha256(publicKey.export({ format: 'der', type: 'spki' })).slice(0, 16);
  console.log(`security-log keygen — ed25519 pair written (key_id=${fp})`);
  console.log(`  private: ${privPath} → store as a secret, then delete this file`);
  console.log(`  public:  ${pubPath} → commit (e.g. config/security-log.pub.pem); auditors pass it to verify via --pubkey-file`);
}

/* ------------------------------------ head ------------------------------------ */

function cmdHead(opts) {
  const file = opts.log || DEFAULT_LOG;
  let prior;
  try { prior = readLog(file); } catch (e) { die(e.message); }
  if (!prior.events.length) die(`empty log: ${file}`);
  console.log(prior.events[prior.events.length - 1].hash);
}

/* ------------------------------------ CLI ------------------------------------ */

function parseArgs(argv) {
  const opts = { _paths: [] };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a.startsWith('--')) {
      const k = a.slice(2);
      if (i + 1 < argv.length && !argv[i + 1].startsWith('--')) opts[k] = argv[++i];
      else opts[k] = true;
    } else opts._paths.push(a);
  }
  return opts;
}

function main() {
  const [op, ...rest] = process.argv.slice(2);
  const opts = parseArgs(rest);
  switch (op) {
    case 'log': cmdLog(opts); break;
    case 'seal': cmdSeal(opts); break;
    case 'verify': cmdVerify(opts); break;
    case 'head': cmdHead(opts); break;
    case 'keygen': cmdKeygen(opts); break;
    case undefined:
    case 'help':
    case '--help':
      console.log('usage: security-log.js <log|seal|verify|head|keygen> [flags] — see header comment');
      break;
    default:
      die(`unknown op: ${op}`);
  }
}

if (require.main === module) main();

module.exports = {
  EVENT_TYPES, FORMAT_V, GENESIS, DEFAULT_LOG,
  readLog, parseEventLine, parseSealLine, eventPayload, sealPayload, verifyLog,
};
