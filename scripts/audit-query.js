#!/usr/bin/env node
// scripts/audit-query.js — audit query over the signed run ledger (issue #174;
// epic #149 "fleet operations"). Ports the Hermes `audit_query.js` query model
// (scripts/audit_query.js in Axel-DaMage/hermes) onto the tamper-evident
// hash-chained ledger emitted by .github/actions/run-ledger.
//
// Post-incident questions — "why did the agent do X" — are answered from
// committed/uploaded ledger entries, so the trail survives workflow-log
// expiry (14-day artifact retention vs. the sealed evidence files the
// auditor downloads as a bundle).
//
// Model:
//   - load:   tolerant JSONL read — a malformed line is a flagged gap, never
//             a reason to drop the rest of the evidence file (forensics must
//             recover partial state; run-ledger.js readLedger hard-dies by
//             design because appends must not proceed on a corrupt file).
//   - check:  every entry's `hash`/`data_sha256`/`prev`/`seq` recomputed, the
//             seal re-verified (head, count, Ed25519/RSA signature when a
//             public key is supplied). Flagged entries stay in the trail —
//             marked — because the tamper evidence itself is the answer.
//   - stitch: files chain via first-entry `prev` == another file's `head`
//             (same rule as `run-ledger.js verify`); unresolved links are
//             reported, never forged.
//   - query:  stage-aware decision extraction answers structured questions:
//             why-escalated · why-skipped · why-labeled · why-merged ·
//             why-held · why-failed · why-dispatched.
//
// CLI:
//   node scripts/audit-query.js trace  --dir artifacts/ --issue 174
//   node scripts/audit-query.js trace  --ledger a.jsonl --ledger b.jsonl --pr 42
//   node scripts/audit-query.js why    escalated --dir artifacts/ --pr 42 [--json]
//   node scripts/audit-query.js labels --dir artifacts/ --issue 174
//   node scripts/audit-query.js trace  --run 12345678 --dir artifacts/
//           [--pubkey-file F | --pubkey-env V] [--strict]
//
// Selectors: --issue N (entry.issue), --pr N (entry.pr), --run ID
// (entry.run.id). No selector = every entry in the loaded set.
//
// Exit codes: 0 ok (including "no matching entries" — a miss is an answer)
//   1 integrity failure (only with --strict) · 2 usage / I/O error.
//
// Canonical label defaults (spec/labels.md, .ai-sdlc.yaml): entries carry the
// decision outputs (armed/dispatch/failed, action/reason); the applied label
// name is derived — repos may rename via `labels.*` / `escalation.label`, so
// derived names are reported as canonical defaults, not verbatim strings.
'use strict';

const fs = require('fs');
const path = require('path');
const crypto = require('crypto');

const LEDGER = require('../.github/actions/run-ledger/run-ledger.js');
const { canonical, sha256, sealPayload, GENESIS } = LEDGER;

const MAX_FILES = intEnv('AI_SDLC_AUDIT_MAX_FILES', 200);
const MAX_FILE_BYTES = intEnv('AI_SDLC_AUDIT_MAX_FILE_BYTES', 8 * 1024 * 1024);

// Canonical label names (overridable per-repo via the contract — reported as
// defaults; the ledger records the decision, not the resolved string).
const LABEL_QUEUED = 'scc:queued';
const LABEL_NOT_READY = 'scc:not-ready';
const LABEL_ESCALATION = 'needs-human';

const WHY_KINDS = ['escalated', 'skipped', 'labeled', 'merged', 'held', 'failed', 'dispatched'];

// scc exit contract (.github/workflows/ai-sdlc-implement.yml):
// 0 ok · 10 no-changes · 20 provider · 21 auth · 22 budget · 23 livelock
const ENGINE_EXIT = {
  0: 'ok', 10: 'no-changes', 20: 'provider-error', 21: 'auth-error',
  22: 'budget-exhausted', 23: 'livelock',
};

function intEnv(name, dflt) {
  const v = Number(process.env[name]);
  return Number.isFinite(v) && v > 0 ? Math.floor(v) : dflt;
}

function die(msg, code = 2) {
  console.error(`audit-query: ${msg}`);
  process.exit(code);
}

/* ------------------------------- load (tolerant) ------------------------------ */

function loadLedgerFile(file) {
  const out = { file, entries: [], seal: null, malformed: [], afterSeal: [] };
  let raw;
  try {
    const st = fs.statSync(file);
    if (st.size > MAX_FILE_BYTES) {
      out.malformed.push({ line: null, error: `file exceeds cap (${st.size} > ${MAX_FILE_BYTES} bytes) — not read` });
      return out;
    }
    raw = fs.readFileSync(file, 'utf8');
  } catch (e) {
    out.malformed.push({ line: null, error: `unreadable: ${e.message}` });
    return out;
  }
  const lines = raw.split('\n');
  let sealed = false;
  for (const [i, line] of lines.entries()) {
    const n = i + 1;
    if (!line.trim()) continue;
    let rec;
    try { rec = JSON.parse(line); } catch (e) {
      out.malformed.push({ line: n, error: `unparseable JSONL: ${e.message}` });
      continue;
    }
    if (rec && rec.type === 'seal') {
      if (out.seal) out.malformed.push({ line: n, error: 'second seal record — ignored' });
      else { out.seal = rec; sealed = true; }
      continue;
    }
    if (sealed) {
      // Entries after a seal are chain corruption (run-ledger.js dies on
      // this); the auditor keeps them visible, flagged.
      out.entries.push(rec);
      out.afterSeal.push(rec);
      continue;
    }
    out.entries.push(rec);
  }
  return out;
}

/* -------------------------------- integrity --------------------------------- */

function entryProblems(e, idx, prevEntry) {
  const p = [];
  const where = e && e.seq !== undefined ? `seq=${e.seq}` : `line-order ${idx + 1}`;
  if (!e || typeof e !== 'object') return [`entry ${where}: not an object`];
  if (e.v !== 1) p.push(`entry ${where}: unknown format version ${JSON.stringify(e.v)}`);
  if (e.seq !== idx + 1) p.push(`entry ${where}: seq gap (expected ${idx + 1})`);
  if (typeof e.stage !== 'string' || !e.stage) p.push(`entry ${where}: missing stage`);
  if (typeof e.ts !== 'string' || Number.isNaN(Date.parse(e.ts))) p.push(`entry ${where}: bad timestamp`);
  if (prevEntry && e.prev !== prevEntry.hash) p.push(`entry ${where}: prev != previous entry hash — chain break`);
  const { hash, ...rest } = e;
  if (!/^[0-9a-f]{64}$/.test(hash || '')) p.push(`entry ${where}: missing/invalid hash`);
  else if (sha256(canonical(rest)) !== hash) p.push(`entry ${where}: entry hash mismatch — content modified`);
  if (e.data !== undefined && sha256(canonical(e.data)) !== e.data_sha256) {
    p.push(`entry ${where}: data_sha256 mismatch — stage data modified`);
  }
  return p;
}

function sealCheck(seal, entries, pub, hasAfterSeal) {
  const r = { sealed: !!seal, signed: false, signature: 'none', problems: [], warnings: [] };
  const head = entries.length ? entries[entries.length - 1].hash : null;
  if (!seal) {
    r.warnings.push('unsealed (append-only ledger, no completion seal)');
    return r;
  }
  r.signed = !!seal.signed;
  if (hasAfterSeal) r.problems.push('entries found after seal — append after seal is not allowed');
  if (seal.head !== head) r.problems.push('seal head mismatch — chain truncated or extended after seal');
  if (seal.entries !== entries.length) r.problems.push(`seal entries mismatch (${seal.entries} != ${entries.length})`);
  if (seal.signed) {
    if (!pub) {
      r.signature = 'unverified';
      r.warnings.push('seal is signed but no public key was provided (--pubkey-file/--pubkey-env)');
    } else {
      try {
        const sigAlg = seal.alg === 'ed25519' ? null : 'sha256';
        const ok = crypto.verify(sigAlg, Buffer.from(sealPayload(seal.head, seal.entries)), pub, Buffer.from(seal.sig, 'base64'));
        r.signature = ok ? 'verified' : 'invalid';
        if (!ok) r.problems.push('seal signature INVALID — ledger was re-signed or modified');
        const fp = sha256(pub.export({ format: 'der', type: 'spki' })).slice(0, 16);
        if (seal.key_id && seal.key_id !== fp) {
          r.warnings.push(`seal key_id ${seal.key_id} != provided key fingerprint ${fp} — signed by a different key`);
        }
      } catch (e) {
        r.signature = 'error';
        r.problems.push(`seal signature check failed: ${e.message}`);
      }
    }
  } else {
    r.warnings.push('seal unsigned — chain integrity checked, attribution not possible');
  }
  return r;
}

function checkFile(loaded, pub) {
  const perEntry = loaded.entries.map((e, i) => ({
    entry: e,
    problems: entryProblems(e, i, i > 0 ? loaded.entries[i - 1] : null),
  }));
  const seal = sealCheck(loaded.seal, loaded.entries, pub, loaded.afterSeal.length > 0);
  const head = loaded.entries.length ? loaded.entries[loaded.entries.length - 1].hash : null;
  const firstPrev = loaded.entries.length ? loaded.entries[0].prev : null;
  return {
    file: loaded.file,
    perEntry,
    head,
    firstPrev,
    seal,
    malformed: loaded.malformed,
    ok: perEntry.every((x) => x.problems.length === 0) && seal.problems.length === 0 && loaded.malformed.length === 0,
  };
}

/* ------------------------------- chain stitching ---------------------------- */

/* Order files into segments by the same prev→head rule `verify` uses. Each
 * file is one segment (a stage's ledger); a file whose first entry prev
 * equals another file's head continues that chain. */
function stitch(results) {
  const byHead = new Map(results.filter((r) => r.head).map((r) => [r.head, r]));
  for (const r of results) {
    if (r.firstPrev === GENESIS || r.firstPrev === null || r.firstPrev === undefined) {
      r.link = 'genesis';
    } else {
      const parent = byHead.get(r.firstPrev);
      r.link = parent && parent.file !== r.file ? parent : 'unresolved';
    }
  }
  // Roots (genesis or unresolved first-prev) first, ordered by earliest
  // entry timestamp; each root's head→file chain is followed to the tip.
  const ordered = [];
  const seen = new Set();
  const follow = (r) => {
    let cur = r;
    while (cur && !seen.has(cur.file)) {
      ordered.push(cur);
      seen.add(cur.file);
      cur = byHead.get(cur.head);
    }
  };
  const roots = results
    .filter((r) => !(r.link instanceof Object))
    .sort((a, b) => earliestTs(a) - earliestTs(b));
  for (const r of roots) follow(r);
  for (const r of results) if (!seen.has(r.file)) follow(r); // cycles/strays
  return ordered;
}

function earliestTs(r) {
  const t = r.perEntry.length ? Date.parse(r.perEntry[0].entry.ts) : NaN;
  return Number.isFinite(t) ? t : Infinity;
}

/* ------------------------------ event normalization ------------------------- */

/* Ledger entry -> normalized audit event (Hermes normalizeDecision/Run port). */
function toEvent(fileResult, pe) {
  const e = pe.entry;
  const run = e.run && typeof e.run === 'object' ? e.run : {};
  return {
    ts: e.ts || null,
    stage: e.stage || 'unknown',
    seq: e.seq ?? null,
    repo: e.repo || null,
    issue: e.issue ?? null,
    pr: e.pr ?? null,
    runId: run.id ? String(run.id) : null,
    runUrl: run.url || null,
    workflow: run.workflow || null,
    job: run.job || null,
    event: run.event || null,
    actor: run.actor || null,
    sha: run.sha || null,
    data: e.data && typeof e.data === 'object' ? e.data : {},
    hash: e.hash || null,
    prev: e.prev || null,
    file: fileResult.file,
    integrity: { ok: pe.problems.length === 0, problems: pe.problems },
  };
}

function matchesSelector(ev, sel) {
  if (!sel) return true;
  if (sel.issue !== undefined && ev.issue === sel.issue) return true;
  if (sel.pr !== undefined && ev.pr === sel.pr) return true;
  if (sel.run !== undefined && ev.runId === sel.run) return true;
  if (sel._issues && ev.issue !== null && sel._issues.has(ev.issue)) return true;
  if (sel._prs && ev.pr !== null && sel._prs.has(ev.pr)) return true;
  return false;
}

/* Selectors expand through issue↔pr linkage: an entry recording both
 * (implement stamps issue+pr) ties the PR-scoped stages back to the issue
 * that dispatched them, so `--issue 7` emits the whole causal chain —
 * intake → implement → review/automerge — not just the issue-stamped
 * stages. Expansion runs to fixpoint over the loaded set. */
function expandSelector(results, sel) {
  if (!sel || (sel.issue === undefined && sel.pr === undefined)) return sel;
  const issues = new Set(sel.issue !== undefined ? [sel.issue] : []);
  const prs = new Set(sel.pr !== undefined ? [sel.pr] : []);
  let grew = true;
  while (grew) {
    grew = false;
    for (const r of results) {
      for (const pe of r.perEntry) {
        const e = pe.entry;
        if (e && Number.isInteger(e.issue) && Number.isInteger(e.pr)) {
          if (issues.has(e.issue) && !prs.has(e.pr)) { prs.add(e.pr); grew = true; }
          if (prs.has(e.pr) && !issues.has(e.issue)) { issues.add(e.issue); grew = true; }
        }
      }
    }
  }
  return { ...sel, _issues: issues, _prs: prs };
}

function collectEvents(results, sel) {
  const resolved = expandSelector(results, sel);
  const events = [];
  for (const r of stitch(results)) {
    for (const pe of r.perEntry) {
      const ev = toEvent(r, pe);
      if (matchesSelector(ev, resolved)) {
        ev.link = r.link === 'genesis' ? 'genesis'
          : r.link === 'unresolved' ? 'unresolved'
          : `chains from ${path.basename(r.link.file)}`;
        events.push(ev);
      }
    }
  }
  events.sort((a, b) => (Date.parse(a.ts) || Infinity) - (Date.parse(b.ts) || Infinity));
  return events;
}

/* --------------------------- decision extraction ---------------------------- */

/* Stage-aware decision read. The ledger records step outputs, so each stage
 * has its own shape — the extractor maps every known shape onto
 * {decision, reason, labelsApplied, engine, artifacts} and falls back to a
 * generic scan for unknown stages. */
function deriveDecision(ev) {
  const d = ev.data || {};
  const out = { stage: ev.stage, decision: 'recorded', reason: '', labelsApplied: [], engine: null, artifacts: [] };

  if (Array.isArray(d.files)) {
    out.artifacts = d.files
      .filter((f) => f && typeof f === 'object')
      .map((f) => ({ path: f.path, bytes: f.bytes ?? null, sha256: f.sha256 || null }));
  }

  switch (ev.stage) {
    case 'intake': {
      if (d.armed === 'false') {
        out.decision = 'not-armed';
        out.reason = 'trigger label absent — intake skipped the issue';
      } else if (d.dispatch === 'true') {
        out.decision = 'dispatched';
        out.reason = 'Definition-of-Ready passed';
        out.labelsApplied.push({ label: LABEL_QUEUED, why: 'DoR passed — queued for implementation (canonical labels.queued default)' });
      } else if (d.dispatch === 'false') {
        out.decision = 'dor-failed';
        out.reason = `Definition-of-Ready failed: ${(d.failed || 'unspecified').split('|').join(', ')}`;
        out.labelsApplied.push({ label: LABEL_NOT_READY, why: `DoR failed (${d.failed || 'unspecified'}) — canonical labels.not-ready default` });
      }
      break;
    }
    case 'implement': {
      const code = Number(d.exit_code);
      out.engine = d.model ? { model: d.model, exit_code: code } : null;
      if (d.fallback_used === 'true' || d.fallback_used === true) {
        out.engine = out.engine || {};
        out.engine.fallback = { used: true, model: d.fallback_model || null, reason: d.fallback_reason || null, exit: d.fallback_exit ?? null };
      }
      if (code === 0) { out.decision = 'implemented'; out.reason = 'engine run succeeded — PR opened'; }
      else if (code === 10) { out.decision = 'no-changes'; out.reason = 'engine reported no changes (exit 10)'; }
      else if (Number.isFinite(code)) {
        out.decision = 'failed';
        out.reason = `engine exit ${code} (${ENGINE_EXIT[code] || 'unknown'})`;
      }
      break;
    }
    case 'review':
    case 'review-fix': {
      const verdict = str(d.verdict);
      out.decision = verdict ? `verdict-${verdict}` : str(d.decision) || 'reviewed';
      out.reason = str(d.reason) || str(d.guard_reason) || '';
      if (d.risk_tier) out.riskTier = str(d.risk_tier);
      break;
    }
    case 'automerge': {
      const action = str(d.action) || 'unknown';
      out.decision = action;
      out.reason = str(d.reason) || '';
      if (action === 'escalated') {
        out.labelsApplied.push({ label: LABEL_ESCALATION, why: `escalated (${out.reason || 'no reason recorded'}) — canonical escalation.label default` });
      }
      break;
    }
    case 'ci-repair': {
      const res = d.result && typeof d.result === 'object' ? d.result : d;
      out.decision = str(res.action) || str(res.outcome) || 'repair-attempted';
      out.reason = str(res.reason) || str(res.error) || '';
      const attempt = d.guard && typeof d.guard === 'object' ? d.guard.attempt : undefined;
      if (attempt !== undefined) out.attempt = attempt;
      break;
    }
    case 'verify': {
      const failed = str(d.failed);
      out.decision = (failed === 'false' || failed === '0' || failed === '') && d.failed !== undefined
        ? 'verify-passed' : (d.failed === undefined ? 'recorded' : 'verify-failed');
      out.reason = failed && failed !== 'false' && failed !== '0' ? `failed checks: ${failed}` : '';
      break;
    }
    default: {
      // Generic scan — unknown/future stages still yield an answer.
      const action = str(d.action) || str(d.verdict) || str(d.decision) || str(d.outcome);
      if (action) out.decision = action;
      out.reason = str(d.reason) || str(d.guard_reason) || str(d.failed) || '';
    }
  }
  return out;
}

const str = (v) => (v === null || v === undefined ? '' : String(v));

/* --------------------------------- why engine ------------------------------- */

const WHY_MATCH = {
  escalated: (ev, dec) => dec.decision === 'escalated' || dec.labelsApplied.some((l) => l.label === LABEL_ESCALATION),
  skipped: (ev, dec) => ['skipped', 'not-armed', 'dor-failed'].includes(dec.decision),
  labeled: (ev, dec) => dec.labelsApplied.length > 0,
  merged: (ev, dec) => dec.decision === 'merged',
  held: (ev, dec) => dec.decision === 'held',
  failed: (ev, dec) => ['failed', 'verify-failed', 'dor-failed'].includes(dec.decision),
  dispatched: (ev, dec) => dec.decision === 'dispatched' || dec.decision === 'implemented',
};

function answerWhy(kind, events) {
  const match = WHY_MATCH[kind];
  const hits = [];
  for (const ev of events) {
    const dec = deriveDecision(ev);
    if (match(ev, dec)) hits.push({ ev, dec });
  }
  const answer = { query: `why-${kind}`, matched: hits.length > 0, summary: '', evidence: [] };
  if (!hits.length) {
    answer.summary = `no ledger entries explain "${kind}" for this target — the decision may predate the ledger or the bundle is incomplete`;
    return answer;
  }
  for (const { ev, dec } of hits) {
    const cite = {
      file: path.basename(ev.file), seq: ev.seq, stage: ev.stage, ts: ev.ts,
      hash: ev.hash, integrity: ev.integrity.ok ? 'ok' : `FLAGGED: ${ev.integrity.problems.join('; ')}`,
    };
    if (kind === 'labeled') {
      for (const l of dec.labelsApplied) {
        answer.evidence.push({ ...cite, label: l.label, why: l.why });
      }
    } else {
      answer.evidence.push({ ...cite, decision: dec.decision, reason: dec.reason || undefined });
    }
  }
  const last = hits[hits.length - 1];
  if (kind === 'labeled') {
    answer.summary = `${hits.length} stage(s) applied labels — see evidence`;
  } else {
    answer.summary = `${last.ev.stage}: ${last.dec.decision}${last.dec.reason ? ` — ${last.dec.reason}` : ''}`;
  }
  return answer;
}

/* ----------------------------------- output --------------------------------- */

function fmtEvent(ev) {
  const dec = deriveDecision(ev);
  const flag = ev.integrity.ok ? 'ok' : 'TAMPER';
  const subj = `${ev.repo || '?'}${ev.issue ? ` issue=#${ev.issue}` : ''}${ev.pr ? ` pr=#${ev.pr}` : ''}`;
  const lines = [];
  const runBits = [ev.runId && `run=${ev.runId}`, ev.actor && `actor=${ev.actor}`].filter(Boolean).join(' ');
  lines.push(`  [${ev.ts}] ${ev.stage} seq=${ev.seq} hash=${(ev.hash || '—').slice(0, 16)}… ${subj} ${runBits} integrity=${flag}`);
  if (dec.decision !== 'recorded' || dec.reason) {
    lines.push(`      decision: ${dec.decision}${dec.reason ? ` — ${dec.reason}` : ''}`);
  }
  for (const l of dec.labelsApplied) lines.push(`      label: +${l.label} (${l.why})`);
  if (dec.engine) {
    const f = dec.engine.fallback && dec.engine.fallback.used ? ` fallback=${dec.engine.fallback.model}(${dec.engine.fallback.reason})` : '';
    lines.push(`      engine: model=${dec.engine.model || '?'} exit=${dec.engine.exit_code ?? '?'}${f}`);
  }
  for (const a of dec.artifacts) lines.push(`      artifact: ${a.path} sha256=${(a.sha256 || '—').slice(0, 16)}…`);
  if (ev.link === 'unresolved') lines.push('      warning: first entry prev resolves to no head in this bundle — prior-stage ledger absent');
  for (const p of ev.integrity.problems) lines.push(`      ⚠ TAMPER: ${p}`);
  return lines.join('\n');
}

function integritySummary(results) {
  const files = results.map((r) => ({
    file: r.file,
    entries: r.perEntry.length,
    head: r.head,
    link: r.link === 'genesis' ? 'genesis' : r.link === 'unresolved' ? 'unresolved' : `chains from ${path.basename(r.link.file)}`,
    sealed: r.seal.sealed,
    signed: r.seal.signed,
    signature: r.seal.signature,
    malformedLines: r.malformed,
    entryProblems: r.perEntry.filter((p) => p.problems.length).map((p) => ({ seq: p.entry && p.entry.seq, problems: p.problems })),
    sealProblems: r.seal.problems,
    warnings: r.seal.warnings,
    ok: r.ok,
  }));
  const flagged = files.reduce((n, f) => n + f.entryProblems.length + f.malformedLines.length + (f.sealProblems.length ? 1 : 0), 0);
  return { files, flagged };
}

/* ------------------------------------- CLI ---------------------------------- */

function parseArgs(argv) {
  const opts = { ledgers: [], dirs: [], paths: [] };
  const MULTI = new Set(['ledger', 'dir']);
  const out = { opts, positional: [] };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a.startsWith('--')) {
      const k = a.slice(2);
      if (i + 1 < argv.length && !argv[i + 1].startsWith('--')) {
        if (MULTI.has(k)) opts[k + 's'].push(argv[++i]);
        else opts[k] = argv[++i];
      } else opts[k] = true;
    } else out.positional.push(a);
  }
  return out;
}

function gatherFiles(opts) {
  const files = [...opts.ledgers];
  const visit = (p) => {
    const st = fs.statSync(p);
    if (st.isDirectory()) {
      for (const name of fs.readdirSync(p).sort()) {
        if (name.endsWith('.jsonl') || name.endsWith('.json')) visit(path.join(p, name));
      }
    } else files.push(p);
  };
  for (const p of [...opts.dirs, ...opts.paths]) {
    if (!fs.existsSync(p)) die(`path not found: ${p}`);
    visit(p);
  }
  const uniq = [...new Set(files)];
  if (uniq.length > MAX_FILES) die(`too many ledger files (${uniq.length} > ${MAX_FILES})`);
  return uniq;
}

function selectorFrom(opts) {
  const sel = {};
  if (opts.issue !== undefined) { const n = Number(opts.issue); if (!Number.isInteger(n)) die(`bad --issue: ${opts.issue}`); sel.issue = n; }
  if (opts.pr !== undefined) { const n = Number(opts.pr); if (!Number.isInteger(n)) die(`bad --pr: ${opts.pr}`); sel.pr = n; }
  if (opts.run !== undefined) sel.run = String(opts.run);
  return Object.keys(sel).length ? sel : null;
}

function selectorLabel(sel) {
  if (!sel) return 'all entries';
  return [sel.issue !== undefined && `issue #${sel.issue}`, sel.pr !== undefined && `pr #${sel.pr}`, sel.run !== undefined && `run ${sel.run}`]
    .filter(Boolean).join(' / ');
}

function loadKey(opts, kind) { // kind: 'pubkey'
  const pem = opts[`${kind}-env`] ? process.env[opts[`${kind}-env`]] : null
    || (opts[`${kind}-file`] ? fs.readFileSync(opts[`${kind}-file`], 'utf8') : null);
  if (!pem || !pem.trim()) return null;
  try { return crypto.createPublicKey(pem); } catch (e) { die(`invalid public key: ${e.message}`); }
  return null;
}

function main() {
  const [opRaw, ...rest] = process.argv.slice(2);
  const { opts, positional } = parseArgs(rest);
  let op = opRaw;
  let whyKind = null;
  if (op === 'why') {
    whyKind = positional.shift();
    if (!WHY_KINDS.includes(whyKind)) die(`why requires a kind: ${WHY_KINDS.join('|')}`);
  }
  if (!op || op === 'help' || op === '--help' || opts.help) {
    console.log(`usage: audit-query.js <trace|why KIND|labels> [ledger paths] [flags]
  trace              stage-by-stage decision trail (default)
  why <kind>         structured answer: ${WHY_KINDS.join(' | ')}
  labels             label applications only
  inputs             --ledger F (repeatable) · --dir D (repeatable) · positional file/dir paths
  selectors          --issue N · --pr N · --run ID (OR-matched; absent = all entries)
  integrity          --pubkey-file F | --pubkey-env V · --strict (exit 1 on any integrity problem)
  output             --json`);
    process.exit(op ? 0 : 2);
  }
  if (!['trace', 'why', 'labels'].includes(op)) die(`unknown op: ${op}`);

  opts.paths = positional;
  const files = gatherFiles(opts);
  if (!files.length) die('no ledger files found — pass --ledger/--dir or a path');

  const pub = loadKey(opts, 'pubkey');
  const results = files.map((f) => checkFile(loadLedgerFile(f), pub));
  const sel = selectorFrom(opts);
  const events = collectEvents(results, sel);
  const integrity = integritySummary(results);
  const integrityFail = integrity.files.some((f) => !f.ok);

  if (opts.json) {
    const body = { target: selectorLabel(sel), files: integrity.files, events, flagged: integrity.flagged };
    if (op === 'why') body.answer = answerWhy(whyKind, events);
    if (op === 'labels') body.labels = answerWhy('labeled', events).evidence;
    console.log(JSON.stringify(body, null, 2));
  } else {
    for (const f of integrity.files) {
      const flags = [`${f.entries} entries`, f.sealed ? (f.signed ? `signed(${f.signature})` : 'unsigned seal') : 'UNSEALED', `link=${f.link}`];
      console.log(`${f.ok ? '✓' : '✗'} ${f.file} — ${flags.join(' · ')}`);
      for (const w of f.warnings) console.log(`    warning: ${w}`);
      for (const m of f.malformedLines) console.log(`    ⚠ malformed line ${m.line ?? '?'}: ${m.error}`);
      for (const ep of f.entryProblems) for (const p of ep.problems) console.log(`    ⚠ TAMPER: ${p}`);
      for (const p of f.sealProblems) console.log(`    ⚠ TAMPER: ${p}`);
    }
    if (op === 'why') {
      const answer = answerWhy(whyKind, events);
      console.log(`\nwhy ${whyKind} — ${selectorLabel(sel)}`);
      console.log(`→ ${answer.summary}`);
      for (const e of answer.evidence) {
        console.log(`  evidence: ${e.file} seq=${e.seq} stage=${e.stage} hash=${(e.hash || '—').slice(0, 16)}… [integrity: ${e.integrity}]`);
        if (e.label) console.log(`    label: +${e.label} — ${e.why}`);
        else if (e.reason || e.decision) console.log(`    ${e.decision || ''}${e.reason ? ` — ${e.reason}` : ''}`);
      }
    } else if (op === 'labels') {
      const answer = answerWhy('labeled', events);
      console.log(`\nlabels applied — ${selectorLabel(sel)}`);
      if (!answer.evidence.length) console.log('  (no label applications in the matched entries)');
      for (const e of answer.evidence) {
        console.log(`  +${e.label} — ${e.why}\n    evidence: ${e.file} seq=${e.seq} hash=${(e.hash || '—').slice(0, 16)}… [integrity: ${e.integrity}]`);
      }
    } else {
      console.log(`\n=== audit trail — ${selectorLabel(sel)} (${events.length} entries) ===`);
      if (!events.length) console.log('  (no ledger entries matched — the run may predate the ledger or be outside this bundle)');
      for (const ev of events) console.log(fmtEvent(ev));
    }
  }

  if (opts.strict && integrityFail) {
    console.error('audit-query: integrity problems detected (--strict)');
    process.exit(1);
  }
  process.exit(0);
}

if (require.main === module) main();

module.exports = {
  loadLedgerFile, checkFile, stitch, collectEvents, deriveDecision, answerWhy,
  entryProblems, sealCheck, toEvent, WHY_KINDS,
};
