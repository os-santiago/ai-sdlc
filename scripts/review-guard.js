#!/usr/bin/env node
// scripts/review-guard.js — review-stage guard decision (issue #127)
//
// Extraction of the `Guard` heredoc from
// .github/workflows/ai-sdlc-review-run.yml: decides whether the current PR
// head gets a fresh review, resumes a blocked verdict into the bounded fix
// pass, or escalates when the fix budget is exhausted. State comes only
// from trusted-author comment markers (bots / repo collaborators) — a
// forged comment can never steer the loop.
//
//   kill-switch engaged/unreadable → skip kill-switch:*   (fail-closed)
//   pr closed/merged               → skip pr-<state>
//   draft                          → skip draft
//   skip label                     → skip skip-label
//   fork head                      → skip fork-head
//   not a pipeline PR              → skip non-pipeline-pr
//   review_mode=human              → skip review-mode-human
//   clean/inconclusive verdict     → skip already-reviewed
//   blocked verdict + fix consumed → skip fix-already-consumed
//   blocked + attempts >= MAX_FIX  → escalate fix-iterations-exhausted
//   blocked verdict                → fix resume-blocked-verdict
//   else                           → review new-head | external-blocking
//
// On `fix`, the findings embedded in the verdict comment's ```json block
// are the canonical record — reused by the fix prompt, no second AI pass.
//
// CLI:
//   node scripts/review-guard.js [--dir $WORK] [--json]
// Scalars come from env (same names the step sets):
//   REPO, PIPELINE_AUTHOR, PIPELINE_LABEL, REVIEW_MODE, EXT, MAX_FIX,
//   SKIP_LABELS (JSON array), ESCALATION, KILL_STATE, KILL_REASON
// Writes external.json (+ fix-findings.json on fix) into --dir; outputs
// append to $GITHUB_OUTPUT when set: decision, reason, attempt, attempts,
// fix_budget_remaining, head_ref, head_sha, base_ref, ext_blocking,
// verdict, findings_json.
'use strict';

const fs = require('fs');
const path = require('path');

const VERDICT_MARK = /<!-- ai-sdlc:review sha=([0-9a-f]{7,40}) verdict=(clean|blocked|inconclusive) -->/;
const FIX_MARK = /<!-- ai-sdlc:review-fix attempt=(\d+) sha=([0-9a-f]{7,40})/;
const EXT_BAD_CONCLUSIONS = ['failure', 'timed_out', 'action_required', 'cancelled'];

const readJson = (dir, f) => { try { return JSON.parse(fs.readFileSync(path.join(dir, f), 'utf8')); } catch (e) { return []; } };

// External reviewer ingestion — a blocking signal outranks the AI verdict
// (fail-safe). Slug match is tolerant: 'coderabbit' matches check-run name
// 'CodeRabbit' and login 'coderabbitai[bot]'.
function collectExternal({ ext, runs, reviews, rcomments }) {
  const match = (s) => { s = (s || '').toLowerCase(); return ext && s && (s.includes(ext) || ext.startsWith(s)); };
  const latest = new Map();
  for (const r of runs) if (match(r.app) || match(r.name)) { if (!latest.has(r.name) || r.id > latest.get(r.name).id) latest.set(r.name, r); }
  const extRunBad = [...latest.values()].filter((r) => EXT_BAD_CONCLUSIONS.includes(r.conclusion));
  const extRunPending = [...latest.values()].filter((r) => r.status !== 'completed');
  const lastExtReview = new Map();
  for (const r of reviews.filter((r) => match(r.login))) lastExtReview.set(r.login, r);  // chronological — last wins
  const extChanges = [...lastExtReview.values()].filter((r) => r.state === 'CHANGES_REQUESTED');
  return {
    reviewer: ext || null,
    blocking: extRunBad.length > 0 || extChanges.length > 0,
    pending: extRunPending.length > 0,
    blocking_checks: extRunBad.map((r) => r.name),
    changes_requested_by: extChanges.map((r) => r.login),
    inline_comments: rcomments.filter((c) => match(c.login)).length,
  };
}

// Extract the findings JSON embedded in the blocking verdict comment —
// the canonical record the fix pass consumes.
function extractFindings(verdictCommentBody) {
  const jm = /```json\s*([\s\S]*?)```/.exec(verdictCommentBody || '');
  if (jm) {
    try { return JSON.stringify(JSON.parse(jm[1]).findings || []); }
    catch (e) { return '[]'; }
  }
  return '[]';
}

function evaluateGuard({ pr, comments, runs, reviews, rcomments, env } = {}) {
  const e = env || process.env;
  const [owner] = String(e.REPO || '/').split('/');
  const labels = (pr.labels || []).map((l) => l.name);
  const SKIP = [...JSON.parse(e.SKIP_LABELS || '[]'), e.ESCALATION].filter(Boolean);
  const MAX_FIX = Number(e.MAX_FIX);
  const EXT = (e.EXT || '').trim().toLowerCase();

  // Marker state (trusted comments only — the caller's jq filter).
  const vmarks = comments.map((c) => ({ m: VERDICT_MARK.exec(c.body || ''), body: c.body || '' })).filter((x) => x.m);
  const fmarks = comments.map((c) => FIX_MARK.exec(c.body || '')).filter(Boolean);
  const headVs = vmarks.filter((x) => x.m[1] === pr.headRefOid);
  const headVerdict = headVs.length ? headVs[headVs.length - 1] : null;
  const fixForHead = fmarks.some((m) => m[2] === pr.headRefOid);
  const attempts = new Set(fmarks.map((m) => Number(m[1]))).size;

  const external = collectExternal({ ext: EXT, runs, reviews, rcomments });

  const author = ((pr.author || {}).login || '').toLowerCase();
  const pauthor = (e.PIPELINE_AUTHOR || '').toLowerCase();
  const isBot = author === pauthor || author === pauthor.replace(/\[bot\]$/, '');
  const pipeline = isBot || labels.includes(e.PIPELINE_LABEL);

  let decision = 'skip', reason = '';
  // Autonomy gate first (issue #172): a revoked or unreadable kill-switch
  // soft-skips review — no verdict, no fix dispatch.
  const killState = (e.KILL_STATE || 'unreadable').toLowerCase();
  if (killState !== 'clear') reason = `kill-switch:${killState === 'unreadable' ? 'unreadable-fail-closed' : 'engaged'}`;
  else if (pr.state !== 'OPEN') reason = `pr-${String(pr.state).toLowerCase()}`;
  else if (pr.isDraft) reason = 'draft';
  else if (labels.some((l) => SKIP.includes(l))) reason = 'skip-label';
  else if (pr.isCrossRepository || (pr.headRepositoryOwner || {}).login !== owner) reason = 'fork-head';
  else if (!pipeline) reason = 'non-pipeline-pr';
  else if (e.REVIEW_MODE === 'human') reason = 'review-mode-human';
  else if (headVerdict && headVerdict.m[2] !== 'blocked') reason = 'already-reviewed';
  else if (headVerdict && fixForHead) reason = 'fix-already-consumed';
  else if (headVerdict && attempts >= MAX_FIX) { decision = 'escalate'; reason = 'fix-iterations-exhausted'; }
  else if (headVerdict) { decision = 'fix'; reason = 'resume-blocked-verdict'; }
  else { decision = 'review'; reason = external.blocking ? 'external-blocking' : 'new-head'; }

  // Resume path: the fix prompt reuses the findings embedded in the
  // verdict comment (canonical record) — no second AI pass needed.
  let findings = '';
  if (decision === 'fix') findings = extractFindings((headVerdict || {}).body);

  return {
    decision, reason, external, findings, attempts, headVerdict,
    outputs: {
      decision, reason,
      attempt: attempts + 1,
      attempts,
      fix_budget_remaining: String(attempts < MAX_FIX),
      head_ref: pr.headRefName,
      head_sha: pr.headRefOid,
      base_ref: pr.baseRefName,
      ext_blocking: String(external.blocking),
      verdict: headVerdict ? headVerdict.m[2] : '',
      findings_json: findings,
    },
  };
}

function parseArgs(argv) {
  const opts = { dir: undefined, json: false };
  for (let i = 0; i < argv.length; i += 1) {
    const a = argv[i];
    const next = () => {
      i += 1;
      if (i >= argv.length) throw new Error(`missing value for ${a}`);
      return argv[i];
    };
    if (a === '--dir') opts.dir = next();
    else if (a === '--json') opts.json = true;
    else if (a === '--help' || a === '-h') { console.log('node scripts/review-guard.js [--dir D] [--json]'); process.exit(0); }
    else throw new Error(`unknown argument: ${a}`);
  }
  return opts;
}

function main(argv, { env } = {}) {
  const opts = parseArgs(argv);
  const e = env || process.env;
  const W = opts.dir || e.WORK;
  if (!W) throw new Error('no work dir: pass --dir or set $WORK');
  const r = evaluateGuard({
    pr: readJson(W, 'pr.json'),
    comments: readJson(W, 'comments.json'),
    runs: readJson(W, 'check-runs.json'),
    reviews: readJson(W, 'reviews.json'),
    rcomments: readJson(W, 'review-comments.json'),
    env: e,
  });
  fs.writeFileSync(path.join(W, 'external.json'), JSON.stringify(r.external, null, 2));
  if (r.decision === 'fix') fs.writeFileSync(path.join(W, 'fix-findings.json'), r.findings);
  console.log(`decision: ${r.decision} (${r.reason}) head=${String(r.outputs.head_sha).slice(0, 8)} attempts=${r.attempts}/${e.MAX_FIX} ext_blocking=${r.external.blocking}`);
  if (e.GITHUB_OUTPUT) {
    fs.appendFileSync(e.GITHUB_OUTPUT,
      Object.entries(r.outputs).map(([k, v]) => `${k}=${v}`).join('\n') + '\n');
  }
  if (opts.json) console.log(JSON.stringify(r.outputs));
  return r;
}

if (require.main === module) {
  try {
    main(process.argv.slice(2));
  } catch (e) {
    console.error(`review-guard: ${e.message}`);
    process.exit(2);
  }
}

module.exports = { VERDICT_MARK, FIX_MARK, collectExternal, extractFindings, evaluateGuard, parseArgs, main };
