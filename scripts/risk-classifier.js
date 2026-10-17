#!/usr/bin/env node
// scripts/risk-classifier.js — deterministic PR risk classifier (issue #204)
//
// Canonical implementation of the spec/risk-taxonomy.md tier assignment:
// computes the risk tier of a pull request from its diff (touched paths,
// diff size, sensitive surfaces, and added-line content signals) and maps
// it to the canonical `pr:risk-low|medium|high|critical` labels. The tier
// is computed from the diff, never self-declared; ambiguity resolves UP —
// when signals straddle tiers the higher tier wins.
//
// Trust boundary: this file must only ever execute from a trusted ref
// (the runtime repo pin / base branch). A PR must never supply the
// classifier that scores its own diff — the label feeds the merge gate.
//
// Bounded: pure heuristics over bounded inputs — no engine, no model.
// `applyRiskLabel` performs a fixed, small number of `gh` calls
// (view + label create + one pr edit) so event→label latency stays well
// inside the taxonomy's labeling budget.
//
// CLI:
//   node scripts/risk-classifier.js --repo OWNER/NAME --pr N [--apply]
//   node scripts/risk-classifier.js --numstat FILE [--diff FILE] [--json]
//   node scripts/risk-classifier.js --files pulls-files.json --diff d.patch \
//        --repo OWNER/NAME --pr N --tier high --apply
//
// Inputs (any of):
//   --numstat FILE   `git diff --numstat` output
//   --files   FILE   REST pulls files array [{filename, additions, deletions, status}]
//   --diff    FILE   unified diff text — scanned for content signals
//                    (added lines only; capped by --diff-max-bytes)
//   --repo/--pr      fetch files + diff via `gh api` when not given above
// Outputs:
//   --json           print the result object to stdout
//   --github-output  append tier/why/files/lines to $GITHUB_OUTPUT
//   --apply          apply the label set via `gh` (one pr edit mutation)
//   --tier T         skip classification and use T (apply-only path)
'use strict';

const fs = require('fs');
const { execFileSync } = require('child_process');

// Optional dep-diff classifier (issue #177): when scripts/dep-update.js is
// present, a diff bounded to dependency manifests/lockfiles that carries
// only patch|minor bumps with no new direct deps takes the low-risk path —
// the taxonomy's lowest-evidence change. When the module is absent (older
// runtime pins fetch only this file) the generic path keeps dep surfaces
// high — fail-safe upward, never a silent downgrade.
let depClassifier = null;
try { depClassifier = require('./dep-update'); } catch (e) { depClassifier = null; }

const TIERS = ['low', 'medium', 'high', 'critical'];
const LABEL_PREFIX = 'pr:risk-';
// Human-owned acceptance label — the classifier never adds or removes it.
const ACCEPTED_LABEL = 'pr:risk-accepted';
const DEFAULT_DIFF_MAX_BYTES = 500000;
const MAX_LISTED_PATHS = 5000;

// --- Path signals ------------------------------------------------------------
// Ordered top-down; the first matching tier wins and ambiguity resolves UP
// (spec/risk-taxonomy.md). Kept in parity with the heuristic the review
// stage used inline before this module became canonical.
const CRIT_PATHS = [
  /^\.github\/workflows\//,
  /^actionlint$/,
  /^tools\/validate-/i,
  /^spec\//,
  /(^|\/)\.env(\.|$)/i,
  /\.(pem|key|p12|pfx|jks)$/i,
  /id_rsa|credentials|secrets?\//i,
  /auth|login|session|oauth|token/i,
  /(^|\/)migrations?\//i,
  /(^|\/)deploy|terraform|\.tf$/i,
  /payment|billing|invoice|payout/i,
  /destroy|purge|wipe|drop[-_]?table/i,
];
const HIGH_PATHS = [
  /^\.github\//,
  /(^|\/)(package(-lock)?\.json|pnpm-lock\.yaml|yarn\.lock|Cargo\.lock|go\.sum|requirements.*\.txt|pyproject\.toml|Gemfile(\.lock)?)$/i,
  /security|permission|rbac|policy|firewall|cors/i,
  /Dockerfile|docker-compose/i,
];
// Deleted files under these roots look like public-API surface removals.
const PUBLIC_API = /(^|\/)(src|lib|api|spec|contracts?)\//i;
const TEXT = /\.(md|markdown|rst|txt|adoc)$/i;
const TEST = /(^|\/)(tests?|__tests__|fixtures?)\/|\.(test|spec)\.[a-z]+$/i;
const CONF = /\.(ya?ml|json|toml|ini|cfg|conf|xml)$/i;

// --- Content signals (added diff lines only) ---------------------------------
// Credential/secret material committed to the tree → critical
// ("credential/secret handling", spec/risk-taxonomy.md).
const CRIT_CONTENT = [
  { name: 'private-key-block', re: /-----BEGIN (?:RSA |EC |OPENSSH |PGP |DSA )?PRIVATE KEY(?: BLOCK)?-----/ },
  { name: 'aws-access-key', re: /\bAKIA[0-9A-Z]{16}\b/ },
  { name: 'github-token', re: /\b(?:ghp|gho|ghu|ghs|ghr|github_pat)_[A-Za-z0-9_]{20,}\b/ },
  { name: 'slack-token', re: /\bxox[baprs]-[A-Za-z0-9-]{10,}\b/ },
  { name: 'stripe-key', re: /\b[sr]k_(?:live|test)_[A-Za-z0-9]{10,}\b/ },
  { name: 'hardcoded-secret', re: /\b(?:api[_-]?key|secret|passwd|password|private[_-]?token|access[_-]?token)\b\s*[:=]\s*['"][A-Za-z0-9_\-\/+.=]{12,}['"]/i },
];
// Dangerous execution / shell-injection shapes → high
// ("security-adjacent"; issue #204 acceptance criteria).
const HIGH_CONTENT = [
  { name: 'eval-call', re: /\beval\s*\(/ },
  { name: 'eval-string', re: /\beval\s+["'`$]/ },            // shell `eval "$cmd"`
  { name: 'new-function', re: /\bnew\s+Function\s*\(/ },     // eval-equivalent
  { name: 'exec-call', re: /\b(?:child_process\.)?exec(?:File|Sync)?\s*\(/ },
  { name: 'os-system', re: /\bos\.(?:system|popen)\s*\(/ },
  { name: 'subprocess-shell', re: /\bsubprocess\.(?:call|run|Popen|check_output|check_call)[^\n]*\bshell\s*=\s*True/ },
  { name: 'runtime-exec', re: /\bRuntime\.getRuntime\(\)\.exec\s*\(|new\s+ProcessBuilder\s*\(/ },
  { name: 'system-call', re: /\bsystem\s*\(/ },              // C/PHP/Ruby/Perl
  { name: 'sh-c', re: /\b(?:ba|z|k|fi)?sh\s+-c\b/ },         // sh -c / bash -c
  { name: 'pipe-to-shell', re: /\b(?:curl|wget)\b[^\n|]*\|\s*(?:sudo\s+)?(?:[a-z]*sh|bash|zsh)\b/ },
];

const DEFAULT_LABEL_COLOR = 'FBCA04';
const LABEL_COLOR = { critical: 'B60205', high: 'D93F0B', medium: 'FBCA04', low: '0E8A16' };

function labelFor(tier) {
  return `${LABEL_PREFIX}${tier}`;
}

// --- Input parsing -----------------------------------------------------------

// `git diff --numstat` rows: "<add>\t<del>\t<path>" ('-' for binary blobs).
function parseNumstat(text) {
  return String(text || '')
    .split('\n')
    .filter(Boolean)
    .map((line) => {
      const [a, d, ...p] = line.split('\t');
      return { path: p.join('\t'), add: +a || 0, del: +d || 0, status: 'modified' };
    })
    .filter((r) => r.path);
}

// REST pulls files array: [{filename, additions, deletions, status}].
function parseFilesJson(doc) {
  const rows = Array.isArray(doc) ? doc : [];
  return rows
    .map((r) => ({
      path: String(r.filename || r.path || ''),
      add: +r.additions || 0,
      del: +r.deletions || 0,
      status: String(r.status || 'modified'),
    }))
    .filter((r) => r.path);
}

// Added lines of a unified diff — content signals must never fire on
// removed code (`-` lines) or on `+++` headers.
function addedLines(diffText, maxBytes) {
  let text = String(diffText || '');
  const cap = Number(maxBytes) > 0 ? Number(maxBytes) : DEFAULT_DIFF_MAX_BYTES;
  if (Buffer.byteLength(text) > cap) text = text.slice(0, cap);
  return text
    .split('\n')
    .filter((l) => l.startsWith('+') && !l.startsWith('+++'))
    .map((l) => l.slice(1));
}

// --- Classification ----------------------------------------------------------

// files: [{path, add, del, status}]; diffText optional unified diff.
// Returns {tier, label, why, files, lines, dirs, paths, signals}.
function classifyDiff({ files, diffText, diffMaxBytes } = {}) {
  const rows = Array.isArray(files) ? files : [];
  const signals = [];

  if (!rows.length) {
    // No file information — nothing low-risk can be proven; per
    // ambiguity-resolves-UP, do not default to the bottom tier.
    return {
      tier: 'medium', label: labelFor('medium'), why: 'no diff file information — defaulting to medium',
      files: 0, lines: 0, dirs: [], paths: [], signals: ['no-files'],
    };
  }

  const paths = rows.map((r) => r.path);
  const lines = rows.reduce((s, r) => s + r.add + r.del, 0);
  const dirs = [...new Set(paths.map((f) => f.split('/')[0] || '.'))];

  // Content signals — added lines only.
  const added = addedLines(diffText, diffMaxBytes);
  const contentHits = (patterns) => {
    const hits = new Set();
    for (const line of added) {
      for (const p of patterns) if (p.re.test(line)) hits.add(p.name);
    }
    return [...hits];
  };
  const critContent = contentHits(CRIT_CONTENT);
  const highContent = contentHits(HIGH_CONTENT);

  const critPath = paths.find((f) => CRIT_PATHS.some((rx) => rx.test(f)));
  const highPath = paths.find((f) => HIGH_PATHS.some((rx) => rx.test(f)));
  const apiDelete = rows.find((r) => r.status === 'removed' || (r.add === 0 && r.del > 0 && PUBLIC_API.test(r.path)));

  // Dep-update fast path (issue #177): only evaluated when every touched
  // file is a dep manifest/lockfile AND the parsed diff stays inside dep
  // files. Eligibility itself is fail-closed — majors, unknown specs, new
  // direct deps and empty diffs are ineligible and fall through to the
  // generic path (dep surfaces land high there). Credential/secret and
  // exec-shape content signals run FIRST so a smuggled line inside a
  // lockfile or manifest still resolves UP.
  let depEligible = null;
  if (depClassifier && rows.length
      && rows.every((r) => depClassifier.DEP_DIFF_FILE_RE.test(r.path))) {
    const cls = depClassifier.classifyDepUpdateDiff(diffText);
    if (!cls.nonDepFiles.length) {
      const v = depClassifier.depUpdateVerdict(cls);
      if (v.eligible) depEligible = v;
    }
  }

  let tier, why;
  if (critContent.length) {
    tier = 'critical';
    why = `credential/secret material in added lines (${critContent.join(', ')})`;
    signals.push(...critContent.map((s) => `content:${s}`));
  } else if (critPath) {
    tier = 'critical';
    why = `policy/secret/supply-chain/deploy surface touched (${critPath})`;
    signals.push(`path:${critPath}`);
  } else if (highContent.length) {
    tier = 'high';
    why = `dangerous execution / injection-shaped pattern in added lines (${highContent.join(', ')})`;
    signals.push(...highContent.map((s) => `content:${s}`));
  } else if (depEligible) {
    tier = 'low';
    why = `vetted dep-only diff — ${depEligible.reason}`;
    signals.push('dep-diff', `dep:${depEligible.kind}`);
  } else if (highPath || apiDelete || lines > 800 || dirs.length >= 3) {
    tier = 'high';
    const bits = [];
    if (highPath) { bits.push(`security-adjacent/dependency surface (${highPath})`); signals.push(`path:${highPath}`); }
    if (apiDelete) { bits.push(`public-API surface deletion (${apiDelete.path})`); signals.push(`delete:${apiDelete.path}`); }
    if (lines > 800) bits.push('>800 diff lines');
    if (dirs.length >= 3) bits.push(`${dirs.length} top-level dirs (multi-subsystem)`);
    why = bits.join('; ');
  } else if (paths.every((f) => TEXT.test(f) || TEST.test(f))) {
    tier = 'low';
    why = 'docs/tests only';
  } else if (paths.every((f) => TEXT.test(f) || TEST.test(f) || CONF.test(f)) && lines <= 300) {
    tier = 'low';
    why = 'config-text additive under 300 lines';
  } else {
    tier = 'medium';
    why = 'behavioral change';
  }

  return {
    tier,
    label: labelFor(tier),
    why,
    files: paths.length,
    lines,
    dirs,
    paths: paths.slice(0, MAX_LISTED_PATHS),
    signals,
  };
}

// --- GitHub application ------------------------------------------------------

// Injectable exec for tests: fn(argv) -> stdout string (throws on failure).
function defaultExec(argv) {
  return execFileSync('gh', argv, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
}

function fetchPrFiles(repo, pr, exec) {
  const run = exec || defaultExec;
  // --paginate emits one JSON array per page; normalize to a flat list.
  const out = run([
    'api', '--paginate', `repos/${repo}/pulls/${pr}/files?per_page=100`,
    '--jq', '.[] | {filename, additions, deletions, status}',
  ]);
  return String(out)
    .split('\n')
    .filter((l) => l.trim().startsWith('{'))
    .map((l) => JSON.parse(l));
}

function fetchPrDiff(repo, pr, maxBytes, exec) {
  const run = exec || defaultExec;
  const out = run(['api', `repos/${repo}/pulls/${pr}`, '-H', 'Accept: application/vnd.github.diff']);
  const text = String(out || '');
  const cap = Number(maxBytes) > 0 ? Number(maxBytes) : DEFAULT_DIFF_MAX_BYTES;
  return Buffer.byteLength(text) > cap ? text.slice(0, cap) : text;
}

// Applies `pr:risk-<tier>` to the PR: ensures the label exists, adds it,
// and removes any stale sibling tier labels in a single edit mutation.
// `pr:risk-accepted` is human-owned and never touched.
function applyRiskLabel({ repo, pr, tier, exec }) {
  if (!TIERS.includes(tier)) throw new Error(`invalid tier '${tier}' — expected ${TIERS.join('|')}`);
  const run = exec || defaultExec;
  const label = labelFor(tier);

  const view = JSON.parse(run(['pr', 'view', String(pr), '-R', repo, '--json', 'labels']) || '{}');
  const current = (view.labels || []).map((l) => l.name);

  try {
    run(['label', 'create', label, '-R', repo,
      '--color', LABEL_COLOR[tier] || DEFAULT_LABEL_COLOR,
      '--description', `AI-SDLC risk tier: ${tier} (spec/risk-taxonomy.md)`,
      '--force']);
  } catch (e) {
    // Label already exists and cannot be updated, or the app lacks label
    // admin — the pr edit below still applies the name.
  }

  const stale = current.filter((l) => TIERS.some((t) => l === labelFor(t)) && l !== label);
  const args = ['pr', 'edit', String(pr), '-R', repo];
  if (!current.includes(label)) args.push('--add-label', label);
  for (const s of stale) args.push('--remove-label', s);
  const changed = args.length > 5;
  if (changed) run(args);

  return { label, added: current.includes(label) ? [] : [label], removed: stale, skipped: !changed };
}

// --- CLI ---------------------------------------------------------------------

function usage() {
  return [
    'usage: risk-classifier.js [--repo OWNER/NAME --pr N] [--numstat F|--files F]',
    '                          [--diff F] [--tier low|medium|high|critical]',
    '                          [--apply] [--json] [--github-output]',
    '                          [--diff-max-bytes N]',
  ].join('\n');
}

function parseArgs(argv) {
  const opts = { json: false, apply: false, githubOutput: false, diffMaxBytes: DEFAULT_DIFF_MAX_BYTES };
  for (let i = 0; i < argv.length; i += 1) {
    const a = argv[i];
    const next = () => {
      i += 1;
      if (i >= argv.length) throw new Error(`missing value for ${a}`);
      return argv[i];
    };
    if (a === '--repo') opts.repo = next();
    else if (a === '--pr') opts.pr = next();
    else if (a === '--numstat') opts.numstat = next();
    else if (a === '--files') opts.files = next();
    else if (a === '--diff') opts.diff = next();
    else if (a === '--tier') opts.tier = next();
    else if (a === '--apply') opts.apply = true;
    else if (a === '--json') opts.json = true;
    else if (a === '--github-output') opts.githubOutput = true;
    else if (a === '--diff-max-bytes') opts.diffMaxBytes = Number(next());
    else if (a === '--help' || a === '-h') { console.log(usage()); process.exit(0); }
    else throw new Error(`unknown argument: ${a}`);
  }
  return opts;
}

function main(argv, { exec, stdout } = {}) {
  const opts = parseArgs(argv);
  const emit = stdout || ((s) => console.log(s));
  let result;

  if (opts.tier) {
    if (!TIERS.includes(opts.tier)) throw new Error(`invalid --tier '${opts.tier}'`);
    result = { tier: opts.tier, label: labelFor(opts.tier), why: 'tier supplied via --tier' };
  } else {
    let rows;
    if (opts.files) rows = parseFilesJson(JSON.parse(fs.readFileSync(opts.files, 'utf8')));
    else if (opts.numstat) rows = parseNumstat(fs.readFileSync(opts.numstat, 'utf8'));
    else if (opts.repo && opts.pr) rows = parseFilesJson(fetchPrFiles(opts.repo, opts.pr, exec));
    else throw new Error(`no input: pass --numstat, --files, or --repo/--pr\n${usage()}`);

    let diffText = '';
    if (opts.diff) diffText = fs.readFileSync(opts.diff, 'utf8');
    else if (opts.repo && opts.pr) {
      try { diffText = fetchPrDiff(opts.repo, opts.pr, opts.diffMaxBytes, exec); } catch (e) { diffText = ''; }
    }
    result = classifyDiff({ files: rows, diffText, diffMaxBytes: opts.diffMaxBytes });
  }

  if (opts.apply) {
    if (!opts.repo || !opts.pr) throw new Error('--apply requires --repo and --pr');
    result.applied = applyRiskLabel({ repo: opts.repo, pr: opts.pr, tier: result.tier, exec });
  }

  if (opts.githubOutput && process.env.GITHUB_OUTPUT) {
    const lines = [
      `risk_tier=${result.tier}`,
      `risk_label=${result.label}`,
      `risk_why=${String(result.why || '').replace(/\n/g, ' ')}`,
      `diff_files=${result.files ?? ''}`,
      `diff_lines=${result.lines ?? ''}`,
    ];
    fs.appendFileSync(process.env.GITHUB_OUTPUT, lines.join('\n') + '\n');
  }

  emit(JSON.stringify(result));
  return result;
}

if (require.main === module) {
  try {
    main(process.argv.slice(2));
  } catch (e) {
    console.error(`risk-classifier: ${e.message}`);
    process.exit(2);
  }
}

module.exports = {
  TIERS,
  LABEL_PREFIX,
  ACCEPTED_LABEL,
  labelFor,
  parseNumstat,
  parseFilesJson,
  addedLines,
  classifyDiff,
  applyRiskLabel,
  fetchPrFiles,
  fetchPrDiff,
  main,
};
