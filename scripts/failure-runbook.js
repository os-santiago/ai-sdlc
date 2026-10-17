#!/usr/bin/env node
// scripts/failure-runbook.js — known-signature → runbook fix injection (issue #188)
//
// Ports Hermes' runbook consult (scripts/failure_runbook.js — lookupSignature /
// matchEntries / bounded advisory block) into the GHA-native runtime. The
// ci-repair stage computes a failure signature per attempt — sha256[:16] over
// the sorted, normalized "<check>:<first-error-line>" parts — and consults the
// failure-mode runbook BEFORE the engine runs. A hit prepends a bounded
// "known failure mode" advisory (prior root cause + fix + prevention) to the
// repair brief so the agent applies the recorded remediation instead of
// rediscovering it; a miss leaves the brief byte-identical (no noise).
//
// Lockstep: the signature formula MUST match the prompt step of
// .github/workflows/ai-sdlc-ci-repair.yml — normalizeLogLine() + the sorted-
// parts sha256 below are the same transform, single-sourced here for tests.
//
// Store: docs/runbooks/failure_modes.md — one `### `<id>` — <title>` section
// per entry, marked <!-- ai-sdlc:runbook-entry -->, with `- **Key:**` bullets:
//   Signatures: `deadbeef12345678`, `cafe99`   exact or >=6-hex prefix of the
//                                            ci-repair failure signature
//   Match:      `literal`, `/regex/`           tested against the normalized
//                                            signature text + root cause;
//                                            literals are case-insensitive
//                                            substrings, /regex/ compiles `im`
//   Fix:        REQUIRED — entries without a fix are never injected
//   Root cause / Prevention / Resolved by: optional, clipped advisory fields
//
// Trust boundary: the store is DATA, fetched by the workflow from the consumer
// repo's BASE ref (merged, reviewed content — a PR head cannot forge its own
// advisory) with the runtime repo's bundled copy as fallback. The advisory is
// trusted text and lands OUTSIDE the untrusted-input fence
// (spec/injection-defense.md). This script itself is trusted code — fetched
// from the runtime repo pin, never the PR head (same boundary as
// risk-classifier.js, issue #204).
//
// Bounded: the workflow caps the fetched store; per-field clips plus a total
// --max-bytes cap (default 1400, Hermes parity) bound the injected excerpt;
// matching is a linear scan over the parsed entry list — no engine, no model.
//
// CLI:
//   node scripts/failure-runbook.js --runbook docs/runbooks/failure_modes.md \
//     --signature <16hex> [--context failure.json | --text STR] \
//     [--prompt-file repair-prompt.md [--out FILE]] [--max-bytes N] \
//     [--json] [--github-output] [--list]
'use strict';

const fs = require('fs');
const path = require('path');
const crypto = require('crypto');

const DEFAULT_MAX_BYTES = 1400;
const DEFAULT_RUNBOOK = path.join(__dirname, '..', 'docs', 'runbooks', 'failure_modes.md');
const ENTRY_MARKER = '<!-- ai-sdlc:runbook-entry -->';
const FIELD_RE = /^\s*[-*]\s*\*\*([^:*]+):\*\*\s*(.+?)\s*$/;
const TOKEN_RE = /`([^`]+)`/g;
const HEX_RE = /^[0-9a-f]{6,64}$/i;

// --- Signature helpers (lockstep with ai-sdlc-ci-repair.yml prompt step) -----

// Same normalization the workflow applies to each failing check's first error
// line before hashing: `##[error]` stripped, hex blobs -> H, digits -> N.
function normalizeLogLine(line) {
  return String(line || '')
    .replace(/##\[error\]/g, '')
    .replace(/\b[0-9a-f]{7,40}\b/g, 'H')
    .replace(/\d+/g, 'N')
    .trim()
    .slice(0, 200);
}

// parts: array of raw "name:line" strings or {name, line} objects.
// Returns the 16-hex signature the workflow persists in failure.json.
function failureSignature(parts) {
  const norm = (Array.isArray(parts) ? parts : []).map((p) =>
    (typeof p === 'string' ? p : `${p.name}:${normalizeLogLine(p.line)}`));
  return crypto.createHash('sha256').update([...norm].sort().join('\n')).digest('hex').slice(0, 16);
}

// --- Store parsing -----------------------------------------------------------

// One `### `<id>` — <title>` section per entry; only sections carrying the
// entry marker are considered (arbitrary doc headings are never entries).
function parseRunbook(text) {
  const sections = [];
  let cur = null;
  for (const line of String(text || '').split('\n')) {
    const h = line.match(/^(#{2,6})\s+(.+?)\s*$/);
    if (h) {
      cur = { title: h[2], body: [] };
      sections.push(cur);
    } else if (cur) {
      cur.body.push(line);
    }
  }
  const entries = [];
  for (const s of sections) {
    const body = s.body.join('\n');
    if (!body.includes(ENTRY_MARKER)) continue;
    const hm = s.title.match(/^`([^`]+)`\s*[—–-]\s*(.*)$/);
    const fields = {};
    for (const bl of s.body) {
      const f = bl.match(FIELD_RE);
      if (f) fields[f[1].trim().toLowerCase()] = f[2].trim();
    }
    const tokens = (v) =>
      [...String(v || '').matchAll(TOKEN_RE)].map((m) => m[1].trim()).filter(Boolean);
    const match = tokens(fields.match || fields.matches || fields.patterns)
      .map((t) => {
        const rx = t.match(/^\/(.+)\/[a-z]*$/);
        if (rx) {
          try {
            return { token: t, regex: new RegExp(rx[1], 'im') };
          } catch (e) {
            // Invalid regex degrades to a literal substring — bad store data
            // must never crash the consult.
          }
        }
        return { token: t, literal: t };
      });
    entries.push({
      id: (hm ? hm[1] : s.title).trim(),
      title: (hm && hm[2] ? hm[2] : s.title).trim(),
      signatures: tokens(fields.signatures || fields.signature)
        .map((s2) => s2.toLowerCase()).filter((s2) => HEX_RE.test(s2)),
      match,
      rootCause: fields['root cause'] || '',
      fix: fields.fix || '',
      prevention: fields.prevention || fields['prevention rule'] || '',
      resolvedBy: fields['resolved by'] || fields['closed by'] || '',
    });
  }
  return { entries };
}

// --- Lookup ------------------------------------------------------------------
// Port of Hermes' lookupSignature/matchEntries: hash hits (exact or >=6-hex
// prefix, either direction) beat pattern hits; first entry in document order
// wins within a pass. Entries without a fix are unreachable — an advisory
// without remediation is noise, not a known fix.
function matchEntry({ signature, text } = {}, entries) {
  const sig = String(signature || '').trim().toLowerCase();
  const usable = (Array.isArray(entries) ? entries : []).filter((e) => e && e.fix);
  if (sig) {
    for (const e of usable) {
      for (const h of e.signatures || []) {
        if (sig === h || (h.length >= 6 && sig.startsWith(h)) || (sig.length >= 6 && h.startsWith(sig))) {
          return { entry: e, via: 'signature' };
        }
      }
    }
  }
  const hay = String(text || '');
  const low = hay.toLowerCase();
  if (hay.trim()) {
    for (const e of usable) {
      for (const t of e.match || []) {
        if (t.regex && t.regex.test(hay)) return { entry: e, via: 'match' };
        if (t.literal && low.includes(t.literal.toLowerCase())) return { entry: e, via: 'match' };
      }
    }
  }
  return null;
}

// --- Advisory rendering ------------------------------------------------------

// Word-boundary clip (ported from Hermes' clip()).
function clip(text, max = 300) {
  const t = String(text || '').replace(/\s+/g, ' ').trim();
  if (t.length <= max) return t;
  const words = t.slice(0, max).split(' ');
  if (words.length > 1) words.pop();
  const out = words.join(' ').replace(/[,;:\-—|]+$/, '').trim();
  return `${out || t.slice(0, max).trim()}…`;
}

// Bounded markdown block injected at the top of the repair brief — trusted
// content, OUTSIDE the untrusted-input fence. `maxBytes <= 0` disables
// injection (advisory renders empty). Hard byte cap: the block is cut at a
// line boundary and marked when oversized.
function buildAdvisory(entry, { maxBytes } = {}) {
  const cap = maxBytes === undefined ? DEFAULT_MAX_BYTES : Number(maxBytes);
  if (!entry || !entry.fix || !(cap > 0)) return '';
  const lines = [
    `## Runbook hit — known failure mode \`${entry.id}\``,
    `The failure signature of this run matches a previously resolved failure mode: ${clip(entry.title, 160)}.`,
    `- Prior root cause: ${clip(entry.rootCause, 280) || 'n/a'}`,
    `- Prior fix: ${clip(entry.fix, 480)}`,
  ];
  if (entry.prevention) lines.push(`- Prevention rule: ${clip(entry.prevention, 280)}`);
  if (entry.resolvedBy) lines.push(`- Resolved by: ${clip(entry.resolvedBy, 160)}`);
  lines.push('Apply the prior fix first; if the failure has drifted from this signature, treat the above as context only and repair normally.');
  let block = lines.join('\n');
  if (Buffer.byteLength(block) > cap) {
    const marker = '\n_(runbook excerpt truncated)_';
    const budget = Math.max(0, cap - Buffer.byteLength(marker));
    // Byte-exact cut, then drop a trailing partial line / split UTF-8 char.
    block = Buffer.from(block, 'utf8').subarray(0, budget).toString('utf8').replace(/\uFFFD+$/, '');
    const nl = block.lastIndexOf('\n');
    if (nl > 0) block = block.slice(0, nl);
    block += marker;
  }
  return block;
}

// Hit → advisory block on top of the brief; miss/empty advisory → brief
// returned unchanged (byte-identical — the no-noise contract).
function injectAdvisory(brief, advisory) {
  const b = String(brief || '');
  const a = String(advisory || '').trim();
  return a ? `${a}\n\n${b}` : b;
}

// Match text from a ci-repair failure.json ({signature, signatureParts,
// rootCause, checks}) or a raw log/text blob.
function contextText(input) {
  if (input && typeof input === 'object') {
    return [
      input.signature,
      ...(Array.isArray(input.signatureParts) ? input.signatureParts : []),
      input.rootCause,
      ...(Array.isArray(input.checks) ? input.checks : []),
    ].filter(Boolean).join('\n');
  }
  return String(input || '');
}

// --- CLI ---------------------------------------------------------------------

function usage() {
  return [
    'usage: failure-runbook.js [--runbook FILE] [--signature HEX]',
    '                          [--context FILE | --text STR]',
    '                          [--prompt-file FILE [--out FILE]] [--max-bytes N]',
    '                          [--list] [--json] [--github-output]',
  ].join('\n');
}

function parseArgs(argv) {
  const opts = {
    runbook: DEFAULT_RUNBOOK,
    maxBytes: DEFAULT_MAX_BYTES,
    json: false,
    githubOutput: false,
    list: false,
  };
  for (let i = 0; i < argv.length; i += 1) {
    const a = argv[i];
    const next = () => {
      i += 1;
      if (i >= argv.length) throw new Error(`missing value for ${a}`);
      return argv[i];
    };
    if (a === '--runbook') opts.runbook = next();
    else if (a === '--signature') opts.signature = next();
    else if (a === '--context') opts.context = next();
    else if (a === '--text') opts.text = next();
    else if (a === '--prompt-file') opts.promptFile = next();
    else if (a === '--out') opts.out = next();
    else if (a === '--max-bytes') opts.maxBytes = Number(next());
    else if (a === '--list') opts.list = true;
    else if (a === '--json') opts.json = true;
    else if (a === '--github-output') opts.githubOutput = true;
    else if (a === '--help' || a === '-h') { console.log(usage()); process.exit(0); }
    else throw new Error(`unknown argument: ${a}`);
  }
  return opts;
}

function main(argv, { stdout } = {}) {
  const opts = parseArgs(argv);
  const emit = stdout || ((s) => console.log(s));

  // Missing/unreadable store → graceful miss, never a failed consult: the
  // repair loop must continue without injection (fail-open on DATA, the
  // script fetch itself is the trusted-code boundary).
  let runbook = { entries: [] };
  let runbookError = null;
  try {
    runbook = parseRunbook(fs.readFileSync(opts.runbook, 'utf8'));
  } catch (e) {
    runbookError = e.message;
  }

  if (opts.list) {
    const list = runbook.entries.map((e) => ({ id: e.id, title: e.title, signatures: e.signatures.length, patterns: e.match.length }));
    emit(JSON.stringify({ entries: list, runbookError }, null, 2));
    return { entries: list };
  }

  let signature = String(opts.signature || '').trim();
  let text = String(opts.text || '');
  if (opts.context) {
    const raw = fs.readFileSync(opts.context, 'utf8');
    let obj = null;
    try { obj = JSON.parse(raw); } catch (e) { /* raw text context */ }
    if (obj && typeof obj === 'object') {
      signature = signature || String(obj.signature || '');
      text = [text, contextText(obj)].filter(Boolean).join('\n');
    } else {
      text = [text, raw].filter(Boolean).join('\n');
    }
  }

  const hit = matchEntry({ signature, text: `${signature}\n${text}` }, runbook.entries);
  const advisory = hit ? buildAdvisory(hit.entry, { maxBytes: opts.maxBytes }) : '';
  const result = {
    hit: Boolean(hit && advisory),
    matched: Boolean(hit),
    entry: hit ? hit.entry.id : null,
    via: hit ? hit.via : null,
    injected: false,
    advisoryBytes: Buffer.byteLength(advisory),
    runbookError,
  };

  if (opts.promptFile) {
    const brief = fs.readFileSync(opts.promptFile, 'utf8');
    const injected = injectAdvisory(brief, advisory);
    const outPath = opts.out || opts.promptFile;
    fs.writeFileSync(outPath, injected);
    result.promptFile = outPath;
    result.promptBytes = Buffer.byteLength(injected);
    result.injected = injected !== brief;
  }

  if (opts.githubOutput && process.env.GITHUB_OUTPUT) {
    fs.appendFileSync(process.env.GITHUB_OUTPUT,
      `runbook_hit=${result.hit}\nrunbook_entry=${result.entry || ''}\n`);
  }

  if (opts.json) emit(JSON.stringify(result));
  else if (!opts.promptFile) emit(advisory);
  return result;
}

if (require.main === module) {
  try {
    main(process.argv.slice(2));
  } catch (e) {
    console.error(`failure-runbook: ${e.message}`);
    process.exit(2);
  }
}

module.exports = {
  DEFAULT_MAX_BYTES,
  DEFAULT_RUNBOOK,
  ENTRY_MARKER,
  normalizeLogLine,
  failureSignature,
  parseRunbook,
  matchEntry,
  clip,
  buildAdvisory,
  injectAdvisory,
  contextText,
  parseArgs,
  main,
};
