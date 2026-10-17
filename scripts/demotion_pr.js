#!/usr/bin/env node
// scripts/demotion_pr.js — escalation-driven autonomy demotion PR
// generator (issue #209, epic #151 trust machinery).
//
// Counterpart of scripts/promotion_pr.js (#202): when the number of
// escalations inside the rolling window exceeds the configured
// `demotion.escalation_threshold` in `.ai-sdlc.yaml`, this module opens
// the contract PR that lowers `autonomy.level` one rung on the
// spec/trust-ladder.md ladder. The diff touches ONLY the `level:` value —
// every other byte of the contract is preserved verbatim.
//
// Demotion semantics (spec/trust-ladder.md §Demotion and freeze):
// breaching the trigger demotes ONE level and the notice must name the
// bound that was crossed. Unlike promotion (a privilege grant the
// pipeline proposes to itself), demotion is the conservative direction —
// still a human-reviewed PR, never a silent contract mutation.
//
// Escalation counting is delegated to the #201 primitives in
// scripts/promotion-evidence.js (label bindings + record normalization);
// a precomputed verdict can also be injected (`--evidence FILE` /
// `--evidence-json`) for the credential-isolated post-script pattern —
// the compute step needs no forge write credentials, only the filing
// step does.
//
// Trigger rule (issue #209 AC): escalationCount > threshold inside the
// last `window` merged runs — equality does NOT trigger.
//
// CLI (default is DRY-RUN — proves the PR payload without touching git or
// the GitHub API; pass --apply to mutate):
//
//   node scripts/demotion_pr.js (--repo OWNER/NAME | --merges FILE) \
//     [--contract .ai-sdlc.yaml] [--base main] [--branch NAME] \
//     [--window N] [--threshold N] [--escalation-label L ...] \
//     [--escalations FILE] [--limit N] \
//     [--evidence FILE | --evidence-json JSON] \
//     [--apply] [--json] [--github-output]
//
// Exit: 0 = ran cleanly (demotion PR opened / dry-run / not triggered /
// at floor — inspect the JSON verdict), 2 = usage/error.
'use strict';

const fs = require('fs');
const path = require('path');

const promotion = require('./promotion_pr');
const pev = require('./promotion-evidence');

// Canonical ladder — normative order of spec/trust-ladder.md, shared with
// the promotion generator (single source for the rung sequence).
const LADDER = promotion.LADDER;
const DEFAULT_LEVEL = promotion.DEFAULT_LEVEL;
const CONTRACT_PATH = promotion.CONTRACT_PATH;
const PR_TITLE_PREFIX = 'fix(demotion): lower autonomy level to ';

// Schema defaults (spec/ai-sdlc.schema.json `demotion:` block).
const DEFAULT_WINDOW = 30;
const DEFAULT_THRESHOLD = 1;
const DEFAULT_ESCALATION_LABELS = pev.DEFAULT_ESCALATION_LABELS;

// --- Ladder ------------------------------------------------------------------

// Previous rung below `current`, or null when already at the floor
// (`shadow` — there is nothing lower to demote to).
function prevLevel(current) {
  const i = LADDER.indexOf(current);
  if (i === -1) {
    throw new Error(`unknown autonomy.level '${current}' — expected one of ${LADDER.join('|')}`);
  }
  return i > 0 ? LADDER[i - 1] : null;
}

function branchNameFor(next) {
  return `fix/demotion-autonomy-${next}`;
}

function buildPrTitle(next) {
  return `${PR_TITLE_PREFIX}${next}`;
}

// --- Contract demotion block -------------------------------------------------
//
// Reads `demotion.escalation_threshold` / `demotion.window` out of the
// contract with the same line-surgery discipline as the autonomy reader —
// no YAML round-trip. A top-level (column-0) non-comment line ends the
// block. Missing keys fall back to the schema defaults; a present-but-
// non-integer value throws (a corrupt trigger is a bug, not a default).
function readDemotionConfig(yamlText) {
  const lines = String(yamlText).split('\n');
  let start = -1;
  for (let i = 0; i < lines.length; i += 1) {
    if (/^demotion\s*:/.test(lines[i])) { start = i; break; }
  }
  const cfg = { escalation_threshold: DEFAULT_THRESHOLD, window: DEFAULT_WINDOW };
  if (start === -1) return cfg;

  for (let i = start + 1; i < lines.length; i += 1) {
    const line = lines[i];
    if (/^[^\s#]/.test(line)) break; // next top-level key
    const m = line.match(/^\s+(escalation_threshold|window)\s*:\s*(\d+)/);
    if (m) cfg[m[1]] = Number(m[2]);
    else if (/^\s+(escalation_threshold|window)\s*:/.test(line)) {
      throw new Error(`unparseable demotion.${line.trim().split(':')[0].trim()} line: ${line.trim()}`);
    }
  }
  return cfg;
}

// --- Escalation evaluation ---------------------------------------------------
//
// records: newest-first merge records (same shape as promotion-evidence:
// {number, mergedAt, labels, escalated?, checksPassed?}). Returns
//   {triggered, window, threshold, examined, escalationCount,
//    escalations, escalationLabels, reasons}
// Triggered iff escalationCount > threshold — the strict bound from the
// issue AC and spec/trust-ladder.md's ">" demotion trigger.
function evaluateDemotionEvidence(records, opts = {}) {
  const window = Number.isInteger(opts.window) && opts.window > 0 ? opts.window : DEFAULT_WINDOW;
  const threshold = Number.isInteger(opts.threshold) && opts.threshold >= 0
    ? opts.threshold
    : DEFAULT_THRESHOLD;
  const escalationLabels = Array.isArray(opts.escalationLabels) && opts.escalationLabels.length
    ? opts.escalationLabels
    : DEFAULT_ESCALATION_LABELS;

  const list = Array.isArray(records) ? records.slice(0, window) : [];
  const windowed = list.map((r) => pev.normalizeRecord(r, escalationLabels));

  const escalations = windowed
    .filter((r) => r.escalated)
    .map((r) => ({
      number: r.number,
      mergedAt: r.mergedAt,
      labels: r.labels.filter((l) => escalationLabels.includes(l)),
    }));

  // Extra escalation records already window-scoped by the caller
  // (run-ledger escalations that never carried the label on the PR).
  const extra = Array.isArray(opts.escalations) ? opts.escalations : [];
  for (const e of extra) {
    escalations.push({
      number: e && (e.number ?? e.issue ?? e.pr) !== undefined ? (e.number ?? e.issue ?? e.pr) : null,
      labels: pev.labelNames(e).filter((l) => escalationLabels.includes(l)),
    });
  }

  const escalationCount = escalations.length;
  const triggered = escalationCount > threshold;

  const reasons = [];
  if (triggered) {
    reasons.push(
      `${escalationCount} escalation(s) inside the last ${window} merged run(s) ` +
      `exceeds demotion.escalation_threshold=${threshold}` +
      ` (${escalations.map((e) => `#${e.number}`).join(', ')})`,
    );
  } else if (!windowed.length && !extra.length) {
    reasons.push('empty window — no merge evidence, no demotion');
  } else {
    reasons.push(
      `${escalationCount} escalation(s) inside the last ${window} merged run(s) ` +
      `does not exceed demotion.escalation_threshold=${threshold}`,
    );
  }

  return {
    triggered,
    window,
    threshold,
    examined: windowed.length,
    escalationCount,
    escalations,
    escalationLabels,
    reasons,
  };
}

// --- Evidence rendering ------------------------------------------------------

// Demotion evidence as a PR-body-friendly payload for the shared
// renderer: summary sentence, metric table, escalated refs.
function demotionEvidencePayload({ evidence, window, threshold }) {
  const ev = evidence && typeof evidence === 'object' ? evidence : null;
  const count = ev && Number.isInteger(ev.escalationCount) ? ev.escalationCount : null;
  const w = ev && Number.isInteger(ev.window) ? ev.window : window;
  const t = ev && Number.isInteger(ev.threshold) ? ev.threshold : threshold;
  const payload = {
    summary: count !== null
      ? `${count} escalation(s) inside the last ${w} merged run(s) exceeded ` +
        `\`demotion.escalation_threshold\` (${t}) — spec/trust-ladder.md demotion trigger.`
      : 'Caller-asserted demotion trigger — see attached evidence.',
    metrics: {
      escalations_in_window: count !== null ? count : 'n/a',
      escalation_threshold: t,
      window: w,
      examined: ev && Number.isInteger(ev.examined) ? ev.examined : 'n/a',
    },
  };
  if (ev && Array.isArray(ev.escalations) && ev.escalations.length) {
    payload.escalated = ev.escalations;
  }
  if (ev && Array.isArray(ev.reasons)) payload.reasons = ev.reasons;
  return payload;
}

function buildPrBody({ current, next, evidence }) {
  return [
    `## Demotion notice — \`${current}\` → \`${next}\``,
    '',
    'Escalation evidence crossed a `spec/trust-ladder.md` demotion trigger',
    'for this repo. Automatic demotion drops **one level**; this PR lowers',
    'the `autonomy.level` ceiling in `.ai-sdlc.yaml` and records which',
    'bound was crossed. A maintainer still reviews and merges the demotion.',
    '',
    '### Evidence',
    '',
    promotion.formatEvidence(evidence),
    '',
    '### Scope',
    '',
    `- Changes **only** \`autonomy.level\` (\`${current}\` → \`${next}\`) — no other`,
    '  field, comment, or byte of the contract is touched.',
    '- Re-earning the rung requires the promotion gate to pass on fresh',
    '  evidence; a human may also lower the level further at any time.',
    '',
    '---',
    '<sub>Generated by `scripts/demotion_pr.js` — epic #151 trust machinery.</sub>',
    '',
  ].join('\n');
}

// --- Planning ----------------------------------------------------------------

// Pure function over the contract text + evidence verdict. Returns:
//   { demotable: true, current, next, title, body, contract, branch }
//   { demotable: false, reason, current }
// `evidence.triggered === false` is a clean no-op verdict (the steady
// state — not an error). An evidence payload without an explicit
// `triggered` flag is treated as caller-asserted, mirroring the promotion
// generator's contract.
function planDemotion({ contractText, evidence, window, threshold, branch } = {}) {
  if (contractText == null) throw new Error('planDemotion requires contractText');

  const { level: current } = promotion.readAutonomyLevel(contractText);

  if (evidence && typeof evidence === 'object' && evidence.triggered === false) {
    return {
      demotable: false,
      reason: `demotion trigger not crossed — ${(evidence.reasons || [])[0] || 'no bound exceeded'}`,
      current,
      next: null,
    };
  }

  const next = prevLevel(current);
  if (next === null) {
    return {
      demotable: false,
      reason: `already at minimum autonomy level '${current}'`,
      current,
      next: null,
    };
  }

  const body = buildPrBody({
    current,
    next,
    evidence: demotionEvidencePayload({ evidence, window, threshold }),
  });

  return {
    demotable: true,
    current,
    next,
    title: buildPrTitle(next),
    body,
    contract: promotion.setAutonomyLevel(contractText, next),
    branch: branch || branchNameFor(next),
  };
}

// --- PR creation (injectable layer, shared with promotion) -------------------

// The gh/git orchestration is identical to the promotion generator —
// clean-worktree guard → branch → write the single-field contract →
// commit → push → `gh pr create`. Delegated verbatim so both paths share
// one implementation; `exec`, `git` and `cwd` stay injectable for tests.
function openDemotionPr(opts) {
  return promotion.openPromotionPr(opts);
}

function applyDemotion(opts) {
  return promotion.applyPromotion(opts);
}

// --- CLI ---------------------------------------------------------------------

function usage() {
  return [
    'usage: demotion_pr.js (--repo OWNER/NAME | --merges FILE | --evidence* ...)',
    '                      [--contract .ai-sdlc.yaml] [--base main] [--branch NAME]',
    '                      [--window N] [--threshold N] [--escalation-label L ...]',
    '                      [--escalations FILE] [--limit N]',
    '                      [--apply] [--json] [--github-output]',
    '',
    'Default mode is a dry-run: prints the demotion verdict and the PR',
    'payload (title/body/branch/diff) without touching git or the GitHub',
    'API. --apply creates the branch, commits the single-field contract',
    'demotion, pushes, and opens the PR.',
  ].join('\n');
}

function parseArgs(argv) {
  const opts = {
    json: false, apply: false, githubOutput: false,
    contract: CONTRACT_PATH, base: 'main',
    escalationLabels: [],
  };
  for (let i = 0; i < argv.length; i += 1) {
    const a = argv[i];
    const next = () => {
      i += 1;
      if (i >= argv.length) throw new Error(`missing value for ${a}`);
      return argv[i];
    };
    if (a === '--repo') opts.repo = next();
    else if (a === '--merges') opts.merges = next();
    else if (a === '--escalations') opts.escalationsFile = next();
    else if (a === '--window') opts.window = Number(next());
    else if (a === '--threshold') opts.threshold = Number(next());
    else if (a === '--limit') opts.limit = Number(next());
    else if (a === '--escalation-label') opts.escalationLabels.push(next());
    else if (a === '--contract') opts.contract = next();
    else if (a === '--base') opts.base = next();
    else if (a === '--branch') opts.branch = next();
    else if (a === '--evidence') opts.evidenceFile = next();
    else if (a === '--evidence-json') opts.evidenceJson = next();
    else if (a === '--apply') opts.apply = true;
    else if (a === '--json') opts.json = true;
    else if (a === '--github-output') opts.githubOutput = true;
    else if (a === '--help' || a === '-h') { console.log(usage()); process.exit(0); }
    else throw new Error(`unknown argument: ${a}`);
  }
  return opts;
}

// Loads an injected verdict (--evidence FILE / --evidence-json). A file
// that does not parse as JSON is a caller error here — unlike the
// promotion generator there is no free-form-text verdict for a trigger
// decision; the post-script contract is structured.
function loadEvidence(opts) {
  if (opts.evidenceFile) return JSON.parse(fs.readFileSync(opts.evidenceFile, 'utf8'));
  if (opts.evidenceJson != null) return JSON.parse(opts.evidenceJson);
  return null;
}

function main(argv, { exec, git, stdout, cwd } = {}) {
  const opts = parseArgs(argv);
  const emit = stdout || ((s) => console.log(s));
  const contractPath = path.join(cwd || '.', opts.contract);
  const contractText = fs.readFileSync(contractPath, 'utf8');

  // Contract-declared triggers are the defaults; CLI flags override.
  const cfg = readDemotionConfig(contractText);
  const window = Number.isInteger(opts.window) && opts.window > 0 ? opts.window : cfg.window;
  const threshold = Number.isInteger(opts.threshold) && opts.threshold >= 0
    ? opts.threshold
    : cfg.escalation_threshold;

  let evidence = loadEvidence(opts);
  if (evidence == null) {
    let records;
    if (opts.merges) {
      const doc = JSON.parse(fs.readFileSync(opts.merges, 'utf8'));
      if (!Array.isArray(doc)) throw new Error('--merges file must contain a JSON array');
      records = doc;
    } else if (opts.repo) {
      records = pev.fetchMergedPrs(opts.repo, opts.limit || window, exec);
    } else {
      throw new Error(`no input: pass --repo, --merges, or --evidence*\n${usage()}`);
    }
    let extraEscalations = [];
    if (opts.escalationsFile) {
      const doc = JSON.parse(fs.readFileSync(opts.escalationsFile, 'utf8'));
      if (!Array.isArray(doc)) throw new Error('--escalations file must contain a JSON array');
      extraEscalations = doc;
    }
    evidence = evaluateDemotionEvidence(records, {
      window,
      threshold,
      escalationLabels: opts.escalationLabels.length ? opts.escalationLabels : undefined,
      escalations: extraEscalations,
    });
  }

  const plan = planDemotion({ contractText, evidence, window, threshold, branch: opts.branch });
  if (!plan.demotable) {
    const out = {
      demoted: false, reason: plan.reason, current: plan.current, evidence,
    };
    if (opts.githubOutput && process.env.GITHUB_OUTPUT) {
      fs.appendFileSync(process.env.GITHUB_OUTPUT, 'demotion_triggered=false\n');
    }
    emit(JSON.stringify(out, null, opts.json ? 2 : 0));
    return out;
  }

  const result = applyDemotion({
    repo: opts.repo,
    base: opts.base,
    branch: plan.branch,
    contractPath: opts.contract,
    contractText: plan.contract,
    title: plan.title,
    body: plan.body,
    exec, git, cwd,
    dryRun: !opts.apply,
  });

  const out = {
    demoted: !result.dryRun,
    dryRun: Boolean(result.dryRun),
    current: plan.current,
    next: plan.next,
    branch: plan.branch,
    title: plan.title,
    body: plan.body,
    diff: promotion.diffPreview(opts.contract, contractText, plan.contract),
    evidence,
    pr: result.pr,
    steps: result.steps,
  };

  if (opts.githubOutput && process.env.GITHUB_OUTPUT) {
    const lines = [
      'demotion_triggered=true',
      `demotion_level=${plan.next}`,
      `demotion_branch=${plan.branch}`,
      `demotion_dry_run=${result.dryRun}`,
    ];
    if (result.pr && result.pr.url) lines.push(`demotion_pr_url=${result.pr.url}`);
    fs.appendFileSync(process.env.GITHUB_OUTPUT, lines.join('\n') + '\n');
  }

  emit(JSON.stringify(out, null, opts.json ? 2 : 0));
  return out;
}

if (require.main === module) {
  try {
    main(process.argv.slice(2));
  } catch (e) {
    console.error(`demotion_pr: ${e.message}`);
    process.exit(2);
  }
}

module.exports = {
  LADDER,
  DEFAULT_LEVEL,
  CONTRACT_PATH,
  PR_TITLE_PREFIX,
  DEFAULT_WINDOW,
  DEFAULT_THRESHOLD,
  DEFAULT_ESCALATION_LABELS,
  prevLevel,
  branchNameFor,
  buildPrTitle,
  readDemotionConfig,
  evaluateDemotionEvidence,
  demotionEvidencePayload,
  buildPrBody,
  planDemotion,
  openDemotionPr,
  applyDemotion,
  parseArgs,
  loadEvidence,
  main,
};
