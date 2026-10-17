#!/usr/bin/env node
// scripts/stall-classifier.js — engine-stall classification (issue #158)
//
// When an engine run dies mid-run the runtime must distinguish WHY before
// deciding what to do next — 'provider API flaked' (transport), 'worker /
// engine code crashed' (tooling) and 'task was impossible' (spec) each route
// to a different owner. Ported from the Hermes worker verdict cascade
// (hermes/scripts/autonomous_pr_worker.js: hermesCategory classes —
// transport, provider_quota, engine_stall, zero_mutations, worktree/config
// deaths) onto the adapter contract of config/engine_adapters.json.
//
// Failure classes and their routing (the "terminal state" recorded in run
// evidence — ai-sdlc-stall.json artifact, job summary, ci-repair manifest):
//
//   ok         run produced a result            → route: publish
//   transport  provider/network/quota death or  → route: retry — bounded
//              zero-evidence timeout               ladder (maxRetries,
//                                                   default 1); exhausted →
//                                                   needs-human
//   tooling    engine/worker crash, spawn       → route: needs-human —
//              ENOENT/EACCES, auth defect,          postmortem on the engine
//              unclassified nonzero exit            repo (sc-agent-cli)
//   spec       engine resolved no_changes /     → route: needs-refinement —
//              not_actionable / blocked, or a       back to the issue author
//              clean exit with zero mutations
//   budget     step/repair/wall-clock budget    → route: needs-refinement —
//              exhausted with real mutations       re-scope the issue or raise
//              (partial work exists)                engine.max_* in the
//                                                   contract
//   livelock   zero-mutation stall — timeout/   → route: needs-human
//              iterations burned with nothing
//              written (model narrated without
//              acting; issue #470-class)
//
// Decision order (highest precedence first — a more specific verdict always
// beats a generic one):
//   1. spawn/tooling death — spawnError (ENOENT|EACCES…) or shell-launch
//      codes 126/127: the engine never ran, retry cannot heal it.
//   2. manifest terminal resolution — when the run manifest carries a
//      semantic verdict (resolution / exit_reason / outcome / verdict) it is
//      authoritative (Hermes #382): a declared no_changes or budget
//      resolution can never be re-tagged transport by echoed prose. Only
//      'error'/unknown resolutions fall through to code+signature rules.
//   3. timeout arm — timedOut flag, kill signals, exit 124/137, or the
//      registry TimeoutError label: transport signature in the output →
//      transport; real mutations → budget (partial work); tool calls with
//      zero mutations → livelock; zero evidence → transport (a bounded
//      retry may heal a silent connect death).
//   4. registry exitCodeMap — the adapter's declared label resolves the base
//      class via LABEL_CLASS, then the adapter's optional stallClassMap
//      (per-code override) wins for codes whose pipeline label is ambiguous
//      (scc exit 20 is 'AgentError' in label space but a provider death in
//      stall space).
//   5. signature override — on a failed process only (Hermes #510: prose
//      quoting signature literals never decides death classes on a clean
//      exit): output matching the transport/quota signature table promotes a
//      generic tooling verdict to transport.
//   6. fallthrough — any other nonzero exit with no evidence → tooling
//      (an unclassified crash is an engine defect to postmortem).
//   A clean exit (registry success label) with zero mutations classifies
//   'spec' — the engine-reported no-op that routes back to the author.
//
// Mutation evidence is caller-supplied (--mutations N: dirty-worktree file
// count or non-empty patch) — the classifier never touches git itself.
// Tool-call evidence comes from --tool-calls N, the manifest's
// tool_calls_total/iterations, or a --audit JSONL scan counting
// "type":"tool_call" records (bounded read).
//
// Trust boundary: pure functions over caller-supplied artifacts — no forge
// calls, no engine, no network. Fetched from the runtime repo pin by the
// workflows, never from a PR head.
//
// CLI:
//   node scripts/stall-classifier.js --engine scc --exit-code 24 \
//     [--registry config/engine_adapters.json] [--manifest FILE] \
//     [--audit FILE] [--log FILE | --text STR] [--mutations N] \
//     [--tool-calls N] [--signal SIG] [--spawn-error ERR] [--timed-out] \
//     [--max-retries N] [--out FILE] [--field NAME] [--json] [--github-output]
'use strict';

const fs = require('fs');
const path = require('path');

const DEFAULT_REGISTRY_PATH = path.join(__dirname, '..', 'config', 'engine_adapters.json');
const DEFAULT_ENGINE = 'scc';
const DEFAULT_MAX_RETRIES = 1;
const MAX_READ_BYTES = 8 * 1024 * 1024; // bounded artifact reads
const CLASSES = ['ok', 'transport', 'tooling', 'spec', 'budget', 'livelock'];

// Routing table — every class lands on a distinct terminal state recorded in
// the run evidence. `retryable`/`maxRetries` bound the transport ladder.
const ROUTES = {
  ok: { route: 'publish', terminal: 'published', retryable: false, maxRetries: 0 },
  transport: { route: 'retry', terminal: 'needs-human', retryable: true, maxRetries: DEFAULT_MAX_RETRIES },
  tooling: { route: 'needs-human', terminal: 'engine-postmortem', retryable: false, maxRetries: 0 },
  spec: { route: 'needs-refinement', terminal: 'author-refinement', retryable: false, maxRetries: 0 },
  budget: { route: 'needs-refinement', terminal: 're-scope-or-raise-budget', retryable: false, maxRetries: 0 },
  livelock: { route: 'needs-human', terminal: 'livelock-escalation', retryable: false, maxRetries: 0 },
};

// Transport + provider-quota signature table (port of Hermes
// isTransportFailure + isProviderQuotaError — quota death is transient infra
// too, kept as a subclass of the same retryable class). Applied ONLY to a
// failed process's own output.
const TRANSPORT_RE = /connection timed out|etimedout|econn(?:reset|refused|aborted)|eai_again|fetch failed|socket hang up|http\s*5\d\d\b|upstream\s+(?:unreachable|error|fail)|provider.{0,25}(?:error|overload|unavailable|unreachable|capacity)|\boverloaded\b|bad gateway|gateway timeout|api error\s*429|http\s*429\b|\b429\b.{0,50}(?:rate|quota|limit|exhaust)|rate.?limit(?:ed)?|quota.{0,30}exhaust|exhausted their quota|resource_exhausted|insufficient_quota|insufficient.{0,15}credits?|out of (?:quota|credits)|spawn\w*\s+\S+\s+(?:enoent|eacces)|no shell is available|failed to run command/i;

const QUOTA_RE = /api error\s*429|http\s*429\b|\b429\b.{0,50}(?:rate|quota|limit|exhaust)|rate.?limit(?:ed)?|quota.{0,30}exhaust|exhausted their quota|resource_exhausted|insufficient_quota|insufficient.{0,15}credits?|out of (?:quota|credits)/i;

// Shell-launch conventions: the engine invocation itself never ran.
const SPAWN_TOOLING_CODES = new Set([126, 127]);
const SPAWN_TOOLING_ERRORS = new Set(['ENOENT', 'EACCES', 'E2BIG', 'ENOEXEC']);
const KILL_SIGNALS = new Set(['SIGKILL', 'SIGTERM', 'SIGXCPU', 'SIGINT']);

// Manifest terminal resolutions → stall class (authoritative; 'error' and
// friends fall through to code+signature rules — Hermes #382 contract).
const RESOLUTION_CLASS = {
  completed: 'ok',
  success: 'ok',
  no_changes: 'spec',
  not_actionable: 'spec',
  blocked: 'spec',
  budget_exceeded: 'budget',
  engine_stall: 'livelock',
  livelock: 'livelock',
  transport: 'transport',
  provider_quota: 'transport',
  timeout: 'timeout', // pseudo-class: resolved by evidence like TimeoutError
  crash: 'tooling',
};

// Registry exitCodeMap labels → base stall class. 'timeout' is a
// pseudo-class the evidence cascade resolves (never emitted verbatim).
const LABEL_CLASS = {
  Success: 'ok',
  completed: 'ok',
  ValidationError: 'spec',
  no_changes: 'spec',
  not_actionable: 'spec',
  blocked: 'spec',
  MaxStepsExceeded: 'budget',
  budget_exceeded: 'budget',
  MaxRepairsExceeded: 'livelock',
  TimeoutError: 'timeout',
  AgentError: 'tooling',
  AgentFailedError: 'tooling',
  error: 'tooling',
};

// --- helpers -----------------------------------------------------------------

// Registry access: the adapter's exitCodeMap + optional stallClassMap. Reads
// the raw JSON document (pure data, never evaluated); a missing/unparsable
// registry degrades to built-in label defaults — classification must not die
// with the registry.
function loadRegistry(registryPath) {
  try {
    const doc = JSON.parse(fs.readFileSync(path.resolve(registryPath), 'utf8'));
    return doc && typeof doc === 'object' && !Array.isArray(doc) ? doc : {};
  } catch {
    return {};
  }
}

function adapterFor(registry, engine) {
  const d = registry && registry[engine];
  return d && typeof d === 'object' ? d : {};
}

function registryLabel(adapter, code) {
  const map = adapter.exitCodeMap || {};
  const key = String(code);
  if (Object.prototype.hasOwnProperty.call(map, key)) return map[key];
  return typeof map.default === 'string' ? map.default : 'AgentError';
}

// Valid stallClassMap values: the emitted classes plus the 'timeout'
// pseudo-class the evidence cascade resolves.
const VALID_OVERRIDES = new Set([...CLASSES, 'timeout']);

// Per-code stall-class override (adapter stallClassMap) — wins over the
// generic label table for ambiguous codes. Unknown values are ignored:
// bad registry data degrades to the label table, never a crash.
function stallClassOverride(adapter, code) {
  const map = adapter.stallClassMap || {};
  const v = map[String(code)];
  return typeof v === 'string' && VALID_OVERRIDES.has(v) ? v : null;
}

// Extract a normalized terminal-resolution token from a run manifest:
// {resolution: 'no_changes' | {code, reason}} or exit_reason / outcome /
// verdict / status fields (first matching, normalized to snake_case).
function manifestResolution(manifest) {
  if (!manifest || typeof manifest !== 'object') return null;
  const candidates = [manifest.resolution, manifest.exit_reason, manifest.outcome, manifest.verdict, manifest.status];
  for (const c of candidates) {
    if (!c) continue;
    const raw = typeof c === 'object' ? (c.code || c.resolution || c.name) : c;
    if (!raw) continue;
    const norm = String(raw).trim().toLowerCase().replace(/[\s-]+/g, '_');
    const reason = typeof c === 'object' && c.reason ? String(c.reason) : null;
    if (norm) return { code: norm, reason };
  }
  return null;
}

// Tool-call evidence: explicit flag > manifest counters > audit JSONL count.
function toolCallCount({ toolCalls, manifest, auditText }) {
  if (Number.isFinite(toolCalls)) return toolCalls;
  if (manifest && typeof manifest === 'object') {
    for (const k of ['tool_calls_total', 'toolCalls', 'tool_calls', 'steps']) {
      if (Number.isFinite(manifest[k])) return manifest[k];
    }
    if (Number.isFinite(manifest.iterations)) return manifest.iterations;
  }
  if (auditText) {
    const m = String(auditText).match(/"type"\s*:\s*"tool_call"/g);
    if (m) return m.length;
  }
  return 0;
}

// Bounded read — never load an unbounded artifact into memory.
function readBounded(file) {
  const fd = fs.openSync(file, 'r');
  try {
    const buf = Buffer.alloc(MAX_READ_BYTES);
    const n = fs.readSync(fd, buf, 0, MAX_READ_BYTES, 0);
    return buf.subarray(0, n).toString('utf8');
  } finally {
    fs.closeSync(fd);
  }
}

// --- classifier ---------------------------------------------------------------

/**
 * Classify one engine run's terminal evidence into a stall class.
 *
 * @param {Object} evidence
 * @param {string} [evidence.engine='scc']   registry engine id
 * @param {number|string|null} exitCode      raw engine exit code (null when
 *                                           killed by signal / never ran)
 * @param {string} [signal]                  terminating signal (SIGKILL…)
 * @param {string} [spawnError]              spawn errno name (ENOENT…)
 * @param {boolean} [timedOut]               killed on a bound
 * @param {Object} [manifest]                parsed run manifest
 * @param {number} [toolCalls]               explicit tool-step count
 * @param {string} [auditText]               audit JSONL (counted, not parsed)
 * @param {string} [outputText]              engine output tail for signatures
 * @param {boolean} [mutations]              workspace produced real changes
 * @param {Object} [opts]
 * @param {Object} [opts.registry]           pre-loaded registry document
 * @param {string} [opts.registryPath]       or path to engine_adapters.json
 * @param {number} [opts.maxRetries]         transport ladder bound
 */
function classifyStall(evidence = {}, opts = {}) {
  const engine = evidence.engine || DEFAULT_ENGINE;
  const registry = opts.registry || loadRegistry(opts.registryPath || DEFAULT_REGISTRY_PATH);
  const adapter = adapterFor(registry, engine);
  const maxRetries = Number.isFinite(opts.maxRetries) ? opts.maxRetries : DEFAULT_MAX_RETRIES;

  const raw = evidence.exitCode;
  const exitCode = raw === null || raw === undefined || raw === '' ? null : Number(raw);
  const procFailed = exitCode === null || exitCode !== 0 || Boolean(evidence.signal) || Boolean(evidence.timedOut) || Boolean(evidence.spawnError);
  const manifest = evidence.manifest && typeof evidence.manifest === 'object' ? evidence.manifest : null;
  const outputText = String(evidence.outputText || '');
  const steps = toolCallCount({ toolCalls: evidence.toolCalls, manifest, auditText: evidence.auditText });
  const mutations = evidence.mutations === undefined || evidence.mutations === null
    ? null : Boolean(evidence.mutations);
  const resolution = manifestResolution(manifest);
  const label = exitCode === null ? null : registryLabel(adapter, exitCode);
  const override = exitCode === null ? null : stallClassOverride(adapter, exitCode);

  const found = (cls, reason) => {
    const r = ROUTES[cls];
    return {
      v: 1,
      engine,
      class: cls,
      reason,
      exitCode,
      signal: evidence.signal || null,
      registryClass: label,
      resolution: resolution ? resolution.code : null,
      mutations,
      toolCalls: steps,
      retryable: r.retryable,
      maxRetries: r.retryable ? maxRetries : 0,
      route: r.route,
      terminal: r.terminal,
    };
  };

  // 1. Spawn/tooling death — the engine never launched (missing binary,
  //    shell, or argv surface). A provisioning defect: no retry heals it.
  const spawnErr = evidence.spawnError && SPAWN_TOOLING_ERRORS.has(String(evidence.spawnError).toUpperCase());
  if (spawnErr || SPAWN_TOOLING_CODES.has(exitCode)) {
    return found('tooling',
      `engine could not be launched (${evidence.spawnError ? `spawn ${evidence.spawnError}` : `exit ${exitCode}`}) — provisioning defect, postmortem the engine image/repo`);
  }

  // 2. Manifest terminal resolution is authoritative (Hermes #382): a
  //    declared verdict suppresses every signal below — a budget/no_changes
  //    resolution can never read as transport by echoed prose.
  if (resolution && RESOLUTION_CLASS[resolution.code] && resolution.code !== 'error') {
    const cls = RESOLUTION_CLASS[resolution.code];
    if (cls !== 'timeout') {
      return found(cls, `manifest resolution '${resolution.code}'${resolution.reason ? ` — ${resolution.reason}` : ''}`);
    }
    // 'timeout' resolutions continue into the timeout arm below.
  }

  // 3. Timeout arm — bounded-wait kill, kill signal, shell timeout codes or
  //    the registry's TimeoutError label. Zero-mutation timeouts classify
  //    livelock-or-transport, never as a crash (issue #158 AC: distinct from
  //    tooling).
  const timeoutish = Boolean(evidence.timedOut)
    || KILL_SIGNALS.has(String(evidence.signal || ''))
    || exitCode === 124 || exitCode === 137
    || label === 'TimeoutError'
    || (resolution && RESOLUTION_CLASS[resolution.code] === 'timeout')
    || override === 'timeout';
  if (timeoutish) {
    if (procFailed && TRANSPORT_RE.test(outputText)) {
      return found('transport', `timeout with transport signature in output (exit ${exitCode ?? evidence.signal})`);
    }
    if (mutations === true) {
      return found('budget', `timed out with real workspace mutations — partial work; raise engine.max_seconds or re-scope (exit ${exitCode ?? evidence.signal})`);
    }
    if (steps > 0) {
      return found('livelock', `timed out after ${steps} tool steps with zero workspace mutations — model narrated without producing work`);
    }
    return found('transport', `timed out with no tool-call or mutation evidence — silent death, eligible for bounded retry (exit ${exitCode ?? evidence.signal})`);
  }

  // 4. Registry label → base class; the adapter's stallClassMap per-code
  //    override wins for ambiguous pipeline labels (scc 20 'AgentError' is a
  //    provider/transport death; 21 'AgentFailedError' is auth/tooling).
  let cls = override && override !== 'timeout' ? override : (LABEL_CLASS[label] || 'tooling');

  // Clean-exit verdict: success label. A measured zero-mutation clean run is
  // the engine-reported no-op (Hermes zero_mutations arm) → spec, not a
  // crash. Unmeasured mutations (null) cannot claim a no-op — stays 'ok'.
  if (cls === 'ok') {
    if (!procFailed && mutations === false) {
      return found('spec', 'engine exited cleanly with zero workspace mutations — nothing implementable in the spec as given');
    }
    return found('ok', 'engine run completed');
  }

  // 5. Signature override — prose signatures only decide death classes when
  //    the process actually failed (Hermes #510): quota/transport evidence
  //    in the output promotes a generic tooling verdict onto the retry path.
  if (procFailed && TRANSPORT_RE.test(outputText)) {
    const sub = QUOTA_RE.test(outputText) ? 'provider quota/rate-limit' : 'transport signature';
    return found('transport', `${sub} in engine output (exit ${exitCode}, registry '${label}') — transient infra death`);
  }

  // 6. Per-class reasons for the fallthrough verdicts.
  if (cls === 'spec') {
    return found('spec', `engine resolved the task without changes (exit ${exitCode}, registry '${label}') — back to the issue author`);
  }
  if (cls === 'budget') {
    return found('budget', `engine exhausted a bounded budget (exit ${exitCode}, registry '${label}')${mutations === true ? ' — partial work in tree' : ''}`);
  }
  if (cls === 'livelock') {
    return found('livelock', `engine aborted on iteration/repair bound (exit ${exitCode}, registry '${label}')${steps > 0 ? ` — ${steps} tool steps, ${mutations === true ? 'with' : 'zero'} mutations` : ''}`);
  }
  if (cls === 'transport') {
    return found('transport', `engine declared transport-class failure (exit ${exitCode}, registry '${label}') — bounded retry applies`);
  }
  return found('tooling', `engine exited ${exitCode} (registry '${label}') — unclassified crash, postmortem the engine repo`);
}

// --- CLI ---------------------------------------------------------------------

function usage() {
  return [
    'usage: stall-classifier.js [--engine ID] [--registry FILE] --exit-code N',
    '                           [--signal SIG] [--spawn-error ERR] [--timed-out]',
    '                           [--manifest FILE] [--audit FILE]',
    '                           [--log FILE | --text STR]',
    '                           [--mutations N] [--tool-calls N]',
    '                           [--max-retries N] [--out FILE]',
    '                           [--field NAME] [--json] [--github-output]',
  ].join('\n');
}

function parseArgs(argv) {
  const opts = {
    engine: DEFAULT_ENGINE,
    registry: DEFAULT_REGISTRY_PATH,
    maxRetries: DEFAULT_MAX_RETRIES,
    json: false,
    githubOutput: false,
    timedOut: false,
  };
  const next = (i) => {
    if (i + 1 >= argv.length) throw new Error(`missing value for ${argv[i]}`);
    return argv[i + 1];
  };
  for (let i = 0; i < argv.length; i += 1) {
    const a = argv[i];
    if (a === '--engine') opts.engine = next(i), i += 1;
    else if (a === '--registry') opts.registry = next(i), i += 1;
    else if (a === '--exit-code') opts.exitCode = next(i), i += 1;
    else if (a === '--signal') opts.signal = next(i), i += 1;
    else if (a === '--spawn-error') opts.spawnError = next(i), i += 1;
    else if (a === '--timed-out') opts.timedOut = true;
    else if (a === '--manifest') opts.manifestFile = next(i), i += 1;
    else if (a === '--audit') opts.auditFile = next(i), i += 1;
    else if (a === '--log') opts.logFile = next(i), i += 1;
    else if (a === '--text') opts.text = next(i), i += 1;
    else if (a === '--mutations') opts.mutations = next(i), i += 1;
    else if (a === '--tool-calls') opts.toolCalls = next(i), i += 1;
    else if (a === '--max-retries') opts.maxRetries = Number(next(i)), i += 1;
    else if (a === '--out') opts.out = next(i), i += 1;
    else if (a === '--field') opts.field = next(i), i += 1;
    else if (a === '--json') opts.json = true;
    else if (a === '--github-output') opts.githubOutput = true;
    else if (a === '--help' || a === '-h') { console.log(usage()); process.exit(0); }
    else throw new Error(`unknown argument: ${a}`);
  }
  return opts;
}

// Read a JSON file leniently (missing/corrupt → null, never throws).
function readJsonFile(file) {
  try {
    return JSON.parse(readBounded(file));
  } catch {
    return null;
  }
}

function main(argv, { stdout } = {}) {
  const opts = parseArgs(argv);
  const emit = stdout || ((s) => console.log(s));

  const auditText = opts.auditFile && fs.existsSync(opts.auditFile)
    ? readBounded(opts.auditFile) : null;
  const logText = [
    opts.logFile && fs.existsSync(opts.logFile) ? readBounded(opts.logFile) : null,
    opts.text,
  ].filter(Boolean).join('\n');
  const mutations = opts.mutations === undefined ? null
    : Number(opts.mutations) > 0 || /^(true|yes|dirty)$/i.test(String(opts.mutations));

  const result = classifyStall({
    engine: opts.engine,
    exitCode: opts.exitCode,
    signal: opts.signal,
    spawnError: opts.spawnError,
    timedOut: opts.timedOut,
    manifest: opts.manifestFile ? readJsonFile(opts.manifestFile) : null,
    toolCalls: opts.toolCalls === undefined ? undefined : Number(opts.toolCalls),
    auditText,
    outputText: logText,
    mutations,
  }, {
    registryPath: opts.registry,
    maxRetries: opts.maxRetries,
  });

  if (opts.out) fs.writeFileSync(opts.out, `${JSON.stringify(result, null, 2)}\n`);
  if (opts.githubOutput && process.env.GITHUB_OUTPUT) {
    fs.appendFileSync(process.env.GITHUB_OUTPUT,
      `stall_class=${result.class}\nstall_route=${result.route}\nstall_terminal=${result.terminal}\nstall_retryable=${result.retryable}\n`);
  }

  if (opts.field) emit(String(result[opts.field] ?? ''));
  else if (opts.json || !opts.out) emit(JSON.stringify(result));
  return result;
}

if (require.main === module) {
  try {
    main(process.argv.slice(2));
  } catch (e) {
    console.error(`stall-classifier: ${e.message}`);
    process.exit(2);
  }
}

module.exports = {
  CLASSES,
  ROUTES,
  TRANSPORT_RE,
  QUOTA_RE,
  SPAWN_TOOLING_CODES,
  SPAWN_TOOLING_ERRORS,
  KILL_SIGNALS,
  RESOLUTION_CLASS,
  LABEL_CLASS,
  DEFAULT_REGISTRY_PATH,
  DEFAULT_MAX_RETRIES,
  loadRegistry,
  adapterFor,
  registryLabel,
  stallClassOverride,
  manifestResolution,
  toolCallCount,
  classifyStall,
  parseArgs,
  main,
};
