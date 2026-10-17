#!/usr/bin/env node
// scripts/dup-detector.js — duplicate detection across the open backlog
// (issue #161, epic #147 intake intelligence).
//
// Deterministic, embedding-free similarity — a port of the Hermes
// control-plane `dup_detector.js` narrowed to this runtime's contract:
//   - normalized token Jaccard weighted toward titles, where issue
//     semantics concentrate: score = 0.7·title + 0.3·(title+body)
//   - marker-key matching for machine-filed bodies: a shared
//     `ai-sdlc-failure-signature`/`ai-sdlc-postmortem-meta` sigHash or a
//     repeated `hermes:epic-child key:"…" parent:#N` is a certain
//     duplicate regardless of text shape; a signature pair carrying
//     DIFFERENT hashes is never a duplicate — each filing is the
//     canonical issue for its signature (spec/postmortem-lifecycle.md)
//   - generated-template chrome (HTML comments, ATX headings, table
//     rows, task-list items) is stripped before scoring so shared
//     boilerplate can't dominate a text verdict
//
// Reporting: ONE upserted comment per issue, marked
//   <!-- ai-sdlc:dup-scan fp:"<sha16>" -->
// fingerprinted over the match set — identical findings never re-post;
// a changed set PATCHes in place; a set that empties rewrites the
// comment to a cleared notice. Advisory only: the detector NEVER closes
// an issue and NEVER applies the `duplicate` label — spec/labels.md T10
// assigns that transition to `dedup`/`human`; this component suggests,
// a human (or an authorized component) disposes.
//
// Wired into ai-sdlc-intake.yml (fail-open, like the refiner): runs on
// every intake event, scores the issue against the open backlog, and
// reports before the DoR gate. `--sweep` scans the whole open backlog
// pairwise for a backlog audit (report-only unless --apply).
//
// CLI:
//   node scripts/dup-detector.js --repo OWNER/NAME --issue N [--apply]
//       [--dup-threshold 0.60] [--related-threshold 0.30]
//       [--max-candidates 200] [--json] [--github-output]
//   node scripts/dup-detector.js --repo OWNER/NAME --sweep [--apply] [...]
//   node scripts/dup-detector.js --issue-file issue.json \
//       [--candidates-file c.json] [--comments-file c.json] [--json]  # offline
'use strict';

const crypto = require('crypto');
const fs = require('fs');
const { execFileSync } = require('child_process');

const MARKER_RE = /<!--\s*ai-sdlc:dup-scan\s+fp:"([a-f0-9]+)"\s*-->/;
const MARKER_SEARCH_RE = /<!--\s*ai-sdlc:dup-scan\s/;

const DUP_THRESHOLD = parseFloat(process.env.AI_SDLC_DUP_THRESHOLD || '0.60');
const RELATED_THRESHOLD = parseFloat(process.env.AI_SDLC_RELATED_THRESHOLD || '0.30');
const MAX_CANDIDATES = parseInt(process.env.AI_SDLC_DUP_MAX_CANDIDATES || '200', 10);
const MAX_LISTED_DUPS = 10;
const MAX_LISTED_RELATED = 5;
const MAX_COMMENTS_SCANNED = 100;

const STOPWORDS = new Set([
  'a', 'an', 'the', 'and', 'or', 'of', 'to', 'in', 'on', 'for', 'with', 'is',
  'are', 'be', 'been', 'it', 'this', 'that', 'when', 'if', 'as', 'at', 'by',
  'from', 'into', 'not', 'no', 'do', 'does', 'should', 'would', 'could',
  'el', 'la', 'los', 'las', 'de', 'del', 'en', 'un', 'una', 'que', 'se',
  'feat', 'fix', 'bug', 'chore', 'docs', 'issue', 'pr',
]);

function tokenize(text) {
  const tokens = String(text || '')
    .toLowerCase()
    .replace(/```[\s\S]*?```/g, ' ') // code blocks add noise, not semantics
    .replace(/[^\p{L}\p{N}_-]+/gu, ' ')
    .split(/\s+/)
    // Keep 2+ char tokens plus single digits — dropping them collapses
    // "task 1" vs "task 2" or "node 20" into false duplicates.
    .filter((t) => (t.length >= 2 || /\d/.test(t)) && !STOPWORDS.has(t));
  return new Set(tokens);
}

function jaccard(a, b) {
  if (!a.size || !b.size) return 0;
  let inter = 0;
  for (const t of a) if (b.has(t)) inter += 1;
  return inter / (a.size + b.size - inter);
}

// Generated-template chrome stripped from bodies before scoring: HTML
// comments (including the ai-sdlc-*/hermes:* machine markers), ATX
// heading lines, markdown table rows and task-list items. Template
// filings otherwise score high on identical boilerplate alone.
function stripBoilerplate(text) {
  return String(text || '')
    .replace(/<!--[\s\S]*?-->/g, ' ')
    .replace(/^[ \t]{0,3}#{1,6}[ \t][^\n]*/gm, ' ')
    .replace(/^[ \t]*\|[^\n]*/gm, ' ')
    .replace(/^[ \t]*[-*+][ \t]+\[[ xX]\][^\n]*/gm, ' ');
}

// --- Marker keys ---------------------------------------------------------------
//
// Marker extraction reads the RAW body — stripBoilerplate removes the
// HTML comments these markers live in.

const SIG_RE = /<!--\s*ai-sdlc-failure-signature:\s*([0-9a-f]{8,64})\s*-->/i;
const META_RE = /<!--\s*ai-sdlc-postmortem-meta:\s*(\{[\s\S]*?\})\s*-->/;
const EPIC_CHILD_RE = /<!--\s*hermes:epic-child\s+key:"?([A-Za-z0-9_.-]+)"?\s+parent:#(\d+)\s*-->/g;
const SUB_RE = /<!--\s*hermes:sub:([A-Za-z0-9_.-]+)\s*-->/g;

// Postmortem signature hash: the `ai-sdlc-failure-signature` comment is
// authoritative; the `ai-sdlc-postmortem-meta` JSON blob is the fallback
// carrier (scripts/postmortem.js contract). null when no marker.
function signatureHashOf(text) {
  const s = String(text || '');
  const sig = s.match(SIG_RE);
  if (sig) return sig[1].toLowerCase();
  const meta = s.match(META_RE);
  if (meta) {
    try {
      const parsed = JSON.parse(meta[1]);
      const h = parsed && (parsed.sigHash || parsed.signatureHash);
      if (typeof h === 'string' && h) return h.toLowerCase();
    } catch (e) { /* malformed marker — treat as absent */ }
  }
  return null;
}

// Machine-filing identity keys: `sig:<hash>` for postmortems,
// `child:<parent>:<key>` for epic-decomposer filings, `sub:<key>` for
// sub-issue markers. A shared key is a certain duplicate — the same
// filing landed twice; text shape is irrelevant.
function markerKeysOf(text) {
  const keys = new Set();
  const s = String(text || '');
  const sig = signatureHashOf(s);
  if (sig) keys.add(`sig:${sig}`);
  for (const m of s.matchAll(EPIC_CHILD_RE)) keys.add(`child:${m[2]}:${m[1]}`);
  for (const m of s.matchAll(SUB_RE)) keys.add(`sub:${m[1]}`);
  return keys;
}

// --- Similarity -----------------------------------------------------------------

// Similarity 0..1 between an issue and a candidate {title, body}. Title
// overlap dominates (0.7); body adds signal (0.3). Bodies are scored on
// free text only — template chrome is stripped first.
function similarity(issue, candidate) {
  const tScore = jaccard(tokenize(issue.title), tokenize(candidate.title));
  const bScore = jaccard(
    tokenize(`${issue.title} ${stripBoilerplate(issue.body)}`),
    tokenize(`${candidate.title} ${stripBoilerplate(candidate.body)}`),
  );
  return Math.round((tScore * 0.7 + bScore * 0.3) * 1000) / 1000;
}

/**
 * Score an issue against open-backlog candidates → {duplicates, related}
 * sorted by score. candidates: [{number, title, body?, url?, state?}] —
 * self is excluded by number. A shared marker key short-circuits to a
 * certain duplicate (score 1, `via` carries the matched keys); a
 * signature pair with different hashes is dropped before text scoring.
 */
function findMatches(issue, candidates, opts = {}) {
  const dupT = opts.dupThreshold !== undefined ? opts.dupThreshold : DUP_THRESHOLD;
  const relT = opts.relatedThreshold !== undefined ? opts.relatedThreshold : RELATED_THRESHOLD;
  const iKeys = markerKeysOf(issue && issue.body);
  const iSigs = [...iKeys].filter((k) => k.startsWith('sig:'));
  const scored = [];
  for (const c of candidates || []) {
    if (!c || c.number === (issue && issue.number)) continue;
    const cKeys = markerKeysOf(c.body);
    const shared = [...iKeys].filter((k) => cKeys.has(k));
    let score;
    if (shared.length) {
      score = 1;
    } else {
      // Both carry postmortem signatures but none match → each filing is
      // the canonical issue for its signature; never a text duplicate.
      const cSigs = [...cKeys].filter((k) => k.startsWith('sig:'));
      if (iSigs.length && cSigs.length) continue;
      score = similarity(issue, c);
      if (score < relT) continue;
    }
    scored.push({
      number: c.number,
      title: c.title,
      url: c.url || `#${c.number}`,
      score,
      via: shared.length ? shared : undefined,
      state: c.state,
    });
  }
  scored.sort((a, b) => b.score - a.score || a.number - b.number);
  return {
    duplicates: scored.filter((s) => s.score >= dupT),
    related: scored.filter((s) => s.score < dupT),
  };
}

// --- Comment marker + rendering ---------------------------------------------------

function parseScanMarker(body) {
  const m = MARKER_RE.exec(String(body || ''));
  return m ? { fp: m[1] } : null;
}

function markerLine(fp) {
  return `<!-- ai-sdlc:dup-scan fp:"${fp}" -->`;
}

// The latest dup-scan comment on the issue (marker + REST comment id —
// needed for the PATCH upsert).
function findScanComment(comments) {
  const list = (Array.isArray(comments) ? comments : []).slice(-MAX_COMMENTS_SCANNED);
  for (let i = list.length - 1; i >= 0; i -= 1) {
    const c = list[i];
    if (c && c.body && MARKER_SEARCH_RE.test(String(c.body))) {
      return { id: c.id, body: String(c.body), marker: parseScanMarker(c.body) };
    }
  }
  return null;
}

// Fingerprint over the match set — `kind:number:score` sorted, so an
// identical finding never re-posts and any movement re-renders.
function matchFingerprint(duplicates, related) {
  const rows = [
    ...duplicates.map((m) => `dup:${m.number}:${m.score}`),
    ...related.map((m) => `rel:${m.number}:${m.score}`),
  ].sort();
  return crypto.createHash('sha256').update(rows.join('\n')).digest('hex').slice(0, 16);
}

function bulletLine(m) {
  const via = m.via && m.via.length ? ` · marker \`${m.via[0]}\`` : '';
  const url = m.url && m.url !== `#${m.number}` ? ` — ${m.url}` : '';
  return `- **#${m.number}** — ${String(m.title || '').trim() || '(untitled)'} (score ${m.score})${via}${url}`;
}

function renderScanComment({ fp, duplicates, related }) {
  const lines = [
    markerLine(fp),
    '\u{1F501} **AI-SDLC intake — possible duplicate detected.** This issue closely matches open backlog item(s):',
    '',
    ...duplicates.slice(0, MAX_LISTED_DUPS).map(bulletLine),
  ];
  if (related.length) {
    lines.push('', 'Possibly related (below duplicate threshold):',
      '', ...related.slice(0, MAX_LISTED_RELATED).map(bulletLine));
  }
  lines.push('',
    'Detection is advisory — nothing was closed or label-blocked automatically '
      + '(spec/labels.md T10: `dedup`/`human` own the `duplicate` transition). '
      + 'If one of these is the canonical issue, a maintainer can apply `duplicate` '
      + 'or close this issue; otherwise dismiss this comment.');
  return lines.join('\n');
}

function renderClearedComment(fp) {
  return [
    markerLine(fp),
    '\u{1F501} **AI-SDLC intake — duplicate scan:** earlier candidates are no longer open '
      + 'or no longer match. Nothing to confirm — detection remains advisory only.',
  ].join('\n');
}

// --- Decision (pure) ---------------------------------------------------------------

/**
 * Decide the dup-scan action for one issue snapshot.
 *
 * @param {object} opts.issue      {number,title,body}
 * @param {Array}  opts.candidates open-backlog issues [{number,title,body?,url?}]
 * @param {Array}  opts.comments   issue comments (REST or gh --json shape)
 * @returns {object} {action, fingerprint, duplicates, related, ...}
 *   action: 'none' | 'report' | 'unchanged' | 'clear'
 */
function detectIssue(opts) {
  const issue = opts.issue || {};
  const comments = opts.comments || [];
  const { duplicates, related } = findMatches(issue, opts.candidates, opts);
  const prior = findScanComment(comments);
  const fp = matchFingerprint(duplicates, related);
  const base = {
    fingerprint: fp,
    issue: issue.number,
    priorCommentId: prior ? prior.id : null,
    duplicates,
    related,
  };

  if (!duplicates.length) {
    // Below the report bar — only worth a write when a stale scan
    // comment still claims candidates.
    if (prior) return { ...base, action: 'clear', comment: renderClearedComment(fp) };
    return { ...base, action: 'none' };
  }
  if (prior && prior.marker && prior.marker.fp === fp) {
    return { ...base, action: 'unchanged' };
  }
  return { ...base, action: 'report', comment: renderScanComment({ fp, duplicates, related }) };
}

// --- GitHub I/O --------------------------------------------------------------------

// Injectable exec for tests: fn(argv) -> stdout string (throws on failure).
function defaultExec(argv) {
  return execFileSync('gh', argv, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
}

function fetchIssue(repo, issueNumber, exec) {
  const run = exec || defaultExec;
  const out = run(['issue', 'view', String(issueNumber), '-R', repo, '--json', 'number,title,body,labels,author,url']);
  return JSON.parse(out || '{}');
}

// The open backlog — bounded by --max-candidates (default 200). `gh issue
// list` returns issues only, never PRs.
function fetchOpenIssues(repo, limit, exec) {
  const run = exec || defaultExec;
  const out = run(['issue', 'list', '-R', repo, '--state', 'open',
    '--limit', String(limit || MAX_CANDIDATES),
    '--json', 'number,title,body,url,labels']);
  return JSON.parse(out || '[]');
}

// REST issue comments — the numeric `id` is required for the PATCH
// upsert (gh --json comments expose node ids, unusable here).
function fetchComments(repo, issueNumber, exec) {
  const run = exec || defaultExec;
  const out = run(['api', '--paginate', `repos/${repo}/issues/${issueNumber}/comments?per_page=100`]);
  return String(out || '[]')
    .split('\n')
    .map((l) => l.trim())
    .filter((l) => l.startsWith('[') || l.startsWith('{'))
    .flatMap((l) => {
      try {
        const doc = JSON.parse(l);
        return Array.isArray(doc) ? doc : [doc];
      } catch (e) {
        return [];
      }
    })
    .map((c) => ({ id: c.id, body: c.body, author: { login: c.user && c.user.login } }));
}

// One upsert: PATCH the existing marker comment or POST a new one.
function upsertComment({ repo, issue, commentId, body, exec }) {
  const run = exec || defaultExec;
  if (commentId) {
    run(['api', `repos/${repo}/issues/comments/${commentId}`, '-X', 'PATCH', '-f', `body=${body}`]);
    return { op: 'patch', id: commentId };
  }
  run(['issue', 'comment', String(issue), '-R', repo, '--body', body]);
  return { op: 'create' };
}

// --- CLI ------------------------------------------------------------------------------

function usage() {
  return [
    'usage: dup-detector.js [--repo OWNER/NAME --issue N] [--repo OWNER/NAME --sweep]',
    '                       [--issue-file F] [--candidates-file F] [--comments-file F]',
    '                       [--dup-threshold F] [--related-threshold F] [--max-candidates N]',
    '                       [--apply] [--json] [--github-output]',
  ].join('\n');
}

function parseArgs(argv) {
  const opts = { apply: false, json: false, githubOutput: false, sweep: false };
  for (let i = 0; i < argv.length; i += 1) {
    const a = argv[i];
    const next = () => {
      i += 1;
      if (i >= argv.length) throw new Error(`missing value for ${a}`);
      return argv[i];
    };
    if (a === '--repo') opts.repo = next();
    else if (a === '--issue') opts.issue = next();
    else if (a === '--sweep') opts.sweep = true;
    else if (a === '--issue-file') opts.issueFile = next();
    else if (a === '--candidates-file') opts.candidatesFile = next();
    else if (a === '--comments-file') opts.commentsFile = next();
    else if (a === '--dup-threshold') opts.dupThreshold = Number(next());
    else if (a === '--related-threshold') opts.relatedThreshold = Number(next());
    else if (a === '--max-candidates') opts.maxCandidates = Number(next());
    else if (a === '--apply') opts.apply = true;
    else if (a === '--json') opts.json = true;
    else if (a === '--github-output') opts.githubOutput = true;
    else if (a === '--help' || a === '-h') { console.log(usage()); process.exit(0); }
    else throw new Error(`unknown argument: ${a}`);
  }
  return opts;
}

function thresholds(opts) {
  return {
    dupThreshold: opts.dupThreshold !== undefined ? opts.dupThreshold : DUP_THRESHOLD,
    relatedThreshold: opts.relatedThreshold !== undefined ? opts.relatedThreshold : RELATED_THRESHOLD,
  };
}

// Backlog-wide pairwise scan: every unordered pair that scores at or
// above the related threshold, sorted by score. Report-only unless
// --apply is set (then each member issue gets the standard upsert).
function sweepBacklog({ repo, candidates, opts, exec, apply }) {
  const seen = new Set();
  const pairs = [];
  for (const issue of candidates) {
    const { duplicates, related } = findMatches(issue, candidates, opts);
    for (const m of [...duplicates, ...related]) {
      const key = [Math.min(issue.number, m.number), Math.max(issue.number, m.number)].join(':');
      if (seen.has(key)) continue;
      seen.add(key);
      pairs.push({
        a: issue.number, b: m.number, score: m.score,
        duplicate: m.score >= opts.dupThreshold, via: m.via,
      });
    }
  }
  pairs.sort((x, y) => y.score - x.score || x.a - y.a || x.b - y.b);
  const applied = [];
  if (apply && repo) {
    for (const issue of candidates) {
      const comments = fetchComments(repo, issue.number, exec);
      const result = detectIssue({ issue, candidates, comments, ...opts });
      if (result.action === 'report' || result.action === 'clear') {
        const op = upsertComment({
          repo, issue: issue.number, commentId: result.priorCommentId, body: result.comment, exec,
        });
        applied.push({ issue: issue.number, action: result.action, op: op.op });
      }
    }
  }
  return { pairs, applied };
}

function main(argv, { exec, stdout } = {}) {
  const opts = parseArgs(argv);
  const emit = stdout || ((s) => console.log(s));
  const run = exec || defaultExec;
  const t = thresholds(opts);

  // --- offline fixture mode ---
  if (opts.issueFile) {
    const issue = JSON.parse(fs.readFileSync(opts.issueFile, 'utf8'));
    const candidates = opts.candidatesFile ? JSON.parse(fs.readFileSync(opts.candidatesFile, 'utf8')) : [];
    const comments = opts.commentsFile ? JSON.parse(fs.readFileSync(opts.commentsFile, 'utf8')) : [];
    const result = detectIssue({ issue, candidates, comments, ...t });
    if (opts.apply && opts.repo
        && (result.action === 'report' || result.action === 'clear')) {
      result.applied = {
        comment: upsertComment({
          repo: opts.repo, issue: result.issue, commentId: result.priorCommentId,
          body: result.comment, exec: run,
        }),
      };
    } else {
      delete result.comment; // preview mode: don't emit the full body in JSON
    }
    emit(JSON.stringify(result));
    return result;
  }

  if (!opts.repo) throw new Error(`--repo is required (or use --issue-file)\n${usage()}`);

  const candidates = fetchOpenIssues(opts.repo, opts.maxCandidates || MAX_CANDIDATES, run);

  // --- backlog sweep mode ---
  if (opts.sweep) {
    const report = sweepBacklog({
      repo: opts.repo, candidates, opts: t, exec: run, apply: opts.apply,
    });
    if (opts.githubOutput && process.env.GITHUB_OUTPUT) {
      fs.appendFileSync(process.env.GITHUB_OUTPUT, [
        'status=sweep',
        `pairs=${report.pairs.length}`,
        `duplicates=${report.pairs.filter((p) => p.duplicate).length}`,
      ].join('\n') + '\n');
    }
    emit(JSON.stringify(report));
    return report;
  }

  // --- single-issue intake mode ---
  if (!opts.issue) throw new Error(`--issue is required (or use --sweep / --issue-file)\n${usage()}`);
  const issue = fetchIssue(opts.repo, opts.issue, run);
  const comments = fetchComments(opts.repo, opts.issue, run);
  const result = detectIssue({ issue, candidates, comments, ...t });

  if (opts.apply && (result.action === 'report' || result.action === 'clear')) {
    result.applied = {
      comment: upsertComment({
        repo: opts.repo, issue: result.issue, commentId: result.priorCommentId,
        body: result.comment, exec: run,
      }),
    };
  } else {
    delete result.comment;
  }

  if (opts.githubOutput && process.env.GITHUB_OUTPUT) {
    fs.appendFileSync(process.env.GITHUB_OUTPUT, [
      `status=${result.action}`,
      `duplicates=${result.duplicates.map((d) => d.number).join(',')}`,
      `related=${result.related.map((r) => r.number).join(',')}`,
      `top_score=${result.duplicates.length ? result.duplicates[0].score : (result.related[0] ? result.related[0].score : 0)}`,
    ].join('\n') + '\n');
  }

  emit(JSON.stringify(result));
  return result;
}

if (require.main === module) {
  try {
    main(process.argv.slice(2));
  } catch (e) {
    console.error(`dup-detector: ${e.message}`);
    process.exit(2);
  }
}

module.exports = {
  MARKER_RE,
  DUP_THRESHOLD,
  RELATED_THRESHOLD,
  MAX_CANDIDATES,
  tokenize,
  jaccard,
  stripBoilerplate,
  signatureHashOf,
  markerKeysOf,
  similarity,
  findMatches,
  parseScanMarker,
  findScanComment,
  matchFingerprint,
  renderScanComment,
  renderClearedComment,
  detectIssue,
  fetchIssue,
  fetchOpenIssues,
  fetchComments,
  upsertComment,
  sweepBacklog,
  main,
};
