#!/usr/bin/env node
// scripts/review-verdict.js — verdict normalization + external signal +
// adversarial merge for the review stage (issue #127)
//
// Extraction of the `Normalize verdict + external signal` heredoc from
// .github/workflows/ai-sdlc-review-run.yml. Maps the engine's raw verdict
// JSON to the canonical clean|blocked|inconclusive, merges adversarial
// pass findings BEFORE the blocked decision (issue #180 — they enter the
// same review-fix loop), and emits the sealed verdict.json record.
//
//   request_changes|blocked|reject|changes_requested → blocked
//   approve|approved|comment|clean|lgtm              → clean
//   anything else / unparseable                      → inconclusive
//   blocked = ai-blocked | any non-minor finding | external blocking
//
// CLI:
//   node scripts/review-verdict.js [--dir $WORK]
//        [--adversarial-module PATH] [--json]
// Scalars come from env (same names the step sets):
//   FINDINGS_MAX, PR, HEAD_SHA, ENGINE_CODE, ENGINE_NAME, MODEL, TIER,
//   WHY, RUN_URL, RUNNER_TEMP (contract engine lookup)
// Writes verdict.json into --dir; outputs append to $GITHUB_OUTPUT when
// set: verdict, findings_json, counts, adversarial.
'use strict';

const fs = require('fs');
const path = require('path');

const SEV = ['blocker', 'major', 'minor'];
const BLOCK_WORDS = ['request_changes', 'blocked', 'reject', 'changes_requested'];
const CLEAN_WORDS = ['approve', 'approved', 'comment', 'clean', 'lgtm'];
const FINDINGS_BYTE_CAP = 8000;

const readJson = (dir, f, fb) => { try { return JSON.parse(fs.readFileSync(path.join(dir, f), 'utf8')); } catch (e) { return fb; } };

// Bound + normalize the findings array — the record embedded in the PR
// comment and reused by the fix pass stays small.
function normalizeFindings(rawFindings, maxFindings) {
  let findings = Array.isArray(rawFindings) ? rawFindings.slice(0, maxFindings).map((f) => ({
    severity: SEV.includes(String(f.severity || '').toLowerCase()) ? String(f.severity).toLowerCase() : 'minor',
    path: String(f.path || '').slice(0, 200),
    line: Number.isFinite(+f.line) ? +f.line : 0,
    summary: String(f.summary || '').slice(0, 300),
  })) : [];
  // Bound the embedded record so downstream jobs/prompts stay small.
  while (findings.length && JSON.stringify(findings).length > FINDINGS_BYTE_CAP) findings.pop();
  return findings;
}

function verdictFor(rawVerdict, parseOk) {
  const v = String(rawVerdict || '').toLowerCase();
  let verdict;
  if (BLOCK_WORDS.includes(v)) verdict = 'blocked';
  else if (CLEAN_WORDS.includes(v)) verdict = 'clean';
  else verdict = 'inconclusive';
  // Engine gave no usable verdict — recorded, never stalls merges.
  if (!parseOk) verdict = 'inconclusive';
  return verdict;
}

// Adversarial pass (issue #180): merge its tagged findings into the record
// before the blocked decision. A skipped/unavailable pass is still
// recorded (auditable); a normalize error degrades to a recorded skip.
function mergeAdversarial({ dir, verdict, findings, maxFindings, adversarialModulePath }) {
  let advSummary = null;
  try {
    const advMod = adversarialModulePath || path.join(dir, 'scripts', 'adversarial-review.js');
    if (fs.existsSync(path.join(dir, 'adversarial.json')) && fs.existsSync(advMod)) {
      const adv = readJson(dir, 'adversarial.json', {});
      const merged = { verdict, findings, adversarial: null };
      require(advMod).mergeAdversarial({ record: merged, adv, findingsMax: maxFindings, byteCap: FINDINGS_BYTE_CAP });
      return { findings: merged.findings, advSummary: merged.adversarial, verdict: merged.verdict === 'blocked' ? 'blocked' : verdict };
    }
    if (fs.existsSync(path.join(dir, 'adversarial-plan.json'))) {
      const plan = readJson(dir, 'adversarial-plan.json', {});
      advSummary = { ran: false, reason: plan.reason || 'not-run', tier: plan.tier || null, attacks: (plan.attacks || []).length, verdict: 'none' };
    }
  } catch (e) {
    advSummary = { ran: false, reason: `normalize-error:${String((e && e.message) || e).slice(0, 80)}`, verdict: 'none' };
  }
  return { findings, advSummary, verdict };
}

function normalizeVerdict({ dir, env, adversarialModulePath } = {}) {
  const e = env || process.env;
  const ext = readJson(dir, 'external.json', {});
  let raw = {}, parseOk = false;
  const rawPath = path.join(dir, 'verdict.raw.json');
  try { raw = JSON.parse(fs.readFileSync(rawPath, 'utf8')); parseOk = true; } catch (err) {}
  const MAXF = Number(e.FINDINGS_MAX);
  let findings = normalizeFindings(raw.findings, MAXF);
  let verdict = verdictFor(raw.verdict, parseOk);

  const adv = mergeAdversarial({ dir, verdict, findings, maxFindings: MAXF, adversarialModulePath });
  findings = adv.findings;
  const advSummary = adv.advSummary;
  verdict = adv.verdict;

  const blockingFinding = findings.some((f) => f.severity !== 'minor');
  const blocked = verdict === 'blocked' || blockingFinding || ext.blocking;
  const final = blocked ? 'blocked' : verdict;
  const counts = SEV.map((s) => `${s}: ${findings.filter((f) => f.severity === s).length}`).join(' · ');
  // Engine is recorded as the adapter id (issue #230): the resolved
  // contract engine.name, else contract.json on the runner, else the
  // runtime default 'scc' (resolve.py DEFAULTS — contract unresolved).
  let contractEngine = null;
  try { contractEngine = (JSON.parse(fs.readFileSync(`${e.RUNNER_TEMP}/ai-sdlc-contract/contract.json`, 'utf8')).engine || {}).name || null; } catch (err) {}
  const engineId = e.ENGINE_NAME || contractEngine || 'scc';
  const record = {
    v: 1, pr: Number(e.PR), head_sha: e.HEAD_SHA,
    verdict: final, engine: engineId, model: e.MODEL, engine_exit: e.ENGINE_CODE,
    risk_tier: e.TIER, risk_why: e.WHY,
    adversarial: advSummary,
    findings, summary: String(raw.summary || '').slice(0, 2000),
    external: ext, run: e.RUN_URL,
  };
  const advOut = advSummary ? `${advSummary.ran ? 'ran' : 'skip'}:${advSummary.verdict}:${advSummary.attacks || 0}-attacks` : 'not-configured';
  return { record, verdict: final, aiVerdict: verdict, parseOk, counts, advOut, findings };
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
    else if (a === '--adversarial-module') opts.adversarialModule = next();
    else if (a === '--json') opts.json = true;
    else if (a === '--help' || a === '-h') { console.log('node scripts/review-verdict.js [--dir D] [--adversarial-module F] [--json]'); process.exit(0); }
    else throw new Error(`unknown argument: ${a}`);
  }
  return opts;
}

function main(argv, { env } = {}) {
  const opts = parseArgs(argv);
  const e = env || process.env;
  const W = opts.dir || e.WORK;
  if (!W) throw new Error('no work dir: pass --dir or set $WORK');
  const r = normalizeVerdict({ dir: W, env: e, adversarialModulePath: opts.adversarialModule });
  fs.writeFileSync(path.join(W, 'verdict.json'), JSON.stringify(r.record, null, 2));
  console.log(`verdict: ${r.verdict} (ai=${r.aiVerdict} parse_ok=${r.parseOk} ext_blocking=${r.record.external.blocking} adversarial=${r.advOut}) findings[${r.counts}]`);
  if (e.GITHUB_OUTPUT) {
    fs.appendFileSync(e.GITHUB_OUTPUT, [
      `verdict=${r.verdict}`,
      `findings_json=${JSON.stringify(r.findings)}`,
      `counts=${r.counts}`,
      `adversarial=${r.advOut}`,
    ].join('\n') + '\n');
  }
  if (opts.json) console.log(JSON.stringify(r.record));
  return r;
}

if (require.main === module) {
  try {
    main(process.argv.slice(2));
  } catch (e) {
    console.error(`review-verdict: ${e.message}`);
    process.exit(2);
  }
}

module.exports = { SEV, BLOCK_WORDS, CLEAN_WORDS, FINDINGS_BYTE_CAP, normalizeFindings, verdictFor, mergeAdversarial, normalizeVerdict, parseArgs, main };
