#!/usr/bin/env node
// scripts/dora-metrics.js — DORA metrics for the agentic pipeline
// (issues #101/#102, epic #88).
//
// Computes the four DORA metrics over a rolling window purely from GitHub
// API data — no run ledger required — each split by authorType
// (agentic vs human), per the epic definitions:
//
//   deployment frequency  merges to the base branch inside the window
//                         (release tags tracked as a secondary counter —
//                         too sparse for DF on CLI/library repos)
//   lead time for changes `ready-to-implement` label applied → merge
//                         (timeline API; fallback: PR creation,
//                         flagged basis=pr_created)
//   change failure rate   merges followed by a red main-branch run
//                         attributable to the merge commit, a merged
//                         revert PR, or a non-converging repair (linked
//                         subject still holding `needs-human`/`scc:failed`,
//                         or the attributed red episode never recovered)
//                         — inside --failure-window
//   MTTR                  base-branch red run → next completed green run,
//                         attributed to the merge that introduced the red
//
// A pipeline ("agentic") PR is the sweep surface: head branch matching
// ^(feat|fix)/issue-N, the `ai-sdlc` label, or an author login in
// --agentic-authors (default: ai-sdlc-runtime[bot]). Everything else is
// human — the same-CI same-review apples-to-apples baseline of epic #88.
//
// Read-only: this script never mutates issues, PRs or labels. The only
// write paths are the local filesystem (--out / --report / --dump-input),
// $GITHUB_STEP_SUMMARY (--github-summary) and $GITHUB_OUTPUT
// (--github-output).
//
// CLI:
//   node scripts/dora-metrics.js --repo OWNER/NAME [--base main] \
//       [--window 30d] [--failure-window 48h] [--format json|markdown] \
//       [--out dora-metrics.json] [--report docs/metrics/dora.md] \
//       [--github-summary] [--github-output]
//   node scripts/dora-metrics.js --input collected.json [--now ISO] ...
//   node scripts/dora-metrics.js --input dora-metrics.json --format markdown
//
// Input contract (--input / --dump-input): either the collected model —
// window-agnostic, compute is pure over it so fixtures replay
// deterministically —
//   {
//     "repo": "owner/name",
//     "collectedAt": "<ISO>",
//     "pullRequests": [ <gh pr list --json ...> ],
//     "issues":       [ <gh issue list --json ...> ],
//     "mainRuns":     [ <gh run list --branch BASE --json ...> ],
//     "releases":     [ <gh release list --json ...> ] | null,
//     "readyAt":      { "issue:5": "<ISO>", "pr:12": "<ISO>" | null }
//   }
// or an already-computed report (the --out JSON), which --format markdown
// renders straight into docs/metrics/dora.md — the "JSON artifact →
// markdown report" hand-off of issue #102.
//
// Bounded: one `pr list`, one `issue list`, one `run list`, one
// `release list`, then at most one timeline call per merged-in-window PR
// (--max-prs). No pagination beyond --limit/--runs/--timeline-limit.
'use strict';

const fs = require('fs');
const path = require('path');
const { execFileSync } = require('child_process');

// Same pipeline-PR surface as ai-sdlc-sweep / slo-metrics.js — the
// agentic population must match what the gate feeds on.
const PIPELINE_BRANCH_RE = /^(feat|fix)\/issue-(\d+)/;
const PIPELINE_LABEL = 'ai-sdlc';
const READY_LABEL = 'ready-to-implement';
const ESCALATION_LABEL = 'needs-human';
const STALLED_RUN_LABEL = 'scc:failed';
// Closing-keyword reference; the branch name is checked first and wins.
const ISSUE_REF_RE = /(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s+#(\d+)/i;
// Revert detection: GitHub's own "Revert" UI flow produces titles of the
// form `Revert "<title>"`, head branches `revert-N-<head>` and bodies
// `Reverts #N` — any of the three binds the revert to its target.
const REVERT_TITLE_RE = /^revert\b/i;
const REVERT_BRANCH_RE = /^revert-(\d+)(-|$)/i;
const ISSUE_NUM_RE = /#(\d+)/g;

const RED_CONCLUSIONS = new Set(['failure', 'cancelled', 'timed_out', 'startup_failure']);
const DEFAULT_AGENTIC_AUTHORS = 'ai-sdlc-runtime[bot]';

const DEFAULT_WINDOW = '30d';
const DEFAULT_BASE = 'main';
const DEFAULT_FAILURE_WINDOW = '48h';
const DEFAULT_MAX_PRS = 200;
const MAX_PRS_CAP = 500;
const DEFAULT_RUNS = 200;
const MAX_RUNS_CAP = 500;
const DEFAULT_RELEASES = 50;
const MAX_RELEASES_CAP = 100;
// Markdown sample tables stay bounded — a 90d window on a busy repo must
// not produce an unreadable wall of rows.
const MD_SAMPLE_CAP = 50;

// --- small helpers -------------------------------------------------------------
function parseDuration(s, fallbackMs) {
  const m = String(s || '').match(/^(\d+)(m|h|d)?$/);
  if (!m) return fallbackMs;
  const n = parseInt(m[1], 10);
  return n * (m[2] === 'd' ? 86400e3 : m[2] === 'h' ? 3600e3 : m[2] === 'm' ? 60e3 : 1e3);
}

function parseCsvList(s) {
  return String(s || '').split(',').map((x) => x.trim()).filter(Boolean);
}

function parseTs(v) {
  if (v === null || v === undefined || v === '') return null;
  const t = new Date(v).getTime();
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

function authorLogin(pr) {
  return (pr && pr.author && pr.author.login) || '';
}

// Linked issue: branch `issue-N` first (canonical, what implement pushes),
// else a closing keyword in the body. `Refs #N` does not bind.
function linkedIssueNumber(pr) {
  const m = String(pr.headRefName || '').match(PIPELINE_BRANCH_RE);
  if (m) return parseInt(m[2], 10);
  const b = String(pr.body || '').match(ISSUE_REF_RE);
  return b ? parseInt(b[1], 10) : null;
}

function isMergedPr(pr) {
  return Boolean(pr.mergedAt) || String(pr.state || '').toUpperCase() === 'MERGED';
}

function mergeSha(pr) {
  return (pr && pr.mergeCommit && pr.mergeCommit.oid) || null;
}

function sorted(nums) {
  return [...nums].sort((a, b) => a - b);
}

function median(nums) {
  if (!nums.length) return null;
  const s = sorted(nums);
  return s.length % 2 ? s[(s.length - 1) / 2] : (s[s.length / 2 - 1] + s[s.length / 2]) / 2;
}

// Nearest-rank p90 (same convention as slo-metrics.js / Hermes pipeline_slos.js).
function p90(nums) {
  if (!nums.length) return null;
  const s = sorted(nums);
  return s[Math.min(Math.floor(s.length * 0.9), s.length - 1)];
}

function round1(n) {
  return n === null || n === undefined ? null : Math.round(n * 10) / 10;
}

function round2(n) {
  return n === null || n === undefined ? null : Math.round(n * 100) / 100;
}

function hourStats(hours) {
  return {
    count: hours.length,
    min: hours.length ? round2(Math.min(...hours)) : null,
    median: round2(median(hours)),
    mean: hours.length ? round2(hours.reduce((a, b) => a + b, 0) / hours.length) : null,
    p90: round2(p90(hours)),
    max: hours.length ? round2(Math.max(...hours)) : null,
  };
}

// --- collection (gh-backed; exec is injectable for tests) ------------------------
function defaultExec(argv) {
  return execFileSync('gh', argv, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
}

const PR_FIELDS = 'number,title,state,headRefName,baseRefName,createdAt,updatedAt,closedAt,mergedAt,isDraft,labels,author,body,mergeCommit';
const ISSUE_FIELDS = 'number,title,state,createdAt,closedAt,labels';
const RUN_FIELDS = 'databaseId,createdAt,updatedAt,status,conclusion,event,headSha,workflowName';
const RELEASE_FIELDS = 'tagName,name,isDraft,isPrerelease,createdAt,publishedAt';

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

function fetchBaseRuns(repo, base, limit, exec) {
  const run = exec || defaultExec;
  return JSON.parse(run([
    'run', 'list', '-R', repo, '--branch', base, '--limit', String(limit), '--json', RUN_FIELDS,
  ]));
}

// Releases are a secondary signal — a repo without releases must not sink
// the report; null marks the fetch failed (distinct from an empty list).
function fetchReleases(repo, limit, exec) {
  const run = exec || defaultExec;
  try {
    return JSON.parse(run([
      'release', 'list', '-R', repo, '--limit', String(limit), '--json', RELEASE_FIELDS,
    ]));
  } catch (e) {
    return null;
  }
}

// Earliest `ready-to-implement` labeled event on an issue/PR timeline.
// The intake trigger is consumed at dispatch (spec/labels.md T4 strips
// the label), so the current label set cannot answer "when was it
// armed" — the timeline can. The jq emits one bare timestamp per line
// (JSONL) — page-safe under --paginate, unlike --slurp which gh refuses
// to combine with --jq. Fail-soft: a timeline we cannot read lands as
// null and the PR falls back to the pr_created basis.
// Parse the timeline-fetch output into the earliest label timestamp.
// Accepts JSONL (one bare "…Z" string per line — what --jq emits per
// page) and a single JSON array (fixture convenience). Exported for tests.
function earliestReadyAt(out) {
  const ts = [];
  const push = (v) => {
    const t = typeof v === 'string' ? parseTs(v) : null;
    if (t !== null) ts.push(t);
  };
  for (const line of String(out || '').split('\n')) {
    const s = line.trim();
    if (!s || s === 'null') continue;
    try {
      const v = JSON.parse(s);
      if (Array.isArray(v)) v.forEach(push);
      else push(v);
    } catch (e) {
      push(s);
    }
  }
  return ts.length ? new Date(Math.min(...ts)).toISOString() : null;
}

function fetchReadyAt(repo, number, exec) {
  const run = exec || defaultExec;
  try {
    return earliestReadyAt(run([
      'api', `repos/${repo}/issues/${number}/timeline?per_page=100`,
      '--paginate',
      '--jq', `.[] | select(.event == "labeled" and .label.name == "${READY_LABEL}") | .created_at`,
    ]));
  } catch (e) {
    return null;
  }
}

// One bounded pass: PR/issue/release/run lists, then at most one timeline
// call per merged-in-window PR (linked issue first, else the PR itself —
// the issues timeline endpoint covers PR numbers).
function collectData({ repo, base, maxPrs, runsLimit, releasesLimit, startMs, endMs }, exec) {
  const prs = fetchPullRequests(repo, maxPrs, exec);
  const issues = fetchIssues(repo, exec);
  const mainRuns = fetchBaseRuns(repo, base, runsLimit, exec);
  const releases = fetchReleases(repo, releasesLimit, exec);

  const readyAt = {};
  const timelineErrors = [];
  for (const pr of prs) {
    if (!isMergedPr(pr) || !inWindowTs(parseTs(pr.mergedAt), startMs, endMs)) continue;
    const issueNum = linkedIssueNumber(pr);
    const key = issueNum !== null ? `issue:${issueNum}` : `pr:${pr.number}`;
    if (readyAt[key] !== undefined) continue; // shared issue across retries
    const target = issueNum !== null ? issueNum : pr.number;
    let hit = null;
    try {
      hit = fetchReadyAt(repo, target, exec);
    } catch (e) {
      timelineErrors.push({ target: key, error: String(e.message || e).split('\n')[0] });
    }
    readyAt[key] = hit;
  }

  return {
    repo,
    collectedAt: new Date().toISOString(),
    pullRequests: prs,
    issues,
    mainRuns,
    releases,
    readyAt,
    timelineErrors,
  };
}

// --- compute -------------------------------------------------------------------

function authorTypeOf(pr, agenticAuthors) {
  if (isPipelinePr(pr)) return 'agentic';
  if (agenticAuthors.has(String(authorLogin(pr)).toLowerCase())) return 'agentic';
  return 'human';
}

function emptySplit() {
  return { agentic: [], human: [] };
}

// Base-branch red→green episodes. Completed runs ordered chronologically;
// a red streak opens an episode, the next success closes it (restore =
// completed-red → completed-green); a streak still open at collection end
// is `ongoing` — it measures MTTR honestly only when it recovers.
function buildEpisodes(mainRuns, { startMs, endMs, gateWorkflows }) {
  const gates = new Set((gateWorkflows || []).map((w) => String(w).toLowerCase()));
  const runs = (Array.isArray(mainRuns) ? mainRuns : [])
    .filter((r) => r && r.status === 'completed')
    .filter((r) => !gates.size || gates.has(String(r.workflowName || '').toLowerCase()))
    .map((r) => ({
      id: r.databaseId || r.id || null,
      headSha: r.headSha || null,
      workflow: r.workflowName || null,
      conclusion: String(r.conclusion || '').toLowerCase(),
      startTs: parseTs(r.createdAt),
      endTs: parseTs(r.updatedAt) || parseTs(r.createdAt),
    }))
    .filter((r) => r.startTs !== null)
    .sort((a, b) => (a.startTs - b.startTs) || ((a.id || 0) - (b.id || 0)));

  const episodes = [];
  let open = null;
  for (const r of runs) {
    const red = RED_CONCLUSIONS.has(r.conclusion);
    if (!open && red) {
      open = { start: r };
    } else if (open && r.conclusion === 'success') {
      episodes.push({
        start: open.start,
        end: r,
        restoreMs: Math.max(0, r.endTs - open.start.endTs),
        ongoing: false,
      });
      open = null;
    }
  }
  if (open) episodes.push({ start: open.start, end: null, restoreMs: null, ongoing: true });

  // Episodes intersecting the window: started before the end, not
  // resolved before the start.
  return episodes.filter((e) => {
    const epStart = e.start.startTs;
    const epEnd = e.end ? e.end.endTs : endMs;
    return epStart < endMs && epEnd >= startMs;
  });
}

// Revert bindings across the collected PR set: merged revert PR → target
// numbers (branch `revert-N`, `#N` refs in title/body of a Revert-titled
// PR). A revert merged after the window still fails the original merge.
function buildRevertMap(prs) {
  const revertOf = new Map(); // target number -> {pr, mergedAt}
  for (const r of prs) {
    if (!isMergedPr(r)) continue;
    const branch = String(r.headRefName || '');
    const branchM = branch.match(REVERT_BRANCH_RE);
    const titled = REVERT_TITLE_RE.test(String(r.title || ''));
    if (!branchM && !titled) continue;
    const targets = new Set();
    if (branchM) targets.add(parseInt(branchM[1], 10));
    if (titled) {
      for (const m of String(`${r.title || ''}\n${r.body || ''}`).matchAll(ISSUE_NUM_RE)) {
        targets.add(parseInt(m[1], 10));
      }
    }
    targets.delete(r.number); // never self-attribute
    for (const t of targets) {
      if (!revertOf.has(t)) revertOf.set(t, { pr: r.number, mergedAt: r.mergedAt || null });
    }
  }
  return revertOf;
}

function computeDora(data, {
  startMs, endMs, windowLabel, repo, base, failureWindowMs, gateWorkflows, agenticAuthors,
} = {}) {
  const prs = (data && data.pullRequests) || [];
  const issues = new Map(((data && data.issues) || []).map((i) => [i.number, i]));
  const readyAt = (data && data.readyAt) || {};
  const releases = (data && data.releases) || null;
  const agenticSet = new Set(
    (agenticAuthors && agenticAuthors.length ? agenticAuthors : parseCsvList(DEFAULT_AGENTIC_AUTHORS))
      .map((a) => String(a).toLowerCase()),
  );
  const windowDays = (endMs - startMs) / 86400e3;

  // Merged-to-base PRs whose merge event lands in the window = deployments.
  const merged = prs.filter((pr) => {
    if (!isMergedPr(pr)) return false;
    if (pr.baseRefName && pr.baseRefName !== base) return false;
    return inWindowTs(parseTs(pr.mergedAt), startMs, endMs);
  });
  const typeOf = new Map(merged.map((pr) => [pr.number, authorTypeOf(pr, agenticSet)]));

  // --- lead time for changes --------------------------------------------------
  const leadSamples = emptySplit();
  for (const pr of merged) {
    const mergedTs = parseTs(pr.mergedAt);
    const issueNum = linkedIssueNumber(pr);
    const key = issueNum !== null ? `issue:${issueNum}` : `pr:${pr.number}`;
    const labelTs = readyAt[key] !== undefined ? parseTs(readyAt[key]) : null;
    const basis = labelTs !== null ? 'ready_label' : 'pr_created';
    const startTs = labelTs !== null ? labelTs : parseTs(pr.createdAt);
    if (startTs === null || mergedTs === null) continue;
    const hours = (mergedTs - startTs) / 3600e3;
    if (hours < 0) continue; // clock skew — never emit negative latency
    leadSamples[typeOf.get(pr.number)].push({
      pr: pr.number,
      issue: issueNum,
      author: authorLogin(pr) || null,
      hours: round2(hours),
      basis,
      armedAt: labelTs !== null ? new Date(labelTs).toISOString() : null,
      mergedAt: pr.mergedAt,
    });
  }
  for (const t of Object.keys(leadSamples)) leadSamples[t].sort((a, b) => a.pr - b.pr);

  // --- change failure rate ------------------------------------------------------
  const episodes = buildEpisodes(data && data.mainRuns, { startMs, endMs, gateWorkflows });
  const byMergeSha = new Map(merged.filter((pr) => mergeSha(pr)).map((pr) => [mergeSha(pr), pr]));
  const mergedSorted = [...merged].sort((a, b) => (parseTs(a.mergedAt) || 0) - (parseTs(b.mergedAt) || 0));

  // Attribute each episode to the merge that introduced the red: the
  // merge commit sha first (exact), else the nearest preceding merge
  // inside --failure-window (dispatch/scheduled runs carry a moved sha).
  for (const ep of episodes) {
    let cause = ep.start.headSha ? byMergeSha.get(ep.start.headSha) : null;
    if (!cause) {
      for (let i = mergedSorted.length - 1; i >= 0; i--) {
        const mTs = parseTs(mergedSorted[i].mergedAt);
        if (mTs === null || mTs > ep.start.startTs) continue;
        if (ep.start.startTs - mTs <= failureWindowMs) cause = mergedSorted[i];
        break; // mergedSorted descends — first hit is the nearest
      }
    }
    ep.causedBy = cause ? cause.number : null;
    ep.authorType = cause ? typeOf.get(cause.number) : 'unattributed';
  }

  const revertOf = buildRevertMap(prs);
  const failures = emptySplit();
  for (const pr of merged) {
    const signals = [];
    const eps = episodes.filter((e) => e.causedBy === pr.number);
    if (eps.length) signals.push('red_main');
    const rev = revertOf.get(pr.number);
    if (rev && (!rev.mergedAt || parseTs(rev.mergedAt) === null
      || parseTs(rev.mergedAt) >= parseTs(pr.mergedAt))) {
      signals.push('revert');
    }
    // Non-converging repair: the attributed episode never recovered inside
    // the window, or the linked subject still holds the escalation /
    // terminal run-state label — the merge is done but the failure isn't.
    const issueNum = linkedIssueNumber(pr);
    const issue = issueNum !== null ? issues.get(issueNum) : null;
    const held = [pr, issue].filter(Boolean).some((o) => {
      const ls = labelNames(o);
      return ls.includes(ESCALATION_LABEL) || ls.includes(STALLED_RUN_LABEL);
    });
    if (held || eps.some((e) => e.ongoing)) signals.push('non_converging_repair');

    if (signals.length) {
      failures[typeOf.get(pr.number)].push({
        pr: pr.number,
        issue: issueNum,
        signals,
        revertedBy: rev ? rev.pr : null,
        episodes: eps.map((e) => ({ run: e.start.id, ongoing: e.ongoing })),
      });
    }
  }
  for (const t of Object.keys(failures)) failures[t].sort((a, b) => a.pr - b.pr);

  // --- MTTR ----------------------------------------------------------------------
  const mttrSamples = { agentic: [], human: [], unattributed: [] };
  for (const ep of episodes) {
    const bucket = ep.authorType === 'unattributed' ? 'unattributed' : ep.authorType;
    mttrSamples[bucket].push({
      run: ep.start.id,
      workflow: ep.start.workflow,
      startedAt: new Date(ep.start.endTs).toISOString(),
      recoveredAt: ep.end ? new Date(ep.end.endTs).toISOString() : null,
      hours: ep.restoreMs !== null ? round2(ep.restoreMs / 3600e3) : null,
      ongoing: ep.ongoing,
      causedBy: ep.causedBy,
    });
  }

  // --- deployments (secondary: release tags) --------------------------------------
  const releaseTags = Array.isArray(releases)
    ? releases.filter((r) => r && !r.isDraft
        && inWindowTs(parseTs(r.publishedAt) || parseTs(r.createdAt), startMs, endMs)).length
    : null;

  const bucket = (t) => merged.filter((pr) => typeOf.get(pr.number) === t);
  const deploySplit = (t) => {
    const n = bucket(t).length;
    return {
      count: n,
      perDay: round2(n / windowDays),
      perWeek: round2((n / windowDays) * 7),
    };
  };

  const leadBlock = (t) => ({ ...hourStats(leadSamples[t].map((s) => s.hours)), samples: leadSamples[t] });
  const allLead = [...leadSamples.agentic, ...leadSamples.human].sort((a, b) => a.pr - b.pr);
  const cfrBlock = (t) => {
    const total = bucket(t).length;
    const failed = failures[t].length;
    return { merges: total, failed, rate: total ? round1((failed / total) * 100) : null, failures: failures[t] };
  };
  const mttrBlock = (t) => {
    const s = mttrSamples[t];
    const recovered = s.filter((e) => !e.ongoing).map((e) => e.hours);
    return {
      episodes: s.length,
      recovered: recovered.length,
      ongoing: s.filter((e) => e.ongoing).length,
      ...hourStats(recovered),
      samples: s,
    };
  };

  return {
    version: 1,
    repo: repo || (data && data.repo) || null,
    generatedAt: new Date(endMs).toISOString(),
    base,
    window: {
      start: new Date(startMs).toISOString(),
      end: new Date(endMs).toISOString(),
      duration: windowLabel || null,
      failureWindow: null, // stamped by caller below
    },
    definitions: {
      deployment: `merge to ${base} (release tags secondary)`,
      leadTime: `${READY_LABEL} label applied → merge (fallback: PR creation, basis=pr_created)`,
      changeFailure: `merge followed by red ${base} run, merged revert, or non-converging repair within the failure window`,
      mttr: `${base} red run → next completed green run, attributed to the introducing merge`,
      agentic: `pipeline-authored — ${PIPELINE_BRANCH_RE.source} branch, "${PIPELINE_LABEL}" label, or author ∈ {${[...agenticSet].join(', ')}}`,
    },
    deployments: {
      agentic: deploySplit('agentic'),
      human: deploySplit('human'),
      total: {
        count: merged.length,
        perDay: round2(merged.length / windowDays),
        perWeek: round2((merged.length / windowDays) * 7),
      },
      releaseTags,
    },
    leadTimeHours: {
      agentic: leadBlock('agentic'),
      human: leadBlock('human'),
      all: { ...hourStats(allLead.map((s) => s.hours)), samples: allLead },
    },
    changeFailureRate: {
      agentic: cfrBlock('agentic'),
      human: cfrBlock('human'),
      all: {
        merges: merged.length,
        failed: failures.agentic.length + failures.human.length,
        rate: merged.length
          ? round1(((failures.agentic.length + failures.human.length) / merged.length) * 100)
          : null,
        failures: [...failures.agentic, ...failures.human].sort((a, b) => a.pr - b.pr),
      },
    },
    mttrHours: (() => {
      const s = [...mttrSamples.agentic, ...mttrSamples.human, ...mttrSamples.unattributed]
        .sort((a, b) => (a.run || 0) - (b.run || 0));
      const rec = s.filter((e) => !e.ongoing).map((e) => e.hours);
      return {
        agentic: mttrBlock('agentic'),
        human: mttrBlock('human'),
        unattributed: mttrBlock('unattributed'),
        all: {
          episodes: s.length,
          recovered: rec.length,
          ongoing: s.filter((e) => e.ongoing).length,
          ...hourStats(rec),
          samples: s,
        },
      };
    })(),
    counts: {
      prs_scanned: prs.length,
      merged_in_window: merged.length,
      agentic_merged: bucket('agentic').length,
      human_merged: bucket('human').length,
      base_runs: ((data && data.mainRuns) || []).length,
      episodes: episodes.length,
      timeline_fetches: Object.keys(readyAt).length,
      timeline_errors: ((data && data.timelineErrors) || []).length,
    },
  };
}

// --- renderers -------------------------------------------------------------------
function fmtHours(h) {
  if (h === null || h === undefined) return 'n/a';
  if (h < 1) return `${Math.round(h * 60)}m`;
  if (h < 48) return `${h.toFixed(1)}h`;
  return `${(h / 24).toFixed(1)}d`;
}

function fmtPct(p) {
  return p === null || p === undefined ? 'n/a' : `${p}%`;
}

function fmtMerges(n) {
  return `${n} merge${n === 1 ? '' : 's'}`;
}

function mdEsc(s) {
  return String(s == null ? '' : s).replace(/\|/g, '\\|').replace(/\n/g, ' ');
}

// Capped sample rows — the report must stay readable on a busy 90d window.
function cappedRows(items, fmt, cap = MD_SAMPLE_CAP) {
  const rows = items.slice(0, cap).map(fmt);
  if (items.length > cap) rows.push(`_… ${items.length - cap} more not shown_`);
  return rows;
}

function renderMarkdown(report) {
  const d = report.deployments;
  const lt = report.leadTimeHours;
  const cfr = report.changeFailureRate;
  const mt = report.mttrHours;
  const lines = [
    `# DORA metrics — ${report.repo || 'unknown repo'} (last ${report.window.duration || 'window'})`,
    '',
    `_Generated ${report.generatedAt} · base \`${report.base}\` · window \`${report.window.start}\` → \`${report.window.end}\`_`,
    '',
    '> Definitions (epic #88): deployment = merge to the base branch (release tags secondary);',
    '> lead time = `ready-to-implement` label applied → merge (fallback: PR creation);',
    '> change failure = merge followed by red main, revert or non-converging repair;',
    '> MTTR = main red → green. **Agentic** = pipeline-authored PRs; **human** = the rest.',
    '> Canonical definitions + event sources: [docs/metrics/definitions.md](definitions.md).',
    '',
    '## Comparison — agentic vs human',
    '',
    '| Metric | Agentic | Human | All |',
    '|---|---|---|---|',
    `| Deployment frequency | ${fmtMerges(d.agentic.count)} · ${d.agentic.perWeek ?? 'n/a'}/wk | ${fmtMerges(d.human.count)} · ${d.human.perWeek ?? 'n/a'}/wk | ${fmtMerges(d.total.count)} · ${d.total.perWeek ?? 'n/a'}/wk |`,
    `| Lead Time for Changes | median ${fmtHours(lt.agentic.median)} · p90 ${fmtHours(lt.agentic.p90)} (n=${lt.agentic.count}) | median ${fmtHours(lt.human.median)} · p90 ${fmtHours(lt.human.p90)} (n=${lt.human.count}) | median ${fmtHours(lt.all.median)} (n=${lt.all.count}) |`,
    `| Change Failure Rate | ${fmtPct(cfr.agentic.rate)} (${cfr.agentic.failed}/${cfr.agentic.merges}) | ${fmtPct(cfr.human.rate)} (${cfr.human.failed}/${cfr.human.merges}) | ${fmtPct(cfr.all.rate)} (${cfr.all.failed}/${cfr.all.merges}) |`,
    `| MTTR | mean ${fmtHours(mt.agentic.mean)} (${mt.agentic.episodes} ep) | mean ${fmtHours(mt.human.mean)} (${mt.human.episodes} ep) | mean ${fmtHours(mt.all.mean)} (${mt.all.episodes} ep) |`,
    '',
    '## Deployment Frequency',
    '',
    `Merges to \`${report.base}\` in the window — agentic ${d.agentic.count}, human ${d.human.count}, total ${d.total.count}` +
      ` (${d.total.perDay ?? 'n/a'}/day).` +
      (d.releaseTags === null
        ? ' Release tags: _unavailable (release fetch failed or unsupported)._'
        : ` Release tags in window (secondary): ${d.releaseTags}.`),
    '',
    '## Lead Time for Changes',
    '',
    `Hours from \`ready-to-implement\` (or PR creation when the label is absent — \`basis\`) to merge.`,
    '',
    '| PR | Author type | Hours | Basis | Issue |',
    '|---|---|---|---|---|',
    ...cappedRows(
      [...lt.agentic.samples.map((s) => ({ ...s, t: 'agentic' })),
       ...lt.human.samples.map((s) => ({ ...s, t: 'human' }))].sort((a, b) => a.pr - b.pr),
      (s) => `| #${s.pr} | ${s.t} | ${fmtHours(s.hours)} | ${s.basis} | ${s.issue ? `#${s.issue}` : '—'} |`,
    ),
    '',
    '## Change Failure Rate',
    '',
    `Failed merges / total merges × 100. Signals: \`red_main\` (base run red, attributed to the merge),` +
      ' `revert` (merged revert PR), `non_converging_repair` (escalated hold or red streak still open).',
    '',
    '| PR | Author type | Signals | Detail |',
    '|---|---|---|---|',
    ...(cfr.all.failures.length
      ? cappedRows(
          [
            ...cfr.agentic.failures.map((f) => ({ ...f, t: 'agentic' })),
            ...cfr.human.failures.map((f) => ({ ...f, t: 'human' })),
          ].sort((a, b) => a.pr - b.pr),
          (f) => `| #${f.pr} | ${f.t} | ${mdEsc(f.signals.join(', '))} | ${f.revertedBy ? `reverted by #${f.revertedBy}` : ''}${f.episodes.length ? ` ${f.episodes.map((e) => `run ${e.run || '?'}${e.ongoing ? ' (ongoing)' : ''}`).join(', ')}` : ''} |`,
        )
      : ['| — | — | none | no failed merges in window |']),
    '',
    '## MTTR — Time to Restore',
    '',
    `Base-branch red → green episodes (gate workflows: ${(report.gateWorkflowsLabel) || 'all'}). ` +
      `Episodes: ${mt.all.episodes} · recovered ${mt.all.recovered} · ongoing ${mt.all.ongoing} · ` +
      `unattributed ${mt.unattributed.episodes}.`,
    '',
    '| Started | Recovered | Hours | Attributed to | Workflow |',
    '|---|---|---|---|---|',
    ...(mt.all.samples.length
      ? cappedRows(mt.all.samples, (e) =>
          `| ${mdEsc(e.startedAt)} | ${e.recoveredAt ? mdEsc(e.recoveredAt) : 'ongoing'} | ${fmtHours(e.hours)} | ${e.causedBy ? `#${e.causedBy}` : '—'} | ${mdEsc(e.workflow || '—')} |`)
      : ['| — | — | — | — | — |']),
    '',
    `_Scanned ${report.counts.merged_in_window} merges (${report.counts.agentic_merged} agentic / ${report.counts.human_merged} human), ` +
      `${report.counts.base_runs} base runs, ${report.counts.timeline_fetches} timelines` +
      `${report.counts.timeline_errors ? ` (${report.counts.timeline_errors} timeline fetch error(s))` : ''}._`,
    '',
    '_Regenerated on schedule by `ai-sdlc-dora.yml` from `scripts/dora-metrics.js`._',
  ];
  return lines.join('\n');
}

function render(report, format) {
  if (format === 'markdown') return renderMarkdown(report);
  return JSON.stringify(report, null, 2);
}

// Flat keys for $GITHUB_OUTPUT — downstream steps/annotations consume the
// headline numbers without re-parsing the JSON artifact.
function githubOutputLines(report) {
  const d = report.deployments;
  const lt = report.leadTimeHours;
  const cfr = report.changeFailureRate;
  const mt = report.mttrHours;
  return [
    `dora_deployments_total=${d.total.count}`,
    `dora_deployments_agentic=${d.agentic.count}`,
    `dora_deployments_human=${d.human.count}`,
    `dora_deployments_per_week=${d.total.perWeek ?? ''}`,
    `dora_lead_time_median_hours=${lt.all.median ?? ''}`,
    `dora_lead_time_median_hours_agentic=${lt.agentic.median ?? ''}`,
    `dora_lead_time_median_hours_human=${lt.human.median ?? ''}`,
    `dora_change_failure_rate=${cfr.all.rate ?? ''}`,
    `dora_change_failure_rate_agentic=${cfr.agentic.rate ?? ''}`,
    `dora_change_failure_rate_human=${cfr.human.rate ?? ''}`,
    `dora_mttr_hours=${mt.all.mean ?? ''}`,
    `dora_mttr_episodes=${mt.all.episodes}`,
    `dora_mttr_ongoing=${mt.all.ongoing}`,
    `dora_release_tags=${d.releaseTags ?? ''}`,
  ];
}

// --- CLI ---------------------------------------------------------------------------
function parseArgs(argv) {
  const opts = { format: 'json' };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    const next = () => {
      i += 1;
      if (i >= argv.length) throw new Error(`missing value for ${a}`);
      return argv[i];
    };
    if (a === '--repo') opts.repo = next();
    else if (a === '--base') opts.base = next();
    else if (a === '--input') opts.input = next();
    else if (a === '--dump-input') opts.dumpInput = next();
    else if (a === '--window') opts.window = next();
    else if (a === '--failure-window') opts.failureWindow = next();
    else if (a === '--gate-workflows') opts.gateWorkflows = next();
    else if (a === '--agentic-authors') opts.agenticAuthors = next();
    else if (a === '--now') opts.now = next();
    else if (a === '--max-prs') opts.maxPrs = parseInt(next(), 10);
    else if (a === '--runs') opts.runs = parseInt(next(), 10);
    else if (a === '--releases') opts.releases = parseInt(next(), 10);
    else if (a === '--format') opts.format = next();
    else if (a === '--out') opts.out = next();
    else if (a === '--report') opts.report = next();
    else if (a === '--github-summary') opts.githubSummary = true;
    else if (a === '--github-output') opts.githubOutput = true;
    else throw new Error(`unknown argument: ${a}`);
  }
  if (!['json', 'markdown'].includes(opts.format)) {
    throw new Error(`--format must be json|markdown (got '${opts.format}')`);
  }
  return opts;
}

// --input accepts either the collected model (recompute) or a computed
// report (render directly — the docs/metrics/dora.md publish path).
function isComputedReport(doc) {
  return Boolean(doc && doc.version && doc.deployments && doc.changeFailureRate && doc.mttrHours);
}

function mkdirp(filePath) {
  const dir = path.dirname(filePath);
  if (dir && dir !== '.') fs.mkdirSync(dir, { recursive: true });
}

function main(argv, { exec, stdout, env } = {}) {
  const opts = parseArgs(argv || []);
  const emit = stdout || ((s) => console.log(s));
  const environ = env || process.env;

  const repo = opts.repo || environ.GITHUB_REPOSITORY;
  const base = opts.base || DEFAULT_BASE;
  const windowLabel = opts.window || DEFAULT_WINDOW;
  const windowMs = parseDuration(windowLabel, 30 * 86400e3);
  const failureWindowMs = parseDuration(opts.failureWindow || DEFAULT_FAILURE_WINDOW, 48 * 3600e3);
  const gateWorkflows = parseCsvList(opts.gateWorkflows);
  const agenticAuthors = parseCsvList(opts.agenticAuthors || DEFAULT_AGENTIC_AUTHORS);
  const endMs = opts.now ? Date.parse(opts.now) : Date.now();
  if (!Number.isFinite(endMs)) throw new Error(`--now is not a valid timestamp: ${opts.now}`);
  const startMs = endMs - windowMs;

  let report;
  if (opts.input) {
    const doc = JSON.parse(fs.readFileSync(opts.input, 'utf8'));
    if (isComputedReport(doc)) {
      report = doc;
    } else {
      if (repo && !doc.repo) doc.repo = repo;
      report = computeDora(doc, {
        startMs, endMs, windowLabel, repo: doc.repo, base,
        failureWindowMs, gateWorkflows, agenticAuthors,
      });
    }
  } else {
    if (!repo) throw new Error('--repo is required (or set GITHUB_REPOSITORY) outside --input mode');
    const data = collectData({
      repo,
      base,
      maxPrs: Math.min(opts.maxPrs || DEFAULT_MAX_PRS, MAX_PRS_CAP),
      runsLimit: Math.min(opts.runs || DEFAULT_RUNS, MAX_RUNS_CAP),
      releasesLimit: Math.min(opts.releases || DEFAULT_RELEASES, MAX_RELEASES_CAP),
      startMs,
      endMs,
    }, exec);
    if (opts.dumpInput) fs.writeFileSync(opts.dumpInput, `${JSON.stringify(data, null, 2)}\n`);
    report = computeDora(data, {
      startMs, endMs, windowLabel, repo, base,
      failureWindowMs, gateWorkflows, agenticAuthors,
    });
  }
  report.window.failureWindow = opts.failureWindow || DEFAULT_FAILURE_WINDOW;
  report.gateWorkflowsLabel = gateWorkflows.length ? gateWorkflows.join(', ') : 'all';

  const text = render(report, opts.format);
  if (opts.out) {
    mkdirp(opts.out);
    fs.writeFileSync(opts.out, `${text}\n`);
  } else {
    emit(text);
  }

  // --report: the committed markdown artifact (issue #102) — independent
  // of --format so one run produces dora-metrics.json AND dora.md.
  if (opts.report) {
    mkdirp(opts.report);
    fs.writeFileSync(opts.report, `${renderMarkdown(report)}\n`);
  }

  if (opts.githubSummary && environ.GITHUB_STEP_SUMMARY) {
    fs.appendFileSync(environ.GITHUB_STEP_SUMMARY, `${renderMarkdown(report)}\n\n`);
  }
  if (opts.githubOutput && environ.GITHUB_OUTPUT) {
    fs.appendFileSync(environ.GITHUB_OUTPUT, `${githubOutputLines(report).join('\n')}\n`);
  }
  return report;
}

if (require.main === module) {
  try {
    main(process.argv.slice(2));
  } catch (e) {
    console.error(`dora-metrics: ${e.message}`);
    process.exit(2);
  }
}

module.exports = {
  PIPELINE_BRANCH_RE,
  PIPELINE_LABEL,
  READY_LABEL,
  ESCALATION_LABEL,
  STALLED_RUN_LABEL,
  REVERT_TITLE_RE,
  REVERT_BRANCH_RE,
  RED_CONCLUSIONS,
  DEFAULT_AGENTIC_AUTHORS,
  DEFAULT_WINDOW,
  DEFAULT_BASE,
  DEFAULT_FAILURE_WINDOW,
  parseDuration,
  parseCsvList,
  isPipelinePr,
  linkedIssueNumber,
  isMergedPr,
  authorTypeOf,
  buildEpisodes,
  buildRevertMap,
  collectData,
  fetchPullRequests,
  fetchIssues,
  fetchBaseRuns,
  fetchReleases,
  fetchReadyAt,
  earliestReadyAt,
  computeDora,
  renderMarkdown,
  githubOutputLines,
  render,
  isComputedReport,
  parseArgs,
  main,
};
