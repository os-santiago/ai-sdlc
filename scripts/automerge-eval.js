#!/usr/bin/env node
// scripts/automerge-eval.js — merge-gate decision table (issue #127)
//
// Extraction of the `Evaluate merge gate` heredoc from
// .github/workflows/ai-sdlc-automerge.yml — the decision logic was
// untestable inside YAML; here it is a pure function over the JSON inputs
// the step already collects, with fixture-driven coverage per
// decision-table row.
//
// Ports the Hermes pr_merger semantics (spec/risk-taxonomy.md):
//   draft / skip-label / non-main base → skipped
//   provenance gate (issue #119)       → escalated (fork head / untrusted
//                                        author; same-repo pipeline-app
//                                        heads pass)
//   main-guard halt (issue #189)       → held     (transient, not escalation)
//   pr:risk-critical unaccepted        → held
//   CONFLICTING                        → escalated
//   failing checks                     → repair-signaled / held / escalated
//     (per-head repair-signal markers bound the loop: stalled past
//      PENDING_MAX → repair-stalled; distinct signatures >= MAX_REPAIRS →
//      repairs-exhausted)
//   pending checks past cap            → escalated (pending-timeout)
//   unknown check bucket (issue #360)  → held, escalated past cap —
//     fail-closed: unknown/error states never count as green
//   required check absent from rollup  → held, escalated past cap
//     (verify.required_checks; issue #360 — a check that never ran is a
//      missing signal, not a green one)
//   MERGEABLE                          → review gate (issue #28):
//     external reviewer blocking       → held then escalated at wait cap
//     verdict=blocked                  → held while fix pass runs /
//                                        escalated on stall
//     zero check-runs + require_checks → held while young, escalated
//                                        (no-checks) past the pending cap
//                                        (issue #120 — never merge blind)
//     no verdict / external pending    → held up to REVIEW_WAIT, then
//                                        merged with `review_timeout`
//     otherwise                        → merged (green | no-checks)
//
// Trust boundary: fetched from the runtime repo pin (inputs.runtime_ref),
// never from the PR head — a change must not weaken the gate that merges
// it.
//
// CLI:
//   node scripts/automerge-eval.js [--pr pr.json] [--checks checks.json]
//        [--trusted-comments trusted-comments.json]
//        [--ext-reviews ext-reviews.json] [--json]
//
// Scalars come from env (same names the workflow step sets):
//   MAIN_GUARD, MAIN_GUARD_REASON, PENDING_MAX, MAX_REPAIRS, SKIP_LABELS
//   (JSON array), ESCALATION, REVIEW_MODE, REVIEW_WAIT, EXT
//   Hardening gates (issues #119/#120): REQUIRE_PROV, REQUIRE_CHECKS,
//   PIPELINE_AUTHOR, ALLOW_AUTHORS (JSON array), REPO
//   Issue #360: REQUIRED_CHECKS (JSON array) + CONTRACT_JSON fallback —
//   when a flat output is absent/empty the resolved contract body supplies
//   merge.skip_labels and verify.required_checks.
// Outputs append to $GITHUB_OUTPUT when set: action, reason.
'use strict';

const fs = require('fs');

const REPAIR_MARK = /<!-- ai-sdlc:repair-signal sha=([0-9a-f]+) -->/;
const VERDICT_MARK = /<!-- ai-sdlc:review sha=([0-9a-f]{7,40}) verdict=(clean|blocked|inconclusive) -->/;
const FIX_MARK = /<!-- ai-sdlc:review-fix attempt=(\d+) sha=([0-9a-f]{7,40})/;

// Green buckets per `gh pr checks --json bucket` — pass and skipping (a
// legitimately skipped check is neutral, matching GitHub protection
// semantics). fail/cancel/pending have their own rows; anything else is
// an unknown signal (issue #360 — fail-closed, never counts as green).
const GREEN_BUCKETS = new Set(['pass', 'skipping']);
const FAILING_BUCKETS = new Set(['fail', 'cancel']);

function parseJsonList(v) {
  try { const p = JSON.parse(v || '[]'); return Array.isArray(p) ? p : []; }
  catch (err) { return []; }
}

function safeJsonObj(v) {
  try { const p = JSON.parse(v || '{}'); return p && typeof p === 'object' ? p : {}; }
  catch (err) { return {}; }
}

// Core decision table. `env` defaults to process.env so tests can inject a
// hermetic map; `nowMs` pins the clock for fixture-driven rows.
function evaluateMergeGate({ pr, checks, trustedComments, extReviews, env, nowMs } = {}) {
  const e = env || process.env;
  const now = nowMs === undefined ? Date.now() : nowMs;
  pr = pr || {};
  checks = Array.isArray(checks) ? checks : [];
  trustedComments = Array.isArray(trustedComments) ? trustedComments : [];
  extReviews = Array.isArray(extReviews) ? extReviews : [];

  const labels = (pr.labels || []).map((l) => l.name);
  // Resolved contract body — the flat step outputs (SKIP_LABELS,
  // REQUIRED_CHECKS) may be absent on a pinned load-config that predates
  // them; the resolved JSON always carries the keys (issue #360).
  const contractJson = safeJsonObj(e.CONTRACT_JSON);
  const envSkips = parseJsonList(e.SKIP_LABELS);
  const contractSkips = ((contractJson.merge || {}).skip_labels) || [];
  const SKIP = [...(envSkips.length ? envSkips : contractSkips), e.ESCALATION].filter(Boolean);
  // verify.required_checks (schema: "check-run names that must be green
  // for automerge") — a declared name absent from the rollup is a missing
  // signal, never a green one.
  const envRequired = parseJsonList(e.REQUIRED_CHECKS);
  const contractRequired = ((contractJson.verify || {}).required_checks) || [];
  const REQUIRED = envRequired.length ? envRequired : contractRequired;

  // Provenance gate (issue #119): with merge.require_pipeline_provenance
  // (default on) only a same-repo head authored by the pipeline app or
  // an explicit merge.allow_authors entry may merge — fork heads and
  // every other author escalate. Missing head/author data fails closed.
  const REQ_PROV = String(e.REQUIRE_PROV || 'true') !== 'false';
  const REQ_CHECKS = String(e.REQUIRE_CHECKS || 'true') !== 'false';
  const PIPELINE_AUTHOR = String(e.PIPELINE_AUTHOR || 'ai-sdlc-runtime[bot]').trim().toLowerCase();
  let extraAuthors = [];
  try { extraAuthors = JSON.parse(e.ALLOW_AUTHORS || '[]'); } catch (err) { extraAuthors = []; }
  const AUTHOR_OK = new Set([PIPELINE_AUTHOR, ...extraAuthors.map((a) => String(a).trim().toLowerCase())].filter(Boolean));
  const [repoOwner, repoName] = String(e.REPO || '/').split('/');
  const headOwner = String((pr.headRepositoryOwner || {}).login || '').toLowerCase();
  const headRepo = String((pr.headRepository || {}).name || '').toLowerCase();
  const authorLogin = String((pr.author || {}).login || '').toLowerCase();
  const crossRepo = pr.isCrossRepository === true || !headOwner
    || (repoOwner && headOwner !== String(repoOwner).toLowerCase())
    || (repoName && headRepo !== String(repoName).toLowerCase());

  // Decision table (spec/risk-taxonomy.md).
  let action = 'skipped', reason = '';
  if (pr.isDraft) { action = 'skipped'; reason = 'draft'; }
  else if (labels.some((l) => SKIP.includes(l))) { action = 'skipped'; reason = 'skip-label'; }
  else if (pr.baseRefName !== 'main') { action = 'skipped'; reason = 'non-main-base'; }
  else if (REQ_PROV && crossRepo) { action = 'escalated'; reason = 'provenance:fork-head'; }
  else if (REQ_PROV && !AUTHOR_OK.has(authorLogin)) { action = 'escalated'; reason = 'provenance:untrusted-author'; }
  // Main-guard sentinel (issue #189): a red base branch holds the
  // merge — a transient repo-wide condition, not an escalation.
  else if (e.MAIN_GUARD === 'halt') { action = 'held'; reason = `main-guard:${e.MAIN_GUARD_REASON || 'base-red'}`; }
  else if (labels.includes('pr:risk-critical') && !labels.includes('pr:risk-accepted')) { action = 'held'; reason = 'risk-critical-unaccepted'; }
  else if (pr.mergeable === 'CONFLICTING') { action = 'escalated'; reason = 'conflict'; }
  else {
    const failing = checks.filter((c) => FAILING_BUCKETS.has(c.bucket));
    const pending = checks.filter((c) => c.bucket === 'pending');
    // Fail-closed (issue #360): a bucket outside the known green/failing/
    // pending set is an unknown signal — it holds the merge like a pending
    // check and escalates past the same cap, never counts as green.
    const unknown = checks.filter((c) => !GREEN_BUCKETS.has(c.bucket)
      && !FAILING_BUCKETS.has(c.bucket) && c.bucket !== 'pending');
    // A required check that never reports is invisible to the bucket rows
    // above — its absence must block the merge just like a failure.
    const present = new Set(checks.map((c) => c.name));
    const missingRequired = REQUIRED.filter((n) => !present.has(n));
    const ageMin = (now - new Date(pr.createdAt)) / 60000;
    // Repair signals carry a per-head-SHA marker so repeated polls
    // (scheduled sweep) neither spam nor loop forever. Trusted
    // authors only (issue #121): a forged signal could stall the
    // merge (`repair-awaiting`) or exhaust the repair budget
    // (`repairs-exhausted`) — same trust model as verdict markers.
    const signals = trustedComments.map((c) => ({ m: REPAIR_MARK.exec(c.body || ''), at: c.created_at })).filter((x) => x.m);
    const signaledShas = new Set(signals.map((x) => x.m[1]));
    const current = signals.filter((x) => x.m[1] === pr.headRefOid);
    if (failing.length) {
      const names = failing.map((c) => c.name).join(',');
      if (current.length) {
        const sinceMin = (now - new Date(current[0].at)) / 60000;
        if (sinceMin > Number(e.PENDING_MAX)) { action = 'escalated'; reason = `repair-stalled:${names}`; }
        else { action = 'held'; reason = 'repair-awaiting'; }
      }
      else if (signaledShas.size >= Number(e.MAX_REPAIRS)) { action = 'escalated'; reason = `repairs-exhausted:${names}`; }
      else { action = 'repair-signaled'; reason = `failing:${names}`; }
    }
    else if (pending.length && ageMin > Number(e.PENDING_MAX)) { action = 'escalated'; reason = 'pending-timeout'; }
    else if (pending.length) { action = 'held'; reason = 'pending'; }
    else if (unknown.length && ageMin > Number(e.PENDING_MAX)) { action = 'escalated'; reason = `check-state-unknown:${unknown.map((c) => c.name).join(',')}`; }
    else if (unknown.length) { action = 'held'; reason = `check-state-unknown:${unknown.map((c) => c.name).join(',')}`; }
    else if (pr.mergeable === 'MERGEABLE') {
      const base = checks.length ? 'green' : 'no-checks';
      // Review gate (issue #28): verdict markers live on the PR as
      // trusted-author comments (`<!-- ai-sdlc:review sha=… -->`).
      // A pending verdict holds the merge only up to
      // review.wait_max_minutes, then merges with `review_timeout`
      // recorded — review infra can never deadlock the pipeline
      // (Hermes PR_REVIEW_WAIT_MINUTES semantics). A blocked verdict
      // holds while the bounded fix pass owns the branch.
      const REVIEW_ON = ['ai', 'hybrid'].includes((e.REVIEW_MODE || '').toLowerCase());
      const WAIT = Number(e.REVIEW_WAIT) || 15;
      const vm = trustedComments.map((c) => ({ m: VERDICT_MARK.exec(c.body || ''), at: c.created_at }))
        .filter((x) => x.m && x.m[1] === pr.headRefOid);
      const fm = trustedComments.map((c) => ({ m: FIX_MARK.exec(c.body || ''), at: c.created_at }))
        .filter((x) => x.m && x.m[2] === pr.headRefOid);
      const EXT = (e.EXT || '').trim().toLowerCase();
      const match = (s) => { s = (s || '').toLowerCase(); return EXT && s && (s.includes(EXT) || EXT.startsWith(s)); };
      const extBad = checks.filter((c) => match(c.name) && (c.bucket === 'fail' || c.bucket === 'cancel'));
      const extPending = checks.filter((c) => match(c.name) && c.bucket === 'pending');
      const lastExt = new Map();
      for (const r of extReviews.filter((r) => match(r.login))) lastExt.set(r.login, r);
      const extBlocking = extBad.length > 0 || [...lastExt.values()].some((r) => r.state === 'CHANGES_REQUESTED');
      const headAt = new Date(((pr.commits || []).slice(-1)[0] || {}).committedDate || pr.createdAt);
      const headMin = (now - headAt) / 60000;
      const verdict = vm.length ? vm[vm.length - 1].m[2] : '';
      const vMin = vm.length ? (now - new Date(vm[vm.length - 1].at)) / 60000 : Infinity;
      const fMin = fm.length ? (now - new Date(fm[fm.length - 1].at)) / 60000 : Infinity;

      // Zero-check fail-closed (issue #120): with
      // merge.require_checks a head reporting ZERO check-runs is a
      // missing signal, never a green one — held while young (the
      // rollup may still be registering), escalated once the head
      // has sat past the pending cap so a no-CI repo surfaces as
      // needs-human instead of merging blind.
      if (!checks.length && REQ_CHECKS) {
        if (headMin > Number(e.PENDING_MAX)) { action = 'escalated'; reason = 'no-checks'; }
        else { action = 'held'; reason = 'no-checks'; }
      }
      else if (missingRequired.length) {
        // Required check absent from the rollup (issue #360): the check
        // never reported — a missing signal, not a green one. Held while
        // the head is young (the run may still be registering), escalated
        // past the pending cap like the zero-checks row.
        const names = missingRequired.join(',');
        if (headMin > Number(e.PENDING_MAX)) { action = 'escalated'; reason = `required-check-missing:${names}`; }
        else { action = 'held'; reason = `required-check-missing:${names}`; }
      }
      else if (REVIEW_ON && extBlocking) {
        // The external reviewer wins over the AI verdict (fail-safe);
        // nothing re-reviews this head on its own → escalate at cap.
        if (headMin < WAIT) { action = 'held'; reason = 'external-review-blocking'; }
        else { action = 'escalated'; reason = 'external-review-blocking'; }
      }
      else if (REVIEW_ON && verdict === 'blocked') {
        // The fix pass stamps its marker at dispatch; a fix that
        // never lands escalates on the async-work cap (PENDING_MAX).
        if (fm.length && fMin < Number(e.PENDING_MAX)) { action = 'held'; reason = 'review-fix-running'; }
        else if (fm.length) { action = 'escalated'; reason = 'review-fix-stalled'; }
        else if (vMin < WAIT) { action = 'held'; reason = 'review-blocked'; }
        else { action = 'escalated'; reason = 'review-fix-not-dispatched'; }
      }
      else if (REVIEW_ON && (!verdict || extPending.length)) {
        if (headMin < WAIT) { action = 'held'; reason = !verdict ? 'review-pending' : 'external-review-pending'; }
        else { action = 'merged'; reason = `${base}+review_timeout`; }
      }
      else { action = 'merged'; reason = base; }
    }
    else { action = 'held'; reason = `mergeable=${pr.mergeable}`; }
  }
  return { action, reason };
}

function parseArgs(argv) {
  const opts = { pr: 'pr.json', checks: 'checks.json', trustedComments: 'trusted-comments.json', extReviews: 'ext-reviews.json', json: false };
  for (let i = 0; i < argv.length; i += 1) {
    const a = argv[i];
    const next = () => {
      i += 1;
      if (i >= argv.length) throw new Error(`missing value for ${a}`);
      return argv[i];
    };
    if (a === '--pr') opts.pr = next();
    else if (a === '--checks') opts.checks = next();
    else if (a === '--trusted-comments') opts.trustedComments = next();
    else if (a === '--ext-reviews') opts.extReviews = next();
    else if (a === '--json') opts.json = true;
    else if (a === '--help' || a === '-h') { console.log('node scripts/automerge-eval.js [--pr F] [--checks F] [--trusted-comments F] [--ext-reviews F] [--json]'); process.exit(0); }
    else throw new Error(`unknown argument: ${a}`);
  }
  return opts;
}

const readJson = (f, fb) => { try { return JSON.parse(fs.readFileSync(f, 'utf8')); } catch (e) { return fb; } };

function main(argv, { env } = {}) {
  const opts = parseArgs(argv);
  const e = env || process.env;
  const result = evaluateMergeGate({
    pr: readJson(opts.pr, {}),
    checks: readJson(opts.checks, []),
    trustedComments: readJson(opts.trustedComments, []),
    extReviews: readJson(opts.extReviews, []),
    env: e,
  });
  console.log(`decision: ${result.action} (${result.reason})`);
  if (e.GITHUB_OUTPUT) {
    fs.appendFileSync(e.GITHUB_OUTPUT, `action=${result.action}\nreason=${result.reason}\n`);
  }
  return result;
}

if (require.main === module) {
  try {
    main(process.argv.slice(2));
  } catch (e) {
    console.error(`automerge-eval: ${e.message}`);
    process.exit(2);
  }
}

module.exports = { evaluateMergeGate, parseArgs, main, REPAIR_MARK, VERDICT_MARK, FIX_MARK };
