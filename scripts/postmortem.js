#!/usr/bin/env node
// scripts/postmortem.js — canonical postmortem lifecycle (issue #191)
//
// Implements spec/postmortem-lifecycle.md for the GHA-native runtime
// (epic #146): every stage failure produces a dedupable failure signature;
// the canonical postmortem issue carries a stable signature marker, and a
// new occurrence of the same signature NEVER files a duplicate issue —
//
//   - signature matches an OPEN issue      → recurrence comment only
//   - signature matches a CLOSED issue inside the dedup window
//       post-close recurrences <  reopen_min → recurrence comment only
//       post-close recurrences >= reopen_min → comment + REOPEN the canonical
//   - no match (or closed match older than the dedup window) → file a new
//     canonical postmortem carrying the signature marker
//
// Recurrence state lives on the issue itself — every recurrence comment
// carries a hidden marker (`ai-sdlc-postmortem-recurrence sig=<hash>
// occ=<id>`), so post-close counts and occurrence-id dedup survive across
// stateless workflow runs with no ledger file.
//
// Ported from the Hermes control plane (scripts/postmortem_analyzer.js,
// issues #375/#448): same canonical-marker dedup, same comment-first-
// then-reopen ordering so evidence lands even when the reopen call fails.
//
// Trust boundary: fetch and run this script only from a trusted runtime
// ref (see ai-sdlc-verify.yml) — never from a PR head or consumer checkout.
//
// CLI:
//   node scripts/postmortem.js --repo OWNER/NAME \
//     --component verify --class post-merge-verify-failure --location "npm test" \
//     --title "verify: post-merge checks failed on <sha>" --body "..." \
//     [--occurrence-id verify-<sha>] [--run-url URL] \
//     [--reopen-min 2] [--dedup-window-days 90] [--labels a,b,c] \
//     [--context "extra evidence"] [--dry-run] [--json] [--github-output]
//
//   --signature "raw text"    hash this string instead of the part triple
//   --signature-hash HEX      use a precomputed hash directly
'use strict';

const fs = require('fs');
const crypto = require('crypto');
const { execFileSync } = require('child_process');

const POSTMORTEM_LABEL = 'postmortem';
const DEFAULT_LABELS = 'ai-sdlc,postmortem,priority:P1';
const DEFAULT_REOPEN_MIN = 2;
const DEFAULT_DEDUP_WINDOW_DAYS = 90;
const MAX_ISSUES = 100;          // per-state scan bound (spec: bounded)
const MAX_COMMENT_PAGES = 3;     // ≤300 comments scanned per canonical
const COMMENTS_PER_PAGE = 100;

const SIG_MARKER_RE = /<!--\s*ai-sdlc-failure-signature:\s*([0-9a-f]{8,64})\s*-->/i;
const META_MARKER_RE = /<!--\s*ai-sdlc-postmortem-meta:\s*(\{[\s\S]*?\})\s*-->/;
const REC_MARKER_RE = /<!--\s*ai-sdlc-postmortem-recurrence\b[\s\S]*?-->/g;
const REC_SIG_RE = /\bsig=([0-9a-f]{8,64})\b/i;
const REC_OCC_RE = /\bocc=([^\s>]+)/;

// --- Signature normalization (spec/postmortem-lifecycle.md § Canonical
// Identity) --------------------------------------------------------------
// Volatile tokens (SHAs, UUIDs, ISO timestamps) collapse BEFORE the spec
// normalization so the same failure on a different commit still hashes to
// the canonical signature — the Hermes #448 lesson.

function collapseVolatile(text) {
  return String(text)
    .replace(/\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b/g, ' ')
    .replace(/\b\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z?\b/gi, ' ')
    .replace(/\b[0-9a-fA-F]{7,64}\b/g, ' ');
}

function normalizeSignatureText(text) {
  return collapseVolatile(text)
    .toLowerCase()
    .replace(/[^\w\s-]/g, ' ')   // punctuation out; hyphen/underscore stay (spec)
    .replace(/\s+/g, ' ')
    .trim();
}

/** signatureHash = sha256(normalize(component + class + location))[:16]. */
function computeSignatureHash(parts) {
  const raw = typeof parts === 'string'
    ? parts
    : `${parts.component || ''} ${parts.class || ''} ${parts.location || ''}`;
  const normalized = normalizeSignatureText(raw) || 'unknown';
  return crypto.createHash('sha256').update(normalized).digest('hex').slice(0, 16);
}

function signatureMarker(hash) {
  return `<!-- ai-sdlc-failure-signature: ${hash} -->`;
}

function recurrenceMarker(hash, occurrenceId) {
  return `<!-- ai-sdlc-postmortem-recurrence sig=${hash} occ=${occurrenceId || 'untracked'} -->`;
}

/** The signature hash stored on a postmortem issue, or null. */
function issueSignatureOf(issue) {
  const body = String((issue && issue.body) || '');
  const marker = body.match(SIG_MARKER_RE);
  if (marker) return marker[1].toLowerCase();
  const meta = body.match(META_MARKER_RE);
  if (meta) {
    try {
      const j = JSON.parse(meta[1]);
      const h = j && (j.sigHash || j.signatureHash);
      if (h) return String(h).toLowerCase();
    } catch (e) { /* malformed meta marker — keep looking */ }
  }
  const title = String((issue && issue.title) || '').match(/\[([0-9a-f]{8,64})\]/i);
  if (title) return title[1].toLowerCase();
  return null;
}

function matchesSignature(issue, hash) {
  const stored = issueSignatureOf(issue);
  if (stored === hash) return true;
  return String((issue && issue.body) || '').includes(hash);
}

/** Lowest-numbered matching issue is the canonical (spec: one per hash). */
function findCanonical(issues, hash) {
  return (issues || [])
    .filter(i => matchesSignature(i, hash))
    .sort((a, b) => a.number - b.number)[0] || null;
}

// --- Recurrence accounting -------------------------------------------------
// Each recurrence comment ends with a marker carrying sig + occurrence id;
// post-close recurrence count = marked comments newer than closedAt.

function parseRecurrenceMarkers(body) {
  const out = [];
  const text = String(body || '');
  let m;
  REC_MARKER_RE.lastIndex = 0;
  while ((m = REC_MARKER_RE.exec(text)) !== null) {
    const frag = m[0];
    const sig = frag.match(REC_SIG_RE);
    const occ = frag.match(REC_OCC_RE);
    out.push({ sig: sig ? sig[1].toLowerCase() : null, occ: occ ? occ[1] : null });
  }
  return out;
}

function commentRecurrences(comment, hash) {
  return parseRecurrenceMarkers(comment && comment.body)
    .filter(r => r.sig === hash);
}

/** True when a recurrence comment already recorded this occurrence id. */
function occurrenceRecorded(comments, hash, occurrenceId) {
  if (!occurrenceId) return false;
  return (comments || []).some(c =>
    commentRecurrences(c, hash).some(r => r.occ === occurrenceId));
}

/** Marked recurrence comments newer than the canonical's closedAt. */
function postCloseRecurrenceCount(comments, hash, closedAt) {
  const closedMs = closedAt ? Date.parse(closedAt) : null;
  let n = 0;
  for (const c of comments || []) {
    if (!commentRecurrences(c, hash).length) continue;
    if (closedMs === null) { n++; continue; }
    const at = Date.parse(c.created_at || c.createdAt || '');
    if (!Number.isFinite(at) || at > closedMs) n++;
  }
  return n;
}

/** Dedup window for a closed canonical (spec § Deduplication). */
function withinDedupWindow(issue, now, windowDays) {
  if (!issue.closedAt) return true;   // unknown → dedup conservatively
  const closedMs = Date.parse(issue.closedAt);
  if (!Number.isFinite(closedMs)) return true;
  return closedMs >= now.getTime() - windowDays * 86400000;
}

// --- Bodies -----------------------------------------------------------------

function filingBody(opts) {
  const meta = {
    signature: opts.signatureText || '',
    sigHash: opts.signatureHash,
    component: opts.component || '',
    class: opts.class || '',
    location: opts.location || '',
  };
  return `${opts.body || ''}

---

Filed by the AI-SDLC postmortem lifecycle (spec/postmortem-lifecycle.md). This is the canonical issue for the failure signature below — recurrences comment here (and reopen this issue past \`postmortem.reopen_min\`) instead of filing duplicates.

${signatureMarker(opts.signatureHash)}
<!-- ai-sdlc-postmortem-meta: ${JSON.stringify(meta)} -->`;
}

function recurrenceComment(opts, { postCloseCount, reopening }) {
  const lines = [
    `### 🔄 Postmortem recurrence — failure signature observed again`,
    '',
    reopening
      ? `The failure signature tracked by this issue recurred after it was closed and reached the reopen threshold — reopening the canonical issue instead of filing a duplicate (spec/postmortem-lifecycle.md).`
      : `The failure signature tracked by this issue occurred again; recording the recurrence on the canonical issue instead of filing a duplicate (spec/postmortem-lifecycle.md).`,
    '',
    `- **Signature hash:** \`${opts.signatureHash}\``,
  ];
  if (postCloseCount != null) {
    lines.push(`- **Post-close occurrences:** ${postCloseCount} (reopen threshold: ${opts.reopenMin})`);
  }
  lines.push(`- **Occurrence:** \`${opts.occurrenceId || new Date().toISOString()}\``);
  if (opts.runUrl) lines.push(`- **Source:** ${opts.runUrl}`);
  const context = String(opts.context || '').trim();
  if (context) {
    lines.push('', '<details><summary>Recurrence context</summary>', '', '```text', context.slice(0, 1500), '```', '', '</details>');
  }
  lines.push('', recurrenceMarker(opts.signatureHash, opts.occurrenceId));
  return lines.join('\n');
}

// --- Core decision + mutation ----------------------------------------------

/**
 * Resolve one failure occurrence against the repo's postmortem issues.
 * `client` supplies the GitHub surface (see ghClient()); tests inject a fake.
 * Returns { action, issueNumber?, issueUrl?, postCloseCount?, reason? }.
 * Actions: filed | commented | reopened | duplicate-occurrence |
 *          dry_run_file | dry_run_comment | dry_run_reopen.
 */
function handleOccurrence(opts, client) {
  const repo = opts.repo;
  const hash = opts.signatureHash;
  const now = opts.now || new Date();
  const reopenMin = Number.isFinite(opts.reopenMin) ? opts.reopenMin : DEFAULT_REOPEN_MIN;
  const windowDays = Number.isFinite(opts.dedupWindowDays) ? opts.dedupWindowDays : DEFAULT_DEDUP_WINDOW_DAYS;
  const dryRun = !!opts.dryRun;
  const o = { ...opts, signatureHash: hash, reopenMin };

  // 1. Open canonical — every recurrence comments; never reopen, never dup.
  const open = client.listIssues({ repo, state: 'open' });
  const openMatch = findCanonical(open, hash);
  if (openMatch) {
    const comments = client.listComments({ repo, issueNumber: openMatch.number });
    if (occurrenceRecorded(comments, hash, opts.occurrenceId)) {
      return { action: 'duplicate-occurrence', issueNumber: openMatch.number, issueUrl: openMatch.url, reason: 'occurrence already recorded' };
    }
    if (!dryRun) {
      client.addComment({ repo, issueNumber: openMatch.number, body: recurrenceComment(o, {}) });
    }
    return { action: dryRun ? 'dry_run_comment' : 'commented', issueNumber: openMatch.number, issueUrl: openMatch.url };
  }

  // 2. Closed canonical inside the dedup window — comment, then reopen at
  //    the post-close recurrence threshold. Comment lands first so evidence
  //    survives a failed reopen call.
  const closed = client.listIssues({ repo, state: 'closed', label: POSTMORTEM_LABEL });
  const closedMatch = findCanonical(
    (closed || []).filter(i => withinDedupWindow(i, now, windowDays)), hash);

  if (!closedMatch) {
    if (dryRun) return { action: 'dry_run_file', signatureHash: hash };
    const labels = Array.isArray(opts.labels) ? opts.labels : String(opts.labels || DEFAULT_LABELS).split(',').map(s => s.trim()).filter(Boolean);
    if (typeof client.ensureLabel === 'function') client.ensureLabel({ repo, label: POSTMORTEM_LABEL });
    const created = client.createIssue({
      repo,
      title: opts.title,
      body: filingBody(o),
      labels,
    });
    return { action: 'filed', issueNumber: created.number, issueUrl: created.url, signatureHash: hash };
  }

  const comments = client.listComments({ repo, issueNumber: closedMatch.number });
  if (occurrenceRecorded(comments, hash, opts.occurrenceId)) {
    return { action: 'duplicate-occurrence', issueNumber: closedMatch.number, issueUrl: closedMatch.url, reason: 'occurrence already recorded' };
  }
  const postCloseCount = postCloseRecurrenceCount(comments, hash, closedMatch.closedAt) + 1;
  const willReopen = postCloseCount >= reopenMin;
  if (!dryRun) {
    client.addComment({
      repo,
      issueNumber: closedMatch.number,
      body: recurrenceComment(o, { postCloseCount, reopening: willReopen }),
    });
    if (willReopen) client.reopenIssue({ repo, issueNumber: closedMatch.number });
  }
  return {
    action: dryRun ? (willReopen ? 'dry_run_reopen' : 'dry_run_comment') : (willReopen ? 'reopened' : 'commented'),
    issueNumber: closedMatch.number,
    issueUrl: closedMatch.url,
    postCloseCount,
    reopenMin,
  };
}

// --- gh-backed client --------------------------------------------------------
// Thin shell over `gh` — the same call surface tests fake. All reads bounded.

function ghClient() {
  const run = (args, allowFail = false) => {
    try {
      return execFileSync('gh', args, {
        encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'], timeout: 30000,
      });
    } catch (e) {
      if (allowFail) return '';
      throw e;
    }
  };
  const bodyFile = (body) => {
    const f = `/tmp/ai_sdlc_postmortem_${Date.now()}_${Math.random().toString(36).slice(2, 8)}.md`;
    fs.writeFileSync(f, body, 'utf8');
    return f;
  };
  return {
    listIssues({ repo, state, label }) {
      const args = ['issue', 'list', '--repo', repo, '--state', state,
        '--limit', String(MAX_ISSUES),
        '--json', 'number,title,body,labels,url,closedAt'];
      if (label) args.push('--label', label);
      const out = run(args, true);
      try { return out ? JSON.parse(out) : []; }
      catch (e) { console.warn(`[!] gh issue list (${state}) failed for ${repo} — treating as empty`); return []; }
    },
    listComments({ repo, issueNumber }) {
      const all = [];
      for (let page = 1; page <= MAX_COMMENT_PAGES; page++) {
        const out = run(['api', `repos/${repo}/issues/${issueNumber}/comments?per_page=${COMMENTS_PER_PAGE}&page=${page}`,
          '--jq', '.[] | {created_at, body}'], true);
        if (!out.trim()) break;
        const rows = out.trim().split('\n').map(l => { try { return JSON.parse(l); } catch (e) { return null; } }).filter(Boolean);
        all.push(...rows);
        if (rows.length < COMMENTS_PER_PAGE) break;
      }
      return all;
    },
    addComment({ repo, issueNumber, body }) {
      const f = bodyFile(body);
      try {
        run(['issue', 'comment', String(issueNumber), '--repo', repo, '--body-file', f]);
      } finally { try { fs.unlinkSync(f); } catch (e) {} }
    },
    reopenIssue({ repo, issueNumber }) {
      run(['issue', 'reopen', String(issueNumber), '--repo', repo]);
    },
    ensureLabel({ repo, label }) {
      run(['label', 'create', label, '--repo', repo, '--color', '5319e7',
        '--description', 'AI-SDLC canonical postmortem — deduped by failure signature'], true);
    },
    createIssue({ repo, title, body, labels }) {
      const f = bodyFile(body);
      try {
        let out = '';
        const base = ['issue', 'create', '--repo', repo, '--title', title, '--body-file', f];
        try {
          out = run([...base, '--label', (labels || []).join(',')]);
        } catch (e) {
          // Label fallback: a repo missing a label must not lose the filing.
          console.warn(`[!] issue create with labels failed (${String(e.message).split('\n')[0]}) — retrying without labels`);
          out = run(base);
        }
        const m = out.trim().match(/\/issues\/(\d+)/);
        return { number: m ? parseInt(m[1], 10) : null, url: out.trim() };
      } finally { try { fs.unlinkSync(f); } catch (e) {} }
    },
  };
}

// --- CLI ---------------------------------------------------------------------

function parseArgs(argv) {
  const opts = { _: [] };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a.startsWith('--')) {
      const k = a.slice(2);
      if (['dry-run', 'json', 'github-output'].includes(k)) opts[k] = true;
      else opts[k] = argv[++i];
    } else opts._.push(a);
  }
  return opts;
}

function readMaybeFile(value, file) {
  if (file) return fs.readFileSync(file, 'utf8');
  return value || '';
}

function main() {
  const a = parseArgs(process.argv.slice(2));
  if (!a.repo || !/^[-A-Za-z0-9_.]+\/[-A-Za-z0-9_.]+$/.test(a.repo)) {
    console.error('usage: postmortem.js --repo OWNER/NAME (--component C --class K --location L | --signature S | --signature-hash H) --title T [--body B|--body-file F] [--occurrence-id ID] [--run-url U] [--reopen-min N] [--dedup-window-days N] [--labels csv] [--context X|--context-file F] [--dry-run] [--json] [--github-output]');
    process.exit(2);
  }
  const signatureHash = a['signature-hash']
    || (a.signature ? computeSignatureHash(a.signature)
      : computeSignatureHash({ component: a.component, class: a['class'], location: a.location }));
  const signatureText = a.signature
    || [a.component, a['class'], a.location].filter(Boolean).join(' ');

  const opts = {
    repo: a.repo,
    signatureHash,
    signatureText,
    component: a.component || '',
    class: a['class'] || '',
    location: a.location || '',
    title: a.title || `postmortem: failure signature ${signatureHash}`,
    body: readMaybeFile(a.body, a['body-file']),
    context: readMaybeFile(a.context, a['context-file']).slice(0, 1500),
    occurrenceId: a['occurrence-id'] || '',
    runUrl: a['run-url'] || '',
    labels: a.labels || DEFAULT_LABELS,
    reopenMin: parseInt(a['reopen-min'] || process.env.POSTMORTEM_REOPEN_MIN || '', 10),
    dedupWindowDays: parseInt(a['dedup-window-days'] || process.env.POSTMORTEM_DEDUP_WINDOW_DAYS || '', 10),
    dryRun: !!a['dry-run'] || process.env.POSTMORTEM_DRY_RUN === '1',
  };

  const result = handleOccurrence(opts, ghClient());
  console.error(`[postmortem] ${result.action} sig=${signatureHash}${result.issueNumber ? ` issue=#${result.issueNumber}` : ''}${result.postCloseCount != null ? ` post_close=${result.postCloseCount}/${result.reopenMin}` : ''}`);
  if (a['github-output'] && process.env.GITHUB_OUTPUT) {
    fs.appendFileSync(process.env.GITHUB_OUTPUT,
      `postmortem_action=${result.action}\npostmortem_issue=${result.issueNumber || ''}\npostmortem_signature=${signatureHash}\n`);
  }
  if (a.json || a['github-output']) console.log(JSON.stringify(result));
}

if (require.main === module) {
  main();
}

module.exports = {
  POSTMORTEM_LABEL,
  DEFAULT_REOPEN_MIN,
  DEFAULT_DEDUP_WINDOW_DAYS,
  collapseVolatile,
  normalizeSignatureText,
  computeSignatureHash,
  signatureMarker,
  recurrenceMarker,
  issueSignatureOf,
  matchesSignature,
  findCanonical,
  parseRecurrenceMarkers,
  occurrenceRecorded,
  postCloseRecurrenceCount,
  withinDedupWindow,
  filingBody,
  recurrenceComment,
  handleOccurrence,
};
