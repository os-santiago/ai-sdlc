#!/usr/bin/env node
// scripts/self-heal.js — unified self-heal orchestrator (issue #187)
//
// The single sequencing module every orchestrator calls — GHA step, Tekton
// Task, or a polling cron — so the self-healing order is normative, not
// re-implemented per runtime:
//
//   extract   failure signature → canonical failure.json artifact
//             ({signature, signatureParts, rootCause, checks, baseFault}) —
//             the SAME artifact the GHA ci-repair stage persists
//   consult   signature → failure-runbook advisory (known fix injected into
//             the next repair brief; a miss adds zero noise)
//   decide    repair-stall guard + flat attempt budget →
//             'repair' | 'escalate' | 'quarantine'
//   escalate  engine-stall classification (transport|tooling|spec) →
//             retry policy + canonical postmortem (signature-deduped) +
//             optional needs-human label
//   sentinel  main-guard poll — for forges without push events this is the
//             scheduled Task that latches/releases the halt
//
// Reuses the merged primitives — never re-implements them:
//   scripts/postmortem.js          signature hash, dedup + reopen lifecycle
//   scripts/failure-runbook.js     signature → advisory (lockstep formula)
//   scripts/repair-stall-guard.js  per-signature burn count → needs-human
//   scripts/main-guard.js          base-branch sentinel latch
//   scripts/engine_adapter.js      registry exitCodeMap → class name
//   scripts/alert-router.js        failure-class → escalation route
//
// Forge-adapter seam: every issue/PR mutation goes through an injected
// client object — {listIssues, listComments, addComment, reopenIssue,
// createIssue, ensureLabel} for postmortems and an `exec(argv)` for labels
// and sentinel calls. The GHA runtime injects the `gh` implementation
// (postmortem.js ghClient / defaultExec); Tekton or other forges implement
// the same surface — nothing below calls GitHub directly.
//
// Trust boundary: trusted code — fetch/run from a pinned runtime ref,
// never from a PR head or consumer checkout (same rule as postmortem.js /
// repair-stall-guard.js). The runbook store is DATA from the base ref.
//
// Bounded: no engine, no model calls, no unbounded loops. Reads are capped
// by the primitives (≤100 issues/state, ≤300 comments/canonical); every
// phase is a pure function over already-collected inputs.
//
// CLI (one surface for any orchestrator):
//   node scripts/self-heal.js plan
//       (--failure FILE | --checks csv | --signature HEX)
//       [--markers FILE] [--attempts N] [--max-repairs N] [--stall-max N]
//       [--base-fault] [--sanitize-blocked]
//       [--runbook FILE] [--prompt-file F [--out F]] [--max-bytes N]
//       [--engine NAME --exit-code N | --outcome X] [--registry F]
//       [--json] [--github-output]
//   node scripts/self-heal.js classify
//       --engine NAME (--exit-code N | --outcome X) [--registry F] [--json]
//   node scripts/self-heal.js postmortem <postmortem.js argv…>
//   node scripts/self-heal.js sentinel --repo OWNER/NAME [--base B]
//       [--apply] [--pr N] [--json]
'use strict';

const fs = require('fs');
const path = require('path');
const { execFileSync } = require('child_process');

const postmortem = require('./postmortem');
const runbook = require('./failure-runbook');
const stallGuard = require('./repair-stall-guard');
const mainGuard = require('./main-guard');
const alertRouter = require('./alert-router');
const { EngineAdapterResolver, getEngineAdapter } = require('./engine_adapter');

const DEFAULT_RUNBOOK = runbook.DEFAULT_RUNBOOK;
const DEFAULT_MAX_REPAIRS = 3;
const DEFAULT_STALL_MAX = stallGuard.DEFAULT_MAX_STALL;

// --- Engine-stall classification (transport / tooling / spec) ------------------
// The missing piece: Hermes routes engine stalls by class — transport
// (provider/auth/network: transient, worth bounded retries), tooling
// (engine loop internals: requeue once, repeat = bug), spec (the
// task/contract layer: retrying the identical contract can never help —
// escalate immediately). The class name comes from the engine-adapter
// registry exitCodeMap — the same classification the GHA stage already
// records on its manifest — so every engine inherits it, and a Tekton
// retry policy reads `retry` verbatim.

const STALL_CLASSES = ['transport', 'tooling', 'spec'];

// Adapter exit-code class name → stall class. Case-sensitive: these are the
// registry's class names (config/engine_adapters.json exitCodeMap values).
const STALL_CLASS_MAP = {
  // transport — infrastructure between pipeline and model (transient)
  TimeoutError: 'transport',
  AgentError: 'transport',        // scc 20 — provider-class failure
  AgentFailedError: 'transport',  // scc 21 — auth/provider-class failure
  NetworkError: 'transport',
  ProviderError: 'transport',
  RateLimitError: 'transport',
  // tooling — engine loop internals (bounded requeue; repeat = engine bug)
  MaxStepsExceeded: 'tooling',
  MaxRepairsExceeded: 'tooling',
  LivelockError: 'tooling',
  EngineError: 'tooling',
  error: 'tooling',               // generic engine-error class (e.g. mimoclaw)
  // spec — task/contract layer (retrying the same contract cannot converge)
  ValidationError: 'spec',
  ContractError: 'spec',
  no_changes: 'spec',
  not_actionable: 'spec',
  blocked: 'spec',
  budget_exceeded: 'spec',
};

// Pipeline-level outcomes (no exit code) → stall class.
const OUTCOME_CLASS_MAP = {
  'no-changes': 'spec',
  'base-contract-error': 'spec',
  'contract-error': 'spec',
  'quarantined': 'spec',
  'push-rejected': 'transport',   // git race on the head — transient
  'commit-failed': 'tooling',
  'not-run': 'tooling',
};

// Tekton-agnostic retry policy per class. `retries` maps straight onto a
// Task's retry field; backoff is advisory metadata for orchestrators that
// support it.
const RETRY_POLICY = {
  transport: { retries: 3, backoff: 'exponential' },
  tooling: { retries: 1, backoff: 'none' },
  spec: { retries: 0, backoff: 'none' },
};

/**
 * Classify an engine stall from its exit code (via the adapter registry
 * exitCodeMap) or a pipeline outcome string.
 * Returns {class, className, retry, reason} — class null when the exit is
 * the engine's success class (not a stall). Unknown classes fall back to
 * 'spec' (fail-closed: an unclassified stall is never retried blind).
 */
function classifyStall({ engine, exitCode, outcome, baseFault, registryPath } = {}) {
  if (baseFault || outcome === 'base-contract-error') {
    return { class: 'spec', className: 'ContractError', retry: RETRY_POLICY.spec, reason: 'contract invalid on base ref' };
  }
  const out = outcome && String(outcome);
  if (out && OUTCOME_CLASS_MAP[out]) {
    const cls = OUTCOME_CLASS_MAP[out];
    return { class: cls, className: out, retry: RETRY_POLICY[cls], reason: `pipeline outcome ${out}` };
  }
  const m = out && out.match(/^engine-exit-(\d+)$/);
  const code = exitCode !== undefined && exitCode !== null && exitCode !== ''
    ? Number(exitCode)
    : (m ? Number(m[1]) : null);
  if (code === null || !Number.isFinite(code)) {
    return { class: null, className: '', retry: RETRY_POLICY.spec, reason: 'no exit code or outcome' };
  }
  let className = '';
  try {
    const adapter = registryPath
      ? new EngineAdapterResolver(registryPath).getEngineAdapter(engine || 'scc')
      : getEngineAdapter(engine || 'scc');
    className = adapter.classifyExitCode(code);
  } catch (e) {
    className = `exit-${code}`;
  }
  if (className === 'Success' || code === 0) {
    return { class: null, className, retry: null, reason: 'engine success class — not a stall' };
  }
  const cls = stallClassOf(className);
  return { class: cls, className, retry: RETRY_POLICY[cls], reason: `engine exit ${code} → ${className}` };
}

/** Adapter class name → stall class; unmapped → 'spec' (fail-closed). */
function stallClassOf(className) {
  return STALL_CLASS_MAP[className] || 'spec';
}

// --- extract: canonical failure-signature artifact -----------------------------
// Emits the same {signature, signatureParts, rootCause, checks, baseFault}
// the GHA ci-repair stage persists to failure.json — signature formula is
// failure-runbook.failureSignature (lockstep with the workflow prompt step).

function extractFailure({ checks, logLines, rootCause, baseFault, signature } = {}) {
  const names = Array.isArray(checks) ? checks.map(String).filter(Boolean) : [];
  const lines = Array.isArray(logLines) ? logLines : [];
  const parts = lines.length
    ? lines.map((l) => (typeof l === 'string' ? l : `${l.name || l.check || ''}:${l.line || ''}`))
    : names.map((n) => `${n}:${''}`);
  return {
    signature: signature || runbook.failureSignature(parts),
    signatureParts: parts,
    rootCause: rootCause || '',
    checks: names,
    baseFault: Boolean(baseFault),
  };
}

// --- consult: runbook advisory ---------------------------------------------------
// Reads the runbook store (base-ref DATA), matches signature → entry,
// renders the bounded advisory, and prepends it to the repair brief when a
// brief is provided. A miss returns the brief byte-identical (no noise).

function consultRunbook({ signature, text, runbookPath, maxBytes, brief } = {}) {
  let store = { entries: [] };
  let runbookError = null;
  try {
    store = runbook.parseRunbook(fs.readFileSync(runbookPath || DEFAULT_RUNBOOK, 'utf8'));
  } catch (e) {
    runbookError = e.message; // unreadable store → graceful miss
  }
  const hit = runbook.matchEntry({ signature, text: `${signature || ''}\n${text || ''}` }, store.entries);
  const advisory = hit ? runbook.buildAdvisory(hit.entry, { maxBytes }) : '';
  return {
    hit: Boolean(hit && advisory),
    matched: Boolean(hit),
    entry: hit ? hit.entry.id : null,
    via: hit ? hit.via : null,
    advisory,
    brief: brief !== undefined ? runbook.injectAdvisory(brief, advisory) : undefined,
    runbookError,
  };
}

// --- decide: bounded repair decision ---------------------------------------------
// Same precedence as the ci-repair guard step: quarantine > base-fault >
// stall-signature > flat budget → repair. `markers` are prior attempt
// markers already filtered to trusted authors by the caller.

function planRepair({ markers, checkNames, attempts, maxRepairs, stallMax, baseFault, sanitizeBlocked } = {}) {
  const atts = Number.isFinite(Number(attempts)) ? Number(attempts) : 0;
  const maxR = Number.isFinite(Number(maxRepairs)) ? Number(maxRepairs) : DEFAULT_MAX_REPAIRS;
  const stall = stallGuard.evaluateStall({ markers, checkNames, maxStall: stallMax });
  if (sanitizeBlocked) {
    return { decision: 'quarantine', reason: 'sanitize-blocked', stall, attempts: atts, maxRepairs: maxR };
  }
  if (baseFault) {
    return { decision: 'escalate', reason: 'base-contract-error', stall, attempts: atts, maxRepairs: maxR };
  }
  if (stall.escalated) {
    return { decision: 'escalate', reason: `repair-stall:${stall.signature}`, stall, attempts: atts, maxRepairs: maxR };
  }
  if (maxR > 0 && atts >= maxR) {
    return { decision: 'escalate', reason: `repairs-exhausted:${atts}/${maxR}`, stall, attempts: atts, maxRepairs: maxR };
  }
  return { decision: 'repair', reason: 'budget-available', stall, attempts: atts, maxRepairs: maxR };
}

// --- escalate: classify → postmortem → label --------------------------------------
// Postmortem filing is forge-agnostic: `client` is the forge adapter (gh
// implementation in production; tests/other forges inject theirs).
// Optional needs-human label via the stall guard's applyNeedsHuman (exec
// injectable — forges map exec to their CLI).

function escalate({ postmortemOpts, labelOpts, engine, exitCode, outcome, baseFault } = {}, client, exec) {
  const stallClass = classifyStall({ engine, exitCode, outcome, baseFault });
  const result = { stallClass, failureClass: null, postmortem: null, label: null };
  if (postmortemOpts && postmortemOpts.repo) {
    result.postmortem = postmortem.handleOccurrence(postmortemOpts, client);
    result.failureClass = alertRouter.classifyFailure({
      reason: postmortemOpts.class || outcome || '',
      signature: postmortemOpts.signatureHash,
      explicitClass: postmortemOpts.failureClass,
    });
  }
  if (labelOpts && labelOpts.repo && labelOpts.pr) {
    result.label = stallGuard.applyNeedsHuman({ ...labelOpts, exec });
  }
  return result;
}

// --- sentinel: main-guard polling Task ---------------------------------------------
// For forges without push events a scheduled Task calls this — the latch is
// the sentinel issue, so the poll is all a forge needs (no webhook).
// `apply` performs issue file/close + PR comment (mutations).

function sentinel(opts = {}, exec) {
  return mainGuard.mainGuardCheck({ ...opts, exec });
}

// --- sequence: the full pipeline in one call ---------------------------------------
// extract → consult → decide (+ classify on escalate). Returns one plan
// object — the artifact a non-GHA orchestrator persists.

function sequence({
  checks, logLines, rootCause, baseFault, signature,
  markers, attempts, maxRepairs, stallMax, sanitizeBlocked,
  runbookPath, maxBytes, brief,
  engine, exitCode, outcome,
} = {}) {
  const artifact = extractFailure({ checks, logLines, rootCause, baseFault, signature });
  const text = runbook.contextText({
    signature: artifact.signature,
    signatureParts: artifact.signatureParts,
    rootCause: artifact.rootCause,
    checks: artifact.checks,
  });
  const consulted = consultRunbook({
    signature: artifact.signature, text, runbookPath, maxBytes, brief,
  });
  const plan = planRepair({
    markers, checkNames: artifact.checks, attempts, maxRepairs, stallMax,
    baseFault: artifact.baseFault, sanitizeBlocked,
  });
  const result = { artifact, runbook: consulted, plan };
  if (plan.decision === 'escalate') {
    result.stallClass = classifyStall({ engine, exitCode, outcome, baseFault: artifact.baseFault });
    // Normalize the decision reason to the canonical failure class
    // (spec/risk-taxonomy.md) before the pattern fallback runs.
    const CANON = {
      'repair-stall': 'repair-stalled',
      'repairs-exhausted': 'repairs-exhausted',
      'base-contract-error': 'contract-error',
    };
    const head = String(plan.reason).split(':')[0];
    result.failureClass = alertRouter.classifyFailure({
      reason: plan.reason,
      explicitClass: CANON[head],
    });
  }
  return result;
}

// --- CLI ---------------------------------------------------------------------------

function usage() {
  return [
    'usage: self-heal.js <command> [opts]',
    'commands:',
    '  plan       signature → runbook → repair decision',
    '             (--failure F | --checks csv | --signature HEX)',
    '             [--markers F] [--attempts N] [--max-repairs N] [--stall-max N]',
    '             [--base-fault] [--sanitize-blocked]',
    '             [--runbook F] [--prompt-file F [--out F]] [--max-bytes N]',
    '             [--engine NAME --exit-code N | --outcome X] [--registry F]',
    '             [--json] [--github-output]',
    '  classify  engine-stall class → retry policy',
    '            --engine NAME (--exit-code N | --outcome X) [--registry F] [--json]',
    '  postmortem  delegates to scripts/postmortem.js (same argv)',
    '  sentinel  main-guard poll: --repo O/N [--base B] [--pr N] [--apply] [--json]',
  ].join('\n');
}

function parseArgs(argv) {
  const opts = { _: [] };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a.startsWith('--')) {
      const k = a.slice(2);
      if (['base-fault', 'sanitize-blocked', 'apply', 'json', 'github-output'].includes(k)) opts[k] = true;
      else opts[k] = argv[++i];
    } else opts._.push(a);
  }
  return opts;
}

function cmdPlan(a) {
  let artifact;
  if (a.failure) {
    artifact = JSON.parse(fs.readFileSync(a.failure, 'utf8'));
    if (!artifact.signature) {
      artifact = extractFailure(artifact);
    }
  } else {
    const checkNames = a.checks ? String(a.checks).split(',').map((s) => s.trim()).filter(Boolean) : [];
    artifact = extractFailure({
      checks: checkNames, signature: a.signature, baseFault: a['base-fault'],
    });
  }
  const markers = a.markers ? JSON.parse(fs.readFileSync(a.markers, 'utf8') || '[]') : [];

  let brief;
  if (a['prompt-file']) brief = fs.readFileSync(a['prompt-file'], 'utf8');

  const result = sequence({
    ...artifact,
    markers,
    attempts: a.attempts !== undefined ? Number(a.attempts) : undefined,
    maxRepairs: a['max-repairs'] !== undefined ? Number(a['max-repairs']) : undefined,
    stallMax: a['stall-max'] !== undefined ? Number(a['stall-max']) : undefined,
    sanitizeBlocked: a['sanitize-blocked'],
    runbookPath: a.runbook,
    maxBytes: a['max-bytes'] !== undefined ? Number(a['max-bytes']) : undefined,
    brief,
    engine: a.engine,
    exitCode: a['exit-code'],
    outcome: a.outcome,
  });

  if (a['prompt-file'] && result.runbook.brief !== brief) {
    const outPath = a.out || a['prompt-file'];
    fs.writeFileSync(outPath, result.runbook.brief);
    result.runbook.promptFile = outPath;
  }

  if (a['github-output'] && process.env.GITHUB_OUTPUT) {
    fs.appendFileSync(process.env.GITHUB_OUTPUT, [
      `self_heal_signature=${result.artifact.signature}`,
      `self_heal_runbook_hit=${result.runbook.hit}`,
      `self_heal_runbook_entry=${result.runbook.entry || ''}`,
      `self_heal_decision=${result.plan.decision}`,
      `self_heal_reason=${result.plan.reason}`,
      `self_heal_stall_sig=${result.plan.stall.signature}`,
      `self_heal_stall_count=${result.plan.stall.count}`,
      `self_heal_stall_class=${(result.stallClass && result.stallClass.class) || ''}`,
      `self_heal_failure_class=${result.failureClass || ''}`,
    ].join('\n') + '\n');
  }
  if (a.json || a['github-output']) console.log(JSON.stringify(result));
  else console.log(`self-heal plan: ${result.plan.decision} (${result.plan.reason}) sig=${result.artifact.signature} runbook=${result.runbook.entry || 'miss'}`);
  return result;
}

function cmdClassify(a) {
  if (!a.engine) throw new Error('classify requires --engine NAME');
  const result = classifyStall({
    engine: a.engine, exitCode: a['exit-code'], outcome: a.outcome,
    registryPath: a.registry,
  });
  console.log(a.json ? JSON.stringify(result) : `stall class: ${result.class || 'none'} (${result.reason})`);
  return result;
}

function cmdPostmortem(argvRest) {
  // Delegates to the canonical CLI — one argv surface, single implementation.
  execFileSync('node', [path.join(__dirname, 'postmortem.js'), ...argvRest], { stdio: 'inherit' });
}

function cmdSentinel(a) {
  if (!a.repo) throw new Error('sentinel requires --repo OWNER/NAME');
  const result = sentinel({
    repo: a.repo,
    base: a.base || 'main',
    pr: a.pr ? Number(a.pr) : null,
    apply: !!a.apply,
  });
  console.log(a.json ? JSON.stringify(result) : `sentinel: ${result.action} (${result.reason || result.verdict || 'n/a'})`);
  return result;
}

function main(argv = process.argv.slice(2)) {
  const [cmd, ...rest] = argv;
  if (!cmd || cmd === '--help' || cmd === '-h') { console.log(usage()); process.exit(cmd ? 0 : 2); }
  const a = parseArgs(rest);
  switch (cmd) {
    case 'plan': return cmdPlan(a);
    case 'classify': return cmdClassify(a);
    case 'postmortem': return cmdPostmortem(rest);
    case 'sentinel': return cmdSentinel(a);
    default:
      console.error(`unknown command: ${cmd}\n${usage()}`);
      process.exit(2);
  }
}

if (require.main === module) {
  try {
    main();
  } catch (e) {
    console.error(`self-heal: ${e.message}`);
    process.exit(2);
  }
}

module.exports = {
  STALL_CLASSES,
  STALL_CLASS_MAP,
  OUTCOME_CLASS_MAP,
  RETRY_POLICY,
  DEFAULT_RUNBOOK,
  DEFAULT_MAX_REPAIRS,
  DEFAULT_STALL_MAX,
  classifyStall,
  stallClassOf,
  extractFailure,
  consultRunbook,
  planRepair,
  escalate,
  sentinel,
  sequence,
  parseArgs,
  main,
};
