#!/usr/bin/env node
/*
 * AI-SDLC run ledger — tamper-evident, hash-chained per-stage run evidence
 * (issue #196; epic #148 "evidence and economics").
 *
 * Every runtime stage appends one JSONL entry carrying the SHA-256 of its
 * stage data plus the hash of the previous entry (`prev`), then seals the
 * file: a final record over the chain head, Ed25519-signed when the caller
 * provides the signing key (secret `AI_SDLC_LEDGER_KEY`; never committed).
 * Without a key the seal still records `signed: false` — the hash chain is
 * tamper-evident on its own, the signature is the trust root.
 *
 * Stages run in separate workflow invocations, so the chain spans workflow
 * runs via an explicit `prev` seed: each stage emits its sealed `head` as a
 * job output / PR marker (`<!-- ai-sdlc:ledger-head <64-hex> -->`) and the
 * next stage appends with `--prev <head>`. `verify` stitches multi-file
 * artifacts back into chains by matching first-entry `prev` to a prior
 * file's head.
 *
 * Ops:
 *   append --ledger F --stage S [--repo o/r] [--issue N] [--pr N]
 *          [--data JSON] [--data-file PATH]... [--prev HEX]
 *          [--engine ID] [--engine-registry PATH]
 *   seal   --ledger F [--key-file F | --key-env VAR | --key PEM]
 *   verify FILE_OR_DIR... [--pubkey-file F | --pubkey-env VAR] [--strict]
 *   head   --ledger F          # print the current chain head (last entry hash)
 *   keygen [--out-dir DIR]     # Ed25519 pair: private → secret, public → committed
 *
 * Engine (issue #230): `entry.engine` records the engine adapter id that
 * produced the stage — never a literal engine name. `--engine ID` is
 * validated against the adapter registry (EngineAdapterResolver +
 * config/engine_adapters.json; override with --engine-registry or
 * AI_SDLC_ENGINE_REGISTRY): an id that resolves no adapter fails loudly
 * instead of leaking a non-adapter string into evidence. Absent --engine
 * the field is null — the stage ran no resolvable engine (e.g. intake on
 * an unresolvable contract). Registered ids 'scc'/'devin' record exactly
 * the previous literals.
 *
 * Bounds (env-overridable): AI_SDLC_LEDGER_MAX_ENTRIES=1000,
 *   AI_SDLC_LEDGER_MAX_BYTES=4MiB (ledger file), AI_SDLC_LEDGER_MAX_DATA_BYTES=64KiB
 *   (per-entry data), AI_SDLC_LEDGER_MAX_FILES=20, AI_SDLC_LEDGER_MAX_FILE_BYTES=8MiB.
 *
 * Exit codes: 0 ok · 1 verification/tamper failure · 2 usage or I/O error.
 * Writes `head`, `entries`, `signed` to $GITHUB_OUTPUT when present.
 */
'use strict';

const fs = require('fs');
const path = require('path');
const crypto = require('crypto');

const GENESIS = '0'.repeat(64);
const FORMAT = 'ai-sdlc-run-ledger';
const FORMAT_V = 1;

const MAX_ENTRIES = intEnv('AI_SDLC_LEDGER_MAX_ENTRIES', 1000);
const MAX_BYTES = intEnv('AI_SDLC_LEDGER_MAX_BYTES', 4 * 1024 * 1024);
const MAX_DATA_BYTES = intEnv('AI_SDLC_LEDGER_MAX_DATA_BYTES', 64 * 1024);
const MAX_FILES = intEnv('AI_SDLC_LEDGER_MAX_FILES', 20);
const MAX_FILE_BYTES = intEnv('AI_SDLC_LEDGER_MAX_FILE_BYTES', 8 * 1024 * 1024);

function intEnv(name, dflt) {
  const v = Number(process.env[name]);
  return Number.isFinite(v) && v > 0 ? Math.floor(v) : dflt;
}

/* Deterministic canonicalization: object keys sorted recursively so a hash
 * never depends on producer key order. */
function canonical(value) {
  if (value === null || typeof value !== 'object') return JSON.stringify(value);
  if (Array.isArray(value)) return `[${value.map(canonical).join(',')}]`;
  const keys = Object.keys(value).sort();
  return `{${keys.map((k) => `${JSON.stringify(k)}:${canonical(value[k])}`).join(',')}}`;
}

const sha256 = (buf) => crypto.createHash('sha256').update(buf).digest('hex');

function die(msg, code = 2) {
  console.error(`run-ledger: ${msg}`);
  process.exit(code);
}

/* ---------------------------------- ledger ---------------------------------- */

function readLedger(file) {
  const out = { entries: [], seal: null, extra: 0 };
  if (!fs.existsSync(file)) return out;
  const st = fs.statSync(file);
  if (st.size > MAX_BYTES) throw new Error(`ledger exceeds cap (${st.size} > ${MAX_BYTES} bytes): ${file}`);
  const lines = fs.readFileSync(file, 'utf8').split('\n').filter((l) => l.trim());
  for (const [i, line] of lines.entries()) {
    let rec;
    try { rec = JSON.parse(line); } catch (e) { throw new Error(`malformed JSONL line ${i + 1} in ${file}: ${e.message}`); }
    if (rec && rec.type === 'seal') {
      if (out.seal) throw new Error(`multiple seal records in ${file}`);
      out.seal = rec;
    } else {
      if (out.seal) throw new Error(`entry after seal in ${file} (line ${i + 1}) — append after seal is not allowed`);
      out.entries.push(rec);
    }
  }
  return out;
}

/* The engine adapter contract ships in the same repository as this action
 * (repo root = three levels up from .github/actions/run-ledger). The
 * require stays lazy so `verify`/`keygen` offline use never touches it. */
function engineRegistryPath(override, canonicalDefault) {
  return override || process.env.AI_SDLC_ENGINE_REGISTRY
    || canonicalDefault
    || path.join(__dirname, '..', '..', '..', 'config', 'engine_adapters.json');
}

/* Validate an engine id against the adapter contract and return the
 * adapter's registered id (descriptor `name`) for the ledger entry.
 * Unknown ids and malformed descriptors die — a value that resolves no
 * adapter must never land in evidence under the `engine` field
 * (issue #230). */
function resolveEngineId(engineId, registryOverride) {
  const repoRoot = path.join(__dirname, '..', '..', '..');
  const { EngineAdapterResolver, DEFAULT_REGISTRY_PATH } =
    require(path.join(repoRoot, 'scripts', 'engine_adapter.js'));
  const registry = engineRegistryPath(registryOverride, DEFAULT_REGISTRY_PATH);
  let resolver;
  try {
    resolver = new EngineAdapterResolver(registry);
  } catch (e) {
    die(`engine adapter registry unavailable (${e.message})`);
  }
  let adapter;
  try {
    adapter = resolver.getEngineAdapter(engineId);
  } catch (e) {
    die(`engine "${engineId}" is not a registered adapter id: ${e.message}`);
  }
  return adapter.name;
}

function emitOutput(kv) {
  if (process.env.GITHUB_OUTPUT) {
    try {
      for (const [k, v] of Object.entries(kv)) fs.appendFileSync(process.env.GITHUB_OUTPUT, `${k}=${v}\n`);
    } catch (_) { /* output channel is best-effort */ }
  }
}

function runContext() {
  const e = process.env;
  if (!e.GITHUB_RUN_ID) return null;
  const run = {
    id: e.GITHUB_RUN_ID,
    attempt: e.GITHUB_RUN_ATTEMPT || null,
    workflow: e.GITHUB_WORKFLOW || null,
    job: e.GITHUB_JOB || null,
    event: e.GITHUB_EVENT_NAME || null,
    sha: e.GITHUB_SHA || null,
    actor: e.GITHUB_ACTOR || null,
  };
  if (e.GITHUB_SERVER_URL && e.GITHUB_REPOSITORY) {
    run.url = `${e.GITHUB_SERVER_URL}/${e.GITHUB_REPOSITORY}/actions/runs/${e.GITHUB_RUN_ID}`;
  }
  return run;
}

function cmdAppend(opts) {
  const ledger = opts.ledger || die('--ledger is required');
  const stage = (opts.stage || '').trim() || die('--stage is required');
  if (!/^[a-z0-9][a-z0-9-]{0,63}$/.test(stage)) die(`invalid stage name: ${stage}`);

  let data = {};
  if (opts.data) {
    try { data = JSON.parse(opts.data); } catch (e) { die(`--data is not valid JSON: ${e.message}`); }
    if (data === null || typeof data !== 'object' || Array.isArray(data)) die('--data must be a JSON object');
  }

  const files = [];
  for (const f of (opts['data-file'] || []).slice(0, MAX_FILES)) {
    try {
      const st = fs.statSync(f);
      if (!st.isFile()) continue;
      if (st.size > MAX_FILE_BYTES) {
        files.push({ path: path.basename(f), bytes: st.size, sha256: null, note: 'over-cap-not-hashed' });
      } else {
        files.push({ path: path.basename(f), bytes: st.size, sha256: sha256(fs.readFileSync(f)) });
      }
    } catch (_) { /* absent evidence files are skipped, never fatal */ }
  }
  if (files.length) data.files = files;

  const dataJson = canonical(data);
  if (Buffer.byteLength(dataJson) > MAX_DATA_BYTES) {
    die(`stage data exceeds cap (${Buffer.byteLength(dataJson)} > ${MAX_DATA_BYTES} bytes)`);
  }

  let prevList;
  try { prevList = readLedger(ledger); } catch (e) { die(e.message); }
  if (prevList.seal) die(`ledger already sealed — append refused: ${ledger}`);
  if (prevList.entries.length >= MAX_ENTRIES) die(`entry cap reached (${MAX_ENTRIES}): ${ledger}`);

  const prev = prevList.entries.length
    ? prevList.entries[prevList.entries.length - 1].hash
    : (opts.prev || GENESIS);
  if (prev !== GENESIS && !/^[0-9a-f]{64}$/.test(prev)) die(`invalid --prev hash: ${prev}`);

  const engine = (opts.engine || '').trim()
    ? resolveEngineId(opts.engine.trim(), opts['engine-registry'])
    : null;

  const entry = {
    v: FORMAT_V,
    seq: prevList.entries.length + 1,
    stage,
    ts: new Date().toISOString(),
    repo: opts.repo || process.env.GITHUB_REPOSITORY || null,
    issue: opts.issue ? Number(opts.issue) : null,
    pr: opts.pr ? Number(opts.pr) : null,
    engine,
    run: runContext(),
    data,
    data_sha256: sha256(dataJson),
    prev,
  };
  entry.hash = sha256(canonical(entry));

  fs.mkdirSync(path.dirname(ledger), { recursive: true });
  fs.appendFileSync(ledger, JSON.stringify(entry) + '\n');

  console.log(`run-ledger: appended seq=${entry.seq} stage=${stage} engine=${engine ?? '—'} hash=${entry.hash} prev=${prev.slice(0, 16)}…`);
  emitOutput({ head: entry.hash, entries: entry.seq });
  return entry;
}

/* ----------------------------------- seal ----------------------------------- */

function sealPayload(head, entries) {
  return canonical({ v: FORMAT_V, type: `${FORMAT}-seal`, head, entries });
}

function cmdSeal(opts) {
  const ledger = opts.ledger || die('--ledger is required');
  let entries, seal;
  try { ({ entries, seal } = readLedger(ledger)); } catch (e) { die(e.message); }
  if (seal) die(`ledger already sealed: ${ledger}`);
  if (!entries.length) die(`refusing to seal an empty ledger: ${ledger}`);

  const head = entries[entries.length - 1].hash;
  const record = {
    v: FORMAT_V,
    type: 'seal',
    ts: new Date().toISOString(),
    head,
    entries: entries.length,
    alg: null,
    signed: false,
    key_id: null,
    sig: null,
  };

  const pem = opts.key || (opts['key-env'] ? process.env[opts['key-env']] : null)
    || (opts['key-file'] && fs.readFileSync(opts['key-file'], 'utf8'));
  if (pem && pem.trim()) {
    let key;
    try { key = crypto.createPrivateKey(pem); } catch (e) { die(`invalid signing key: ${e.message}`); }
    const pubDer = crypto.createPublicKey(key).export({ format: 'der', type: 'spki' });
    // Ed25519 signs with a null algorithm; RSA/EC sign sha256 digests.
    const sigAlg = key.asymmetricKeyType === 'ed25519' ? null : 'sha256';
    record.alg = sigAlg === null ? 'ed25519' : `${key.asymmetricKeyType}-sha256`;
    record.signed = true;
    record.key_id = sha256(pubDer).slice(0, 16);
    try {
      record.sig = crypto.sign(sigAlg, Buffer.from(sealPayload(head, record.entries)), key).toString('base64');
    } catch (e) { die(`signing failed: ${e.message}`); }
  } else {
    console.log('run-ledger: no signing key provided — seal is recorded unsigned (hash chain still tamper-evident)');
    console.log('::notice title=ai-sdlc run-ledger::unsigned seal — set the AI_SDLC_LEDGER_KEY secret to sign stage evidence');
  }

  fs.appendFileSync(ledger, JSON.stringify(record) + '\n');
  console.log(`run-ledger: sealed head=${head} entries=${record.entries} signed=${record.signed}`);
  emitOutput({ head, entries: record.entries, signed: String(record.signed) });
  return record;
}

/* ---------------------------------- verify ---------------------------------- */

function verifyFile(file, pub) {
  const problems = [];
  const warnings = [];
  let entries = [], seal = null;
  try {
    ({ entries, seal } = readLedger(file)); // structural order enforced by readLedger
  } catch (e) {
    // readLedger die()s via process.exit — intercept it for per-file reporting.
    return { file, entries: 0, head: null, firstPrev: null, sealed: false, signed: false,
             problems: [`unreadable/malformed ledger — ${e.message}`], warnings, fatalExit: true };
  }

  if (!entries.length) problems.push('no entries');

  entries.forEach((e, i) => {
    const where = `entry ${i + 1} (seq=${e.seq ?? '?'}, stage=${e.stage ?? '?'})`;
    if (e.v !== FORMAT_V) problems.push(`${where}: unknown format version ${e.v}`);
    if (e.seq !== i + 1) problems.push(`${where}: seq gap (expected ${i + 1})`);
    if (typeof e.stage !== 'string' || !e.stage) problems.push(`${where}: missing stage`);
    if (typeof e.ts !== 'string' || Number.isNaN(Date.parse(e.ts))) problems.push(`${where}: bad timestamp`);
    if (i > 0 && e.prev !== entries[i - 1].hash) problems.push(`${where}: prev != previous entry hash — chain break`);
    const { hash, ...rest } = e;
    if (!/^[0-9a-f]{64}$/.test(hash || '')) problems.push(`${where}: missing/invalid hash`);
    else if (sha256(canonical(rest)) !== hash) problems.push(`${where}: entry hash mismatch — content modified`);
    if (e.data !== undefined && sha256(canonical(e.data)) !== e.data_sha256) {
      problems.push(`${where}: data_sha256 mismatch — stage data modified`);
    }
  });

  const head = entries.length ? entries[entries.length - 1].hash : null;

  if (!seal) {
    warnings.push('unsealed (append-only ledger, no completion seal)');
  } else {
    if (seal.head !== head) problems.push('seal head mismatch — chain truncated or extended after seal');
    if (seal.entries !== entries.length) problems.push(`seal entries mismatch (${seal.entries} != ${entries.length})`);
    if (seal.signed) {
      if (!pub) {
        problems.push('seal is signed but no public key was provided (--pubkey-file/--pubkey-env)');
      } else {
        try {
          const sigAlg = seal.alg === 'ed25519' ? null : 'sha256';
          const ok = crypto.verify(sigAlg, Buffer.from(sealPayload(seal.head, seal.entries)), pub, Buffer.from(seal.sig, 'base64'));
          if (!ok) problems.push('seal signature INVALID — ledger was re-signed or modified');
          const fp = sha256(pub.export({ format: 'der', type: 'spki' })).slice(0, 16);
          if (seal.key_id && seal.key_id !== fp) warnings.push(`seal key_id ${seal.key_id} != provided key fingerprint ${fp} — signed by a different key`);
        } catch (e) {
          problems.push(`seal signature check failed: ${e.message}`);
        }
      }
    } else {
      warnings.push('seal unsigned — chain integrity checked, attribution not possible');
    }
  }

  return { file, entries: entries.length, head, firstPrev: entries[0] ? entries[0].prev : null, sealed: !!seal, signed: !!(seal && seal.signed), problems, warnings };
}

function cmdVerify(opts) {
  const paths = opts._paths || [];
  if (!paths.length) die('verify requires at least one ledger file or directory');

  const files = [];
  const visit = (p) => {
    const st = fs.statSync(p);
    if (st.isDirectory()) {
      for (const name of fs.readdirSync(p).sort()) {
        if (name.endsWith('.jsonl') || name.endsWith('.json')) visit(path.join(p, name));
      }
    } else files.push(p);
  };
  for (const p of paths) {
    if (!fs.existsSync(p)) die(`path not found: ${p}`);
    visit(p);
  }
  if (!files.length) die('no ledger files found');

  let pem = opts['pubkey-env'] ? process.env[opts['pubkey-env']] : null;
  if (!pem && opts['pubkey-file']) pem = fs.readFileSync(opts['pubkey-file'], 'utf8');
  const pub = pem && pem.trim() ? crypto.createPublicKey(pem) : null;

  const results = files.map((f) => verifyFile(f, pub));

  // Cross-file stitching: a file whose first entry `prev` equals another
  // file's head continues that chain — this is how per-stage artifacts
  // reassemble the whole pipeline run.
  const byHead = new Map(results.filter((r) => r.head).map((r) => [r.head, r]));
  for (const r of results) {
    if (r.firstPrev === GENESIS || r.firstPrev === null) { r.link = 'genesis'; continue; }
    const parent = byHead.get(r.firstPrev);
    if (parent && parent.file !== r.file) r.link = `chains from ${path.basename(parent.file)}`;
    else {
      r.link = 'unresolved — prior-stage ledger not in the verified set';
      r.warnings.push(`first entry prev=${r.firstPrev.slice(0, 16)}… resolves to no head in this set`);
    }
  }
  // Cycle guard: a file can chain from at most one other file.
  const seen = new Map();
  for (const r of results) {
    if (!r.link || !r.link.startsWith('chains from ')) continue;
    const key = r.link;
    if (seen.has(key)) {
      r.problems.push(`ambiguous chain — ${r.file} and ${seen.get(key)} both claim the same parent head`);
      seen.get(key).problems.push('ambiguous chain — claimed by multiple descendants');
    } else seen.set(key, r.file);
  }

  let exit = 0;
  console.log(`run-ledger verify — ${files.length} file(s)`);
  for (const r of results) {
    const ok = r.problems.length === 0;
    if (!ok) exit = 1;
    const flags = [`${r.entries} entries`, `head=${r.head ? r.head.slice(0, 16) : '—'}…`,
      r.signed ? 'signed' : 'unsigned', `link=${r.link}`];
    console.log(`${ok ? '✓' : '✗'} ${r.file} — ${flags.join(' · ')}`);
    for (const w of r.warnings) {
      console.log(`    warning: ${w}`);
      if (opts.strict) exit = 1;
    }
    for (const p of r.problems) console.log(`    TAMPER: ${p}`);
  }
  if (exit === 0) console.log('run-ledger verify: OK — chain and signatures intact');
  else console.error('run-ledger verify: FAILED — tampering or structural violation detected');
  process.exit(exit);
}

/* ---------------------------------- keygen ---------------------------------- */

function cmdKeygen(opts) {
  const dir = opts['out-dir'] || '.';
  fs.mkdirSync(dir, { recursive: true });
  const { privateKey, publicKey } = crypto.generateKeyPairSync('ed25519');
  const privPem = privateKey.export({ format: 'pem', type: 'pkcs8' });
  const pubPem = publicKey.export({ format: 'pem', type: 'spki' });
  const privPath = path.join(dir, 'ai-sdlc-ledger.key.pem');
  const pubPath = path.join(dir, 'ai-sdlc-ledger.pub.pem');
  fs.writeFileSync(privPath, privPem, { mode: 0o600 });
  fs.writeFileSync(pubPath, pubPem);
  const fp = sha256(publicKey.export({ format: 'der', type: 'spki' })).slice(0, 16);
  console.log(`run-ledger keygen — ed25519 pair written (key_id=${fp})`);
  console.log(`  private: ${privPath} → store as the AI_SDLC_LEDGER_KEY secret, then delete this file`);
  console.log(`  public:  ${pubPath} → commit; auditors pass it to verify via --pubkey-file`);
}

/* ------------------------------------ CLI ------------------------------------ */

function parseArgs(argv) {
  const opts = { _paths: [] };
  const MULTI = new Set(['data-file']);
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a.startsWith('--')) {
      const k = a.slice(2);
      if (i + 1 < argv.length && !argv[i + 1].startsWith('--')) {
        if (MULTI.has(k)) (opts[k] = opts[k] || []).push(argv[++i]);
        else opts[k] = argv[++i];
      } else opts[k] = true;
    } else opts._paths.push(a);
  }
  return opts;
}

function cmdHead(opts) {
  const ledger = opts.ledger || die('--ledger is required');
  let entries;
  try { ({ entries } = readLedger(ledger)); } catch (e) { die(e.message); }
  if (!entries.length) die(`empty ledger: ${ledger}`);
  const head = entries[entries.length - 1].hash;
  console.log(head);
  return head;
}

function main() {
  const [op, ...rest] = process.argv.slice(2);
  const opts = parseArgs(rest);
  switch (op) {
    case 'append': cmdAppend(opts); break;
    case 'seal': cmdSeal(opts); break;
    case 'verify': cmdVerify(opts); break;
    case 'head': cmdHead(opts); break;
    case 'keygen': cmdKeygen(opts); break;
    case undefined:
    case 'help':
    case '--help':
      console.log('usage: run-ledger.js <append|seal|verify|head|keygen> [flags] — see header comment');
      break;
    default:
      die(`unknown op: ${op}`);
  }
}

if (require.main === module) main();

module.exports = { canonical, sha256, readLedger, sealPayload, resolveEngineId, engineRegistryPath, GENESIS };
