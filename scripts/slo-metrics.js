#!/usr/bin/env node
// scripts/slo-metrics.js — agentic SLO metrics over a rolling window (issue #194)
//
// Computes the pipeline SLOs the trust ladder is gated on
// (spec/trust-ladder.md § Transition criteria) purely from GitHub API data —
// no run ledger required:
//
//   time-to-merge        seconds from linked-issue creation to PR merge
//                        (fallback: PR creation, flagged basis=pr_created)
//   iterations-to-green  workflow runs on the PR head branch until the first
//                        completed run with conclusion=success (1-based;
//                        every run counts — failures, cancellations and
//                        in-progress attempts are all iterations that did
//                        not go green)
//   escalation rate      subjects holding `needs-human` / pipeline subjects
//                        observed in the window (spec/labels.md: `needs-human`
//                        is the orthogonal human hold, applied by the
//                        admission gate, the merge gate and review when a
//                        bound is exhausted)
//
// A pipeline PR is what the sweep scans: head branch matching
// ^(feat|fix)/issue-N (what ai-sdlc-implement pushes) or carrying the
// `ai-sdlc` label. A subject is a work item — the linked issue when one is
// resolvable, else the PR itself — so a `needs-human` hold on either side of
// an issue↔PR pair escalates the same subject exactly once.
//
// Read-only: this script never mutates issues, PRs or labels. The only write
// path is the local filesystem (--out / --dump-input / --github-summary).
//
// CLI:
//   node scripts/slo-metrics.js --repo OWNER/NAME [--window 30d] \
//       [--format json|markdown|prometheus] [--out FILE] [--github-summary]
//   node scripts/slo-metrics.js --input collected.json [--now ISO] ...
//
// Input contract (--input / --dump-input): the collected model is stable and
// window-agnostic — compute is pure over it, so fixtures replay deterministically.
//   {
//     "repo": "owner/name",
//     "collectedAt": "<ISO>",
//     "pullRequests": [ <gh pr list --json ...> ],
//     "issues":       [ <gh issue list --json ...> ],
//     "workflowRuns": { "<headRefName>": [ <gh run list --json ...> ] }
//   }
//
// Bounded: one `pr list`, one `issue list`, then at most --max-prs `run list`
// calls (deduped by head branch). No pagination beyond --limit/--runs-per-pr.
'use strict';

const fs = require('fs');
const { execFileSync } = require('child_process');

// Same pipeline-PR surface as ai-sdlc-sweep.yml (PIPELINE_BRANCH_RE /
// PIPELINE_LABEL) — the SLO population must match what the gate feeds on.
const PIPELINE_BRANCH_RE = /^(feat|fix)\/issue-(\d+)/;
const PIPELINE_LABEL = 'ai-sdlc';
const ESCALATION_LABEL = 'needs-human';
const SCC_LABEL_RE = /^scc:/;
// Closing-keyword reference; the branch name is checked first and wins.
const ISSUE_REF_RE = /(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s+#(\d+)/i;

const DEFAULT_WINDOW = '30d';
const DEFAULT_MAX_PRS = 100;
const MAX_PRS_CAP = 500;
const DEFAULT_RUNS_PER_PR = 100;

// Prometheus histogram buckets (seconds / iteration counts).
const TTM_BUCKETS = [3600, 21600, 86400, 259200, 604800, 2592000];
const ITERATIONS_BUCKETS = [1, 2, 3, 5, 8];

function parseDuration(s, fallbackMs) {
  const m = String(s || '').match(/^(\d+)(m|h|d)?$/);
  if (!m) return fallbackMs;
  const n = parseInt(m[1], 10);
  return n * (m[2] === 'd' ? 86400e3 : m[2] === 'h' ? 3600e3 : m[2] === 'm' ? 60e3 : 1e3);
}

function parseTs(v) {
  const t = new Date(v || 0).getTime();
  return Number.isFinite(t) ? t : null;
}

function inWindowTs(ts, startMs, endMs) {
  return ts !== null && ts >= startMs && ts < endMs;
}

function labelNames(obj) {
  return ((obj && obj.labels) || []).map((l) => (typeof l === 'string' ? l : l && l.name)).filter(Boolean);
}

function isPipelinePr(pr) {
  return PIPELINE_BRANCH_RE.test(pr.headRefName || '') || labelNames(pr).includes(PIPELINE_LABEL);
}

// Linked issue: branch `issue-N` first (canonical, what implement pushes),
// else a closing keyword in the body. Cross-reference text (`Refs #N`) does
// not bind — it does not claim to resolve the issue.
function linkedIssueNumber(pr) {
  const m = String(pr.headRefName || '').match(PIPELINE_BRANCH_RE);
  if (m) return parseInt(m[2], 10);
  const b = String(pr.body || '').match(ISSUE_REF_RE);
  return b ? parseInt(b[1], 10) : null;
}

function isMergedPr(pr) {
  return Boolean(pr.mergedAt) || String(pr.state || '').toUpperCase() === 'MERGED';
}

function isClosedUnmergedPr(pr) {
  const s = String(pr.state || '').toUpperCase();
  return (s === 'CLOSED' || Boolean(pr.closedAt)) && !isMergedPr(pr);
}

// --- statistics ---------------------------------------------------------------
function sorted(nums) {
  return [...nums].sort((a, b) => a - b);
}

function median(nums) {
  if (!nums.length) return null;
  const s = sorted(nums);
  return s.length % 2 ? s[(s.length - 1) / 2] : (s[s.length / 2 - 1] + s[s.length / 2]) / 2;
}

// Nearest-rank p90 (same convention as Hermes pipeline_slos.js).
function p90(nums) {
  if (!nums.length) return null;
  const s = sorted(nums);
  return s[Math.min(Math.floor(s.length * 0.9), s.length - 1)];
}

function round1(n) {
  return n === null || n === undefined ? null : Math.round(n * 10) / 10;
}

// --- collection (gh-backed; exec is injectable for tests) ---------------------
function defaultExec(argv) {
  return execFileSync('gh', argv, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
}

const PR_FIELDS = 'number,title,state,headRefName,headRefOid,createdAt,updatedAt,closedAt,mergedAt,isDraft,labels,body';
const ISSUE_FIELDS = 'number,title,state,createdAt,closedAt,labels';
const RUN_FIELDS = 'databaseId,createdAt,status,conclusion,event,headSha,workflowName';

function fetchPullRequests(repo, maxPrs, exec) {
  const run = exec || defaultExec;
  return JSON.parse(run([
    'pr', 'list', '-R', repo, '--state', 'all', '--limit', String(maxPrs), '--json', PR_FIELDS,
  ]));
}

function fetchIssues(repo, exec) {
  const run = exec || defaultExec;
  return JSON.parse(run([
    'issue', 'list', '-R', repo, '--state', 'all', '--limit', '500', '--json', ISSUE_FIELDS,
  ]));
}

function fetchWorkflowRuns(repo, branch, perPage, exec) {
  const run = exec || defaultExec;
  return JSON.parse(run([
    'run', 'list', '-R', repo, '--branch', branch, '--limit', String(perPage), '--json', RUN_FIELDS,
  ]));
}

// Window-active pipeline PRs only — the runs fan-out stays bounded at one
// call per distinct head branch (retried issues share subjects, not heads).
function collectData({ repo, maxPrs, runsPerPr, startMs, endMs }, exec) {
  const prs = fetchPullRequests(repo, maxPrs, exec);
  const issues = fetchIssues(repo, exec);
  const workflowRuns = {};
  const branches = new Set();
  for (const pr of prs) {
    if (!isPipelinePr(pr) || branches.has(pr.headRefName)) continue;
    const active = [parseTs(pr.createdAt), parseTs(pr.mergedAt), parseTs(pr.closedAt)]
      .some((t) => inWindowTs(t, startMs, endMs));
    if (!active) continue;
    branches.add(pr.headRefName);
    try {
      workflowRuns[pr.headRefName] = fetchWorkflowRuns(repo, pr.headRefName, runsPerPr, exec);
    } catch (e) {
      // A missing/failed runs scan must not sink the report — the PR lands in
      // the `unsampled` count so the gap is visible instead of silent.
      workflowRuns[pr.headRefName] = null;
    }
  }
  return {
    repo,
    collectedAt: new Date().toISOString(),
    pullRequests: prs,
    issues,
    workflowRuns,
  };
}

// --- compute ------------------------------------------------------------------
//
// A subject is a work item: `issue:N` when a pipeline PR resolves to an issue
// (or the issue itself is pipeline-flagged), else `pr:N`. Subject membership
// in the window requires an event timestamp inside [start, end) — created /
// merged / closed on either side of the pair. Escalated subjects are always
// included: `needs-human` is observable only as current state via the list
// APIs, so a live hold is reported even when the item predates the window.
function computeSlos(data, { startMs, endMs, windowLabel, repo } = {}) {
  const prs = ((data && data.pullRequests) || []).filter(isPipelinePr);
  const issues = new Map((((data && data.issues) || [])).map((i) => [i.number, i]));
  const runsByBranch = (data && data.workflowRuns) || {};

  const subjects = new Map(); // key -> {key, prNumbers, issues, inWindow, escalated}
  const subjectFor = (kind, n) => {
    const key = `${kind}:${n}`;
    if (!subjects.has(key)) {
      subjects.set(key, { key, kind, number: n, prs: [], issueNumbers: new Set(), inWindow: false, escalated: false });
    }
    return subjects.get(key);
  };

  for (const pr of prs) {
    const issueNum = linkedIssueNumber(pr);
    const sub = issueNum !== null ? subjectFor('issue', issueNum) : subjectFor('pr', pr.number);
    sub.prs.push(pr.number);
    if (issueNum !== null) sub.issueNumbers.add(issueNum);
    const active = [parseTs(pr.createdAt), parseTs(pr.mergedAt), parseTs(pr.closedAt)]
      .some((t) => inWindowTs(t, startMs, endMs));
    const issue = issueNum !== null ? issues.get(issueNum) : null;
    const issueActive = issue
      ? [parseTs(issue.createdAt), parseTs(issue.closedAt)].some((t) => inWindowTs(t, startMs, endMs))
      : false;
    if (active || issueActive) sub.inWindow = true;
    if (labelNames(pr).includes(ESCALATION_LABEL)) sub.escalated = true;
    if (issue && labelNames(issue).includes(ESCALATION_LABEL)) sub.escalated = true;
  }

  // Issue-side subjects with no pipeline PR: pipeline-flagged issues only —
  // a needs-human hold or an scc:* run-state label marks pipeline ownership;
  // arbitrary repo issues must not dilute the escalation denominator.
  for (const issue of issues.values()) {
    const labels = labelNames(issue);
    const flagged = labels.includes(ESCALATION_LABEL) || labels.some((l) => SCC_LABEL_RE.test(l));
    if (!flagged) continue;
    const existing = subjects.get(`issue:${issue.number}`);
    if (existing && existing.prs.length) continue; // already covered via its PR
    const sub = existing || subjectFor('issue', issue.number);
    sub.issueNumbers.add(issue.number);
    if ([parseTs(issue.createdAt), parseTs(issue.closedAt)].some((t) => inWindowTs(t, startMs, endMs))) {
      sub.inWindow = true;
    }
    if (labels.includes(ESCALATION_LABEL)) sub.escalated = true;
  }

  // A live hold joins the denominator even when nothing else landed in the
  // window — the rate can never exceed 100% on a point-in-time signal.
  const windowSubjects = [...subjects.values()].filter((s) => s.inWindow || s.escalated);
  const escalated = windowSubjects.filter((s) => s.escalated);

  // Window-active pipeline PR = created, merged or closed inside the window.
  // Same bound collectData applies before fetching runs.
  const prActiveInWindow = (pr) =>
    [parseTs(pr.createdAt), parseTs(pr.mergedAt), parseTs(pr.closedAt)]
      .some((t) => inWindowTs(t, startMs, endMs));

  // --- time-to-merge (merged pipeline PRs whose merge event is in window) ---
  const ttmSamples = [];
  for (const pr of prs) {
    const mergedTs = parseTs(pr.mergedAt);
    if (!inWindowTs(mergedTs, startMs, endMs)) continue;
    const issueNum = linkedIssueNumber(pr);
    const issue = issueNum !== null ? issues.get(issueNum) : null;
    const issueTs = issue ? parseTs(issue.createdAt) : null;
    const basis = issueTs !== null ? 'issue_created' : 'pr_created';
    const startTs = issueTs !== null ? issueTs : parseTs(pr.createdAt);
    if (startTs === null) continue;
    const seconds = Math.round((mergedTs - startTs) / 1000);
    if (seconds < 0) continue; // clock skew — never emit negative latency
    ttmSamples.push({ pr: pr.number, issue: issueNum, seconds, basis });
  }
  ttmSamples.sort((a, b) => a.pr - b.pr);
  const ttm = ttmSamples.map((s) => s.seconds);

  // --- iterations-to-green (window-active pipeline PRs only) ------------------
  const iterSamples = [];
  for (const pr of prs) {
    if (!prActiveInWindow(pr)) continue;
    const runs = runsByBranch[pr.headRefName];
    if (runs === undefined || runs === null) continue; // not sampled / fetch failed
    const ordered = [...runs].sort((a, b) => {
      const d = (parseTs(a.createdAt) || 0) - (parseTs(b.createdAt) || 0);
      return d !== 0 ? d : (a.databaseId || a.id || 0) - (b.databaseId || b.id || 0);
    });
    const greenIdx = ordered.findIndex(
      (r) => r.status === 'completed' && r.conclusion === 'success',
    );
    iterSamples.push({
      pr: pr.number,
      head: pr.headRefName,
      runs: ordered.length,
      green: greenIdx !== -1,
      // Censored samples keep the attempts observed — a PR that never went
      // green still consumed `runs` iterations of signal.
      iterations: greenIdx !== -1 ? greenIdx + 1 : ordered.length,
    });
  }
  iterSamples.sort((a, b) => a.pr - b.pr);
  const greenIters = iterSamples.filter((s) => s.green).map((s) => s.iterations);

  return {
    version: 1,
    repo: repo || (data && data.repo) || null,
    window: {
      start: new Date(startMs).toISOString(),
      end: new Date(endMs).toISOString(),
      duration: windowLabel || null,
    },
    counts: {
      pipeline_prs: prs.length,
      merged_prs: prs.filter(isMergedPr).length,
      open_prs: prs.filter((p) => String(p.state || '').toUpperCase() === 'OPEN').length,
      closed_unmerged_prs: prs.filter(isClosedUnmergedPr).length,
      window_prs: prs.filter(prActiveInWindow).length,
      subjects: windowSubjects.length,
      escalated_subjects: escalated.length,
    },
    time_to_merge_seconds: {
      count: ttm.length,
      min: ttm.length ? Math.min(...ttm) : null,
      median: median(ttm),
      p90: p90(ttm),
      mean: ttm.length ? Math.round(ttm.reduce((a, b) => a + b, 0) / ttm.length) : null,
      max: ttm.length ? Math.max(...ttm) : null,
      samples: ttmSamples,
    },
    iterations_to_green: {
      sampled_prs: iterSamples.length,
      green: greenIters.length,
      not_green: iterSamples.filter((s) => !s.green).length,
      mean: greenIters.length ? round1(greenIters.reduce((a, b) => a + b, 0) / greenIters.length) : null,
      median: greenIters.length ? round1(median(greenIters)) : null,
      p90: greenIters.length ? p90(greenIters) : null,
      max: greenIters.length ? Math.max(...greenIters) : null,
      samples: iterSamples,
    },
    escalation: {
      subjects: windowSubjects.length,
      escalated: escalated.length,
      rate: windowSubjects.length ? round1((escalated.length / windowSubjects.length) * 100) : null,
      items: escalated
        .map((s) => ({ kind: s.kind, number: s.number, prs: s.prs }))
        .sort((a, b) => a.number - b.number),
    },
  };
}

// --- renderers ----------------------------------------------------------------
function fmtSeconds(s) {
  if (s === null || s === undefined) return 'n/a';
  if (s < 3600) return `${Math.round(s / 60)}m`;
  if (s < 86400) return `${(s / 3600).toFixed(1)}h`;
  return `${(s / 86400).toFixed(1)}d`;
}

function renderMarkdown(report) {
  const ttm = report.time_to_merge_seconds;
  const it = report.iterations_to_green;
  const esc = report.escalation;
  const lines = [
    `## AI-SDLC SLO metrics — ${report.repo || 'unknown repo'} (last ${report.window.duration || 'window'})`,
    '',
    `Window: \`${report.window.start}\` → \`${report.window.end}\``,
    '',
    '| SLO | Value | Sample |',
    '|---|---|---|',
    `| time-to-merge (issue → merge) | median ${fmtSeconds(ttm.median)} · p90 ${fmtSeconds(ttm.p90)} · mean ${fmtSeconds(ttm.mean)} | ${ttm.count} merged |`,
    `| iterations-to-green | mean ${it.mean ?? 'n/a'} · median ${it.median ?? 'n/a'} · p90 ${it.p90 ?? 'n/a'} | ${it.green} green / ${it.sampled_prs} sampled |`,
    `| escalation rate (needs-human) | ${esc.rate !== null ? `${esc.rate}%` : 'n/a'} | ${esc.escalated}/${esc.subjects} subjects |`,
    '',
    `Pipeline PRs: ${report.counts.merged_prs} merged · ${report.counts.open_prs} open · ${report.counts.closed_unmerged_prs} closed unmerged — ` +
      `never green: ${it.not_green} · PRs without run data: ${report.counts.window_prs - it.sampled_prs}`,
  ];
  return lines.join('\n');
}

// Prometheus text exposition: histograms carry the full sample distribution
// (sum+count are derivable, never separate truths), escalation lands as
// counters + a ratio gauge. All series are labeled with `repo` so a fleet
// scrape can aggregate.
function renderPrometheus(report) {
  const repo = String(report.repo || 'unknown').replace(/\\/g, '\\\\').replace(/"/g, '\\"');
  const L = `repo="${repo}"`;
  const out = [];

  const histogram = (name, help, buckets, values) => {
    out.push(`# HELP ${name} ${help}`, `# TYPE ${name} histogram`);
    for (const b of buckets) {
      // Buckets ascend — count(≤b) is already cumulative.
      out.push(`${name}_bucket{${L},le="${b}"} ${values.filter((v) => v <= b).length}`);
    }
    out.push(`${name}_bucket{${L},le="+Inf"} ${values.length}`);
    out.push(`${name}_sum{${L}} ${values.reduce((a, b) => a + b, 0)}`);
    out.push(`${name}_count{${L}} ${values.length}`);
  };

  histogram(
    'ai_sdlc_time_to_merge_seconds',
    'Seconds from linked-issue creation (fallback: PR creation) to merge of a pipeline PR.',
    TTM_BUCKETS,
    report.time_to_merge_seconds.samples.map((s) => s.seconds),
  );
  histogram(
    'ai_sdlc_iterations_to_green',
    'Workflow runs on a pipeline PR head branch until the first completed run concluding success.',
    ITERATIONS_BUCKETS,
    report.iterations_to_green.samples.filter((s) => s.green).map((s) => s.iterations),
  );

  out.push(
    '# HELP ai_sdlc_pipeline_subjects_total Pipeline work items (issues and/or their PRs) observed in the window.',
    '# TYPE ai_sdlc_pipeline_subjects_total counter',
    `ai_sdlc_pipeline_subjects_total{${L}} ${report.escalation.subjects}`,
    '# HELP ai_sdlc_escalations_total Window subjects currently holding the needs-human hold label.',
    '# TYPE ai_sdlc_escalations_total counter',
    `ai_sdlc_escalations_total{${L}} ${report.escalation.escalated}`,
    '# HELP ai_sdlc_escalation_ratio Escalated subjects / window subjects (percent).',
    '# TYPE ai_sdlc_escalation_ratio gauge',
  );
  if (report.escalation.rate !== null) {
    out.push(`ai_sdlc_escalation_ratio{${L}} ${report.escalation.rate}`);
  } else {
    out.push('# no pipeline subjects in window — escalation ratio undefined');
  }
  out.push(
    '# HELP ai_sdlc_pipeline_prs_not_green Pipeline PRs sampled whose head branch never produced a successful run.',
    '# TYPE ai_sdlc_pipeline_prs_not_green gauge',
    `ai_sdlc_pipeline_prs_not_green{${L}} ${report.iterations_to_green.not_green}`,
    '# HELP ai_sdlc_pipeline_prs_total Pipeline PRs observed (any state).',
    '# TYPE ai_sdlc_pipeline_prs_total counter',
    `ai_sdlc_pipeline_prs_total{${L}} ${report.counts.pipeline_prs}`,
  );
  return out.join('\n');
}

function render(report, format) {
  if (format === 'markdown') return renderMarkdown(report);
  if (format === 'prometheus') return renderPrometheus(report);
  return JSON.stringify(report, null, 2);
}

// --- CLI ----------------------------------------------------------------------
function parseArgs(argv) {
  const opts = { format: 'json' };
  const take = (i) => argv[i + 1];
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a === '--repo') opts.repo = take(i++);
    else if (a === '--input') opts.input = take(i++);
    else if (a === '--dump-input') opts.dumpInput = take(i++);
    else if (a === '--window') opts.window = take(i++);
    else if (a === '--now') opts.now = take(i++);
    else if (a === '--max-prs') opts.maxPrs = parseInt(take(i++), 10);
    else if (a === '--runs-per-pr') opts.runsPerPr = parseInt(take(i++), 10);
    else if (a === '--format') opts.format = take(i++);
    else if (a === '--out') opts.out = take(i++);
    else if (a === '--github-summary') opts.githubSummary = true;
    else throw new Error(`unknown argument: ${a}`);
  }
  if (!['json', 'markdown', 'prometheus'].includes(opts.format)) {
    throw new Error(`--format must be json|markdown|prometheus (got '${opts.format}')`);
  }
  return opts;
}

function main(argv, { exec, stdout, env } = {}) {
  const opts = parseArgs(argv || []);
  const emit = stdout || ((s) => console.log(s));
  const environ = env || process.env;

  const repo = opts.repo || environ.GITHUB_REPOSITORY;
  const windowLabel = opts.window || DEFAULT_WINDOW;
  const windowMs = parseDuration(windowLabel, 30 * 86400e3);
  const endMs = opts.now ? Date.parse(opts.now) : Date.now();
  if (!Number.isFinite(endMs)) throw new Error(`--now is not a valid timestamp: ${opts.now}`);
  const startMs = endMs - windowMs;

  let data;
  if (opts.input) {
    data = JSON.parse(fs.readFileSync(opts.input, 'utf8'));
  } else {
    if (!repo) throw new Error('--repo is required (or set GITHUB_REPOSITORY) outside --input mode');
    data = collectData({
      repo,
      maxPrs: Math.min(opts.maxPrs || DEFAULT_MAX_PRS, MAX_PRS_CAP),
      runsPerPr: opts.runsPerPr || DEFAULT_RUNS_PER_PR,
      startMs,
      endMs,
    }, exec);
  }
  if (repo && !data.repo) data.repo = repo;
  if (opts.dumpInput) fs.writeFileSync(opts.dumpInput, `${JSON.stringify(data, null, 2)}\n`);

  const report = computeSlos(data, { startMs, endMs, windowLabel, repo: data.repo });
  const text = render(report, opts.format);

  if (opts.out) fs.writeFileSync(opts.out, `${text}\n`);
  else emit(text);

  if (opts.githubSummary && environ.GITHUB_STEP_SUMMARY) {
    fs.appendFileSync(environ.GITHUB_STEP_SUMMARY, `${renderMarkdown(report)}\n\n`);
  }
  return report;
}

if (require.main === module) {
  try {
    main(process.argv.slice(2));
  } catch (e) {
    console.error(`slo-metrics: ${e.message}`);
    process.exit(2);
  }
}

module.exports = {
  PIPELINE_BRANCH_RE,
  PIPELINE_LABEL,
  ESCALATION_LABEL,
  TTM_BUCKETS,
  ITERATIONS_BUCKETS,
  parseDuration,
  isPipelinePr,
  linkedIssueNumber,
  collectData,
  fetchPullRequests,
  fetchIssues,
  fetchWorkflowRuns,
  computeSlos,
  renderMarkdown,
  renderPrometheus,
  render,
  parseArgs,
  main,
};
