#!/usr/bin/env node
// scripts/contract-drift.js — declared-vs-observed contract drift sweep
// (issue #176; epic #149 fleet operations).
//
// `.ai-sdlc.yaml` is the declared contract; this sweep diffs it against what
// the runtime actually did — stage invocations (workflow runs), labels
// applied, merges observed — and records every divergence on ONE canonical
// per-repo issue:
//
//   - drift found, no canonical           → file the canonical drift issue
//   - drift found, open canonical         → comment when the finding set
//                                           changed since last report;
//                                           identical fingerprint → no-op
//   - drift found, closed canonical       → comment the findings, reopen
//   - no drift, open canonical            → comment "resolved" + close
//   - no drift, no canonical              → clean; no issue ever created
//
// The canonical issue carries `<!-- ai-sdlc-drift-signature: <hash> -->`
// (sha256(repo)[:16]) so repeat sweeps update the same issue instead of
// duplicating; each finding set carries a fingerprint
// (`<!-- ai-sdlc-drift-meta: {"fingerprint":"…"} -->`) over sorted
// `check:subject` keys — the same divergence set never re-comments.
//
// Drift checks (declared → observed):
//   engine-unregistered      engine.name / engines.* not in the certified
//                            adapter registry (config/engine_adapters.json)
//   stage-engine-mismatch    observed stage ran under a different engine
//                            than engines.<stage> || engine.name resolves to
//   declared-engine-unused   engine data observed and the declared default
//                            engine never appears in any stage invocation
//   stage-never-invoked      evidence demands a stage (pipeline PRs →
//                            implement, merges → automerge/verify, …) yet it
//                            recorded zero invocations — runs named
//                            ai-sdlc-<stage> OR job checks on PR rollups
//                            (`review / review`, bare `intake` — reusable
//                            `uses:` calls never mint their own run entries)
//   autonomy-exceeded        merged pipeline PRs while autonomy.level is
//                            below auto-merge-low (the merge ceiling)
//   merge-checks-bypassed    merge_requires_checks + a merged pipeline PR
//                            whose reported checks contain a failure
//   required-check-missing   a verify.required_checks name never appears in
//                            any observed pipeline PR check rollup
//   merge-method-mismatch    merge.method vs observed merge-commit parent
//                            count (merge → 2 parents; squash|rebase → 1)
//   skip-label-violation     a merged pipeline PR carried a merge.skip_labels
//                            entry the gate must honor
//   label-veto-violation     an issue holds both sides of a forbidden pair
//                            (labels.state_machine.veto_pairs ∪ every
//                            triggers.skip_labels entry vetoing the trigger
//                            — spec/labels.md machine-checkable matrix)
//
// Every check fails OPEN on missing observation data: absent stage-engine
// records, check rollups or merge-parent probes skip the check rather than
// filing a false positive (partial data → partial report).
//
// Ported shape: Hermes env_catalog/janitor drift concept via the canonical-
// marker dedup of scripts/postmortem.js — same comment-first-then-reopen
// ordering so evidence lands even when the reopen call fails.
//
// Trust boundary: fetch and run this script only from a trusted runtime
// ref (see ai-sdlc-drift.yml) — never from a PR head or consumer checkout.
//
// CLI:
//   node scripts/contract-drift.js --repo OWNER/NAME [--window 30d] \
//     [--registry FILE] [--observed FILE] [--dry-run] [--json] [--github-output]
//   node scripts/contract-drift.js --input model.json [--dry-run] [--json]
//
// Inputs (one of):
//   --repo OWNER/NAME     collect live via `gh` + fetch .ai-sdlc.yaml raw
//   --input FILE          replay a collected model (fixture format below)
// Options:
//   --contract FILE       .ai-sdlc.yaml path (default: fetch from --repo, or
//                         model.contract inside --input)
//   --contract-json FILE  already-resolved contract JSON
//   --registry FILE       engine adapter registry JSON (object or id array)
//   --observed FILE       supplemental observed model merged over the live
//                         collection (e.g. ledger-derived stage engines)
//   --window 30d          trailing window for runs/merges (default 30d)
//   --limit N             per-list collection bound (default 200, cap 500)
//   --dry-run             report only — never comments/files/closes
//   --json                print the report object to stdout
//   --github-output       append drift_action/drift_issue/drift_findings
//
// Model (--input / --dump-input):
//   { repo, collectedAt, contract: {...}, registry: {...}|[],
//     observed: { workflowRuns: [{name|workflowName, conclusion, createdAt}],
//                 pullRequests: [<gh pr list rows + mergeParents?>],
//                 issues: [<gh issue list rows>],
//                 stages: { implement: {runs, engines:[...]}, ... } } }
//
// Bounded: one contents fetch + run list + pr list + issue list, then at
// most 10 commit probes (merge-parent evidence) on the newest merged PRs.
// Exit: 0 ok · 2 usage/I-O error.
'use strict';

const fs = require('fs');
const crypto = require('crypto');
const { execFileSync } = require('child_process');

const DRIFT_LABEL = 'contract-drift';
const DEFAULT_LABELS = 'ai-sdlc,contract-drift,priority:P3';
const DEFAULT_WINDOW = '30d';
const DEFAULT_LIMIT = 200;
const LIMIT_CAP = 500;
const RUNS_LIMIT = 300;
const MAX_MERGE_PROBES = 10;   // commit lookups for merge-method evidence
const MAX_ISSUES_SCAN = 100;   // per-state canonical scan bound

const SIG_MARKER_RE = /<!--\s*ai-sdlc-drift-signature:\s*([0-9a-f]{8,64})\s*-->/i;
const META_MARKER_RE = /<!--\s*ai-sdlc-drift-meta:\s*(\{[\s\S]*?\})\s*-->/;
const PIPELINE_BRANCH_RE = /^(feat|fix)\/issue-(\d+)/;
const PIPELINE_LABEL = 'ai-sdlc';
// Only explicit failure conclusions prove a bypass — null/pending entries in
// a rollup reflect current head state, not merge-time evidence (fail-open).
const FAILING_CHECK_STATES = new Set([
  'FAILURE', 'TIMED_OUT', 'CANCELLED', 'ACTION_REQUIRED', 'ERROR',
  'failure', 'timed_out', 'cancelled', 'action_required', 'error',
]);

// Autonomy ladder (spec/trust-ladder.md): position in the array is the
// ceiling rank — merging requires >= 'auto-merge-low'.
const AUTONOMY_LEVELS = ['shadow', 'suggest', 'auto-pr', 'auto-merge-low', 'auto-merge-all', 'auto-deploy'];
const MERGE_MIN_LEVEL = 'auto-merge-low';

// Canonical pipeline stage names (schema `engines` keys) → workflow name.
const STAGES = ['intake', 'implement', 'review', 'automerge', 'verify', 'ci-repair', 'sweep'];

function sha256(text) {
  return crypto.createHash('sha256').update(String(text)).digest('hex');
}

function parseTs(v) {
  const t = new Date(v || 0).getTime();
  return Number.isFinite(t) ? t : null;
}

function parseDuration(s, fallbackMs) {
  const m = String(s || '').match(/^(\d+)(m|h|d)?$/);
  if (!m) return fallbackMs;
  const n = parseInt(m[1], 10);
  return n * (m[2] === 'd' ? 86400e3 : m[2] === 'h' ? 3600e3 : m[2] === 'm' ? 60e3 : 1e3);
}

function labelNames(obj) {
  return ((obj && obj.labels) || [])
    .map((l) => (typeof l === 'string' ? l : l && l.name))
    .filter(Boolean);
}

function isPipelinePr(pr) {
  return PIPELINE_BRANCH_RE.test(pr.headRefName || '') || labelNames(pr).includes(PIPELINE_LABEL);
}

function isMergedPr(pr) {
  return Boolean(pr.mergedAt) || String(pr.state || '').toUpperCase() === 'MERGED';
}

// --- Contract accessors ------------------------------------------------------

function contractEngineForStage(contract, stage) {
  const c = contract || {};
  const sel = (c.engines || {})[stage];
  if (typeof sel === 'string') return sel;
  if (sel && typeof sel === 'object' && sel.engine) return sel.engine;
  return (c.engine && c.engine.name) || 'scc';   // schema default
}

/** All engine ids the contract declares (default + every stage pin). */
function declaredEngines(contract) {
  const c = contract || {};
  const out = new Set();
  const name = (c.engine && c.engine.name) || 'scc';
  out.add(name);
  for (const stage of Object.keys(c.engines || {})) {
    const v = c.engines[stage];
    out.add(typeof v === 'string' ? v : (v && v.engine) || name);
  }
  return [...out];
}

/** Forbidden label pairs (spec/labels.md): declared veto_pairs ∪ every
 *  triggers.skip_labels entry vetoing the trigger label. */
function forbiddenPairs(contract) {
  const c = contract || {};
  const pairs = [];
  const declared = (((c.labels || {}).state_machine || {}).veto_pairs) || [];
  for (const p of declared) {
    if (p && p.winner && p.loser) pairs.push({ winner: p.winner, loser: p.loser });
  }
  const trigger = (c.triggers || {}).issue_label || 'ready-to-implement';
  for (const s of ((c.triggers || {}).skip_labels) || []) {
    pairs.push({ winner: s, loser: trigger });
  }
  return pairs;
}

// --- Observed model normalization --------------------------------------------
// Maps the collected model into per-stage run/engine evidence and pipeline
// PR/issue views. Every field tolerates absence — partial data skips checks.

function stageNameOfRun(run) {
  const raw = String(run.workflowName || run.name || '');
  const m = raw.match(/^ai-sdlc-([a-z-]+?)(?:\.ya?ml)?$/);
  if (!m) return null;
  return m[1];
}

/** Check-run name → stage. Reusable-call stages surface on PR rollups as
 *  caller/called job names (`review / review`, `ci-repair / repair`) or bare
 *  job ids (`intake`, `implement`) — any path segment matching a stage key
 *  counts as invocation evidence. */
function stageOfCheckName(name) {
  const segs = String(name || '').split('/').map((s) => s.trim()).filter(Boolean);
  for (const s of segs) if (STAGES.includes(s)) return s;
  return null;
}

function normalizeObserved(model, { startMs, endMs } = {}) {
  const obs = (model && model.observed) || {};
  const inWindow = (ts) => {
    if (startMs == null || endMs == null) return true;
    const t = parseTs(ts);
    return t !== null && t >= startMs && t < endMs;
  };

  const stages = {};  // stage -> {runs, engines:Set}
  const bump = (stage, engine) => {
    if (!stage) return;
    if (!stages[stage]) stages[stage] = { runs: 0, engines: new Set() };
  };
  for (const run of obs.workflowRuns || []) {
    const stage = stageNameOfRun(run);
    if (!stage) continue;
    if (!inWindow(run.createdAt)) continue;
    bump(stage);
    stages[stage].runs += 1;
  }
  // Supplemental stage evidence (e.g. ledger-derived {runs, engines}).
  for (const [stage, rec] of Object.entries(obs.stages || {})) {
    bump(stage);
    stages[stage].runs += (rec && rec.runs) || 0;
    for (const e of (rec && rec.engines) || []) stages[stage].engines.add(e);
  }
  // Flat invocation list shape: observed.stageRuns: [{stage, engine}]
  for (const r of obs.stageRuns || []) {
    bump(r && r.stage);
    if (r && r.stage && inWindow(r.at || r.createdAt)) {
      stages[r.stage].runs += 1;
      if (r.engine) stages[r.stage].engines.add(r.engine);
    }
  }

  const prs = (obs.pullRequests || []).filter(isPipelinePr);
  const windowedMerged = prs.filter((p) => isMergedPr(p) && inWindow(p.mergedAt));
  const windowedPrs = prs.filter((p) =>
    [parseTs(p.createdAt), parseTs(p.mergedAt), parseTs(p.closedAt)]
      .some((t) => t !== null && (startMs == null || (t >= startMs && t < endMs))));

  // Reusable-call stage evidence lives on PR check rollups (job names), not
  // in workflow-run names — `uses:` stages never mint their own run entries.
  for (const pr of windowedPrs) {
    for (const n of rollupCheckNames(pr)) {
      const stage = stageOfCheckName(n);
      if (stage) { bump(stage); stages[stage].runs += 1; }
    }
  }

  return {
    stages,
    pipelinePrs: windowedPrs,
    mergedPrs: windowedMerged,
    issues: obs.issues || [],
  };
}

/** Check-context names a PR's rollup reports (checkRun.name or context). */
function rollupCheckNames(pr) {
  return ((pr.statusCheckRollup) || [])
    .map((r) => (r && (r.name || r.context)) || null)
    .filter(Boolean);
}

function rollupHasFailures(pr) {
  const entries = (pr.statusCheckRollup) || [];
  if (!entries.length) return false;
  return entries.some((e) => {
    const v = e && (e.conclusion || e.state);
    return v != null && FAILING_CHECK_STATES.has(v);
  });
}

// --- Drift evaluation ---------------------------------------------------------
// Pure over (contract, observed model). Returns findings:
//   {check, subject, declared, observed, detail}

function evaluateDrift(model, opts = {}) {
  const contract = (model && model.contract) || {};
  const registry = (model && model.registry) || null;
  const obs = model && model.__normalized
    ? model.__normalized
    : normalizeObserved(model, opts);
  const findings = [];
  const find = (check, subject, declared, observed, detail) =>
    findings.push({ check, subject, declared, observed, detail });

  // -- engine-unregistered ----------------------------------------------------
  const registryIds = registry == null ? null
    : new Set(Array.isArray(registry) ? registry : Object.keys(registry));
  if (registryIds) {
    for (const id of declaredEngines(contract)) {
      if (!registryIds.has(id)) {
        find('engine-unregistered', `engine:${id}`, id, [...registryIds].sort(),
          `declared engine '${id}' has no entry in the certified adapter registry`);
      }
    }
  }

  // -- stage-engine-mismatch / declared-engine-unused --------------------------
  const stagesWithEngines = Object.entries(obs.stages).filter(([, s]) => s.engines.size > 0);
  for (const [stage, s] of stagesWithEngines) {
    const expected = contractEngineForStage(contract, stage);
    const wrong = [...s.engines].filter((e) => e !== expected);
    if (wrong.length) {
      find('stage-engine-mismatch', `stage:${stage}`, expected, wrong.sort(),
        `stage '${stage}' ran under engine(s) the contract does not select`);
    }
  }
  const defaultEngine = (contract.engine && contract.engine.name) || 'scc';
  // The default engine is only "unused" when an UNPINNED stage (which would
  // resolve to it) carries engine evidence and still never ran it — pinned
  // stages legitimately never invoke the default.
  const unpinnedWithData = stagesWithEngines.filter(([s]) => !(contract.engines || {})[s]);
  if (unpinnedWithData.length) {
    const seen = new Set();
    for (const [, s] of unpinnedWithData) for (const e of s.engines) seen.add(e);
    if (!seen.has(defaultEngine)) {
      find('declared-engine-unused', `engine:${defaultEngine}`, defaultEngine, [...seen].sort(),
        `declared default engine '${defaultEngine}' never appears in observed invocations of stages resolving to it`);
    }
  }

  // -- stage-never-invoked ------------------------------------------------------
  // Evidence the stage should have run inside the window.
  const pipelineActivity = obs.pipelinePrs.length;   // merged ⊆ pipeline
  const triggerLabel = (contract.triggers || {}).issue_label || 'ready-to-implement';
  const armedIssues = obs.issues.filter((i) => labelNames(i).includes(triggerLabel));
  const reviewMode = ((contract.review || {}).mode) || 'ai';
  const expected = {
    intake: pipelineActivity > 0 || armedIssues.length > 0,
    implement: pipelineActivity > 0,
    review: pipelineActivity > 0 && ['ai', 'hybrid'].includes(reviewMode),
    // automerge/verify are excluded: invoked via `uses:` inside sweep or
    // post-merge callers they mint no observable run entries and no PR
    // checks — their outcome IS the merge (covered by autonomy/merge
    // checks). ci-repair/sweep are conditional or scheduler-owned.
  };
  for (const stage of Object.keys(expected)) {
    if (!expected[stage]) continue;
    const runs = (obs.stages[stage] && obs.stages[stage].runs) || 0;
    if (runs === 0) {
      find('stage-never-invoked', `stage:${stage}`, contractEngineForStage(contract, stage),
        `${runs} runs`,
        `stage '${stage}' shows ${pipelineActivity} pipeline PR(s) in the window but recorded no '${`ai-sdlc-${stage}`}' workflow runs`);
    }
  }

  // -- autonomy-exceeded ---------------------------------------------------------
  const level = ((contract.autonomy || {}).level) || 'suggest';
  const mergeRank = AUTONOMY_LEVELS.indexOf(MERGE_MIN_LEVEL);
  const declaredRank = AUTONOMY_LEVELS.indexOf(level);
  if (obs.mergedPrs.length && declaredRank !== -1 && declaredRank < mergeRank) {
    find('autonomy-exceeded', 'autonomy.level', level,
      obs.mergedPrs.map((p) => `#${p.number}`).sort(),
      `autonomy.level '${level}' is below the merge ceiling yet pipeline PRs were merged`);
  }

  // -- merge-checks-bypassed -----------------------------------------------------
  if ((contract.autonomy || {}).merge_requires_checks !== false) {
    for (const pr of obs.mergedPrs) {
      if (rollupHasFailures(pr)) {
        find('merge-checks-bypassed', `pr:${pr.number}`, 'merge_requires_checks=true',
          rollupCheckNames(pr).join(', '),
          `pipeline PR #${pr.number} merged with non-passing check(s) in its rollup`);
      }
    }
  }

  // -- required-check-missing ----------------------------------------------------
  const required = ((contract.verify || {}).required_checks) || [];
  if (required.length) {
    const rollupsSeen = obs.pipelinePrs.some((p) => rollupCheckNames(p).length > 0);
    if (rollupsSeen) {
      const seen = new Set();
      for (const p of obs.pipelinePrs) for (const n of rollupCheckNames(p)) seen.add(n);
      for (const name of required) {
        if (!seen.has(name)) {
          find('required-check-missing', `check:${name}`, name, [...seen].sort(),
            `required check '${name}' never appears in observed pipeline PR check rollups`);
        }
      }
    }
  }

  // -- merge-method-mismatch -----------------------------------------------------
  const method = ((contract.merge || {}).method) || 'squash';
  for (const pr of obs.mergedPrs) {
    if (!Number.isFinite(pr.mergeParents)) continue;   // not probed — skip
    const expectedParents = method === 'merge' ? 2 : 1;
    if (pr.mergeParents !== expectedParents) {
      find('merge-method-mismatch', `pr:${pr.number}`, method,
        `${pr.mergeParents} merge-commit parents`,
        `merge.method '${method}' expects ${expectedParents} parent(s); observed ${pr.mergeParents} on PR #${pr.number}`);
    }
  }

  // -- skip-label-violation ------------------------------------------------------
  const mergeSkips = new Set(((contract.merge || {}).skip_labels) || []);
  if (mergeSkips.size) {
    for (const pr of obs.mergedPrs) {
      const hit = labelNames(pr).filter((l) => mergeSkips.has(l));
      if (hit.length) {
        find('skip-label-violation', `pr:${pr.number}`, [...mergeSkips].sort(), hit.sort(),
          `pipeline PR #${pr.number} merged while carrying merge-skip label(s) ${hit.join(', ')}`);
      }
    }
  }

  // -- label-veto-violation ------------------------------------------------------
  const pairs = forbiddenPairs(contract);
  if (pairs.length) {
    for (const issue of obs.issues) {
      // Open issues only — a veto pair on a closed issue is historical
      // residue, not live drift the janitor would act on.
      if (String(issue.state || 'open').toUpperCase() === 'CLOSED') continue;
      const held = new Set(labelNames(issue));
      for (const p of pairs) {
        if (held.has(p.winner) && held.has(p.loser)) {
          find('label-veto-violation', `issue:${issue.number}`, `${p.winner} ⊣ ${p.loser}`,
            [p.winner, p.loser].sort(),
            `issue #${issue.number} holds the forbidden pair '${p.loser}' + '${p.winner}' (spec/labels.md)`);
        }
      }
    }
  }

  return findings;
}

// --- Fingerprint / canonical markers -------------------------------------------

/** sha256 over sorted `check:subject` keys — identifies the finding SET. */
function fingerprintFindings(findings) {
  const keys = (findings || []).map((f) => `${f.check}:${f.subject}`).sort();
  return sha256(keys.join('\n')).slice(0, 16);
}

function repoSignature(repo) {
  return sha256(repo).slice(0, 16);
}

function signatureMarker(sig) {
  return `<!-- ai-sdlc-drift-signature: ${sig} -->`;
}

function metaMarker(repo, fingerprint) {
  return `<!-- ai-sdlc-drift-meta: ${JSON.stringify({ repo, fingerprint })} -->`;
}

function issueSignatureOf(issue) {
  const body = String((issue && issue.body) || '');
  const m = body.match(SIG_MARKER_RE);
  return m ? m[1].toLowerCase() : null;
}

/** Lowest-numbered matching issue is the canonical (one per repo). */
function findCanonical(issues, sig) {
  return (issues || [])
    .filter((i) => issueSignatureOf(i) === sig ||
      String((i && i.body) || '').includes(sig))
    .sort((a, b) => a.number - b.number)[0] || null;
}

/** The last-recorded findings fingerprint on issue body + comments. */
function storedFingerprint(issue, comments) {
  const scan = [issue && issue.body, ...(comments || []).map((c) => c && c.body)];
  for (let i = scan.length - 1; i >= 0; i--) {
    const m = String(scan[i] || '').match(META_MARKER_RE);
    if (m) {
      try { return (JSON.parse(m[1]) || {}).fingerprint || null; }
      catch (e) { /* malformed meta — keep looking */ }
    }
  }
  return null;
}

// --- Bodies --------------------------------------------------------------------

function renderFindings(findings) {
  return (findings || []).map((f) =>
    `- **${f.check}** \`${f.subject}\` — declared: \`${truncate(f.declared)}\`; observed: \`${truncate(Array.isArray(f.observed) ? f.observed.join(', ') : f.observed)}\`\n  ${f.detail}`)
    .join('\n');
}

function truncate(v, n = 200) {
  const s = String(v == null ? '' : v);
  return s.length > n ? `${s.slice(0, n)}…` : s;
}

function filingBody(repo, findings, fingerprint, sig) {
  return `### Contract drift — declared vs observed pipeline behavior

The \`.ai-sdlc.yaml\` contract for \`${repo}\` diverges from observed runtime behavior. Each divergence below is declared-vs-observed evidence collected by the contract-drift sweep (issue #176; epic #149):

${renderFindings(findings)}

---

This is the canonical drift issue for this repo — repeat sweeps update it (comment on a changed finding set, close when the drift clears) instead of filing duplicates. Findings fingerprint: \`${fingerprint}\`.

${signatureMarker(sig)}
${metaMarker(repo, fingerprint)}`;
}

function updateComment(repo, findings, fingerprint) {
  return `### 🔄 Contract drift — finding set changed

The drift sweep re-ran and the declared-vs-observed divergence set changed:

${renderFindings(findings)}

${metaMarker(repo, fingerprint)}`;
}

function resolvedComment(repo, fingerprint) {
  return `### ✅ Contract drift resolved

The latest sweep observed no divergence between the declared contract and pipeline behavior — the finding set is empty. Closing the canonical issue; a future sweep that finds drift will reopen or re-file under the same signature.

${metaMarker(repo, fingerprint)}`;
}

// --- Core decision + mutation ----------------------------------------------------
// `client` supplies the GitHub surface (see ghClient()); tests inject a fake.
// Actions: filed | updated | unchanged | reopened | resolved | clean |
//          dry_run_file | dry_run_comment | dry_run_reopen | dry_run_close.

function handleDrift({ repo, findings, now, dryRun, labels }, client) {
  const sig = repoSignature(repo);
  const fingerprint = findings.length ? fingerprintFindings(findings) : 'clean';
  const list = (state) => client.listIssues({ repo, state });
  const open = findCanonical(list('open'), sig);
  const closed = open ? null : findCanonical(list('closed'), sig);
  const canonical = open || closed;
  const comments = canonical ? client.listComments({ repo, issueNumber: canonical.number }) : [];

  if (!findings.length) {
    if (!open) return { action: 'clean', fingerprint };
    if (dryRun) return { action: 'dry_run_close', issueNumber: open.number, issueUrl: open.url, fingerprint };
    client.addComment({ repo, issueNumber: open.number, body: resolvedComment(repo, fingerprint) });
    if (typeof client.closeIssue === 'function') client.closeIssue({ repo, issueNumber: open.number });
    return { action: 'resolved', issueNumber: open.number, issueUrl: open.url, fingerprint };
  }

  const stored = storedFingerprint(canonical || {}, comments);
  if (open && stored === fingerprint) {
    return { action: 'unchanged', issueNumber: open.number, issueUrl: open.url, fingerprint };
  }

  if (open) {
    if (dryRun) return { action: 'dry_run_comment', issueNumber: open.number, issueUrl: open.url, fingerprint };
    client.addComment({ repo, issueNumber: open.number, body: updateComment(repo, findings, fingerprint) });
    return { action: 'updated', issueNumber: open.number, issueUrl: open.url, fingerprint };
  }

  if (closed) {
    if (dryRun) return { action: 'dry_run_reopen', issueNumber: closed.number, issueUrl: closed.url, fingerprint };
    // Comment first — evidence lands even if the reopen call fails.
    client.addComment({ repo, issueNumber: closed.number, body: updateComment(repo, findings, fingerprint) });
    client.reopenIssue({ repo, issueNumber: closed.number });
    return { action: 'reopened', issueNumber: closed.number, issueUrl: closed.url, fingerprint };
  }

  if (dryRun) return { action: 'dry_run_file', fingerprint };
  const labelList = Array.isArray(labels) ? labels
    : String(labels || DEFAULT_LABELS).split(',').map((s) => s.trim()).filter(Boolean);
  if (typeof client.ensureLabel === 'function') client.ensureLabel({ repo, label: DRIFT_LABEL });
  const created = client.createIssue({
    repo,
    title: `contract drift: declared vs observed divergence in ${repo} [${sig}]`,
    body: filingBody(repo, findings, fingerprint, sig),
    labels: labelList,
  });
  return { action: 'filed', issueNumber: created.number, issueUrl: created.url, fingerprint };
}

// --- gh-backed client + collection -----------------------------------------------

function defaultExec(args) {
  return execFileSync('gh', args, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'], timeout: 30000 });
}

function ghClient(exec) {
  const run = exec || defaultExec;
  const safe = (args, fallback) => {
    try { return run(args); } catch (e) { return fallback; }
  };
  const bodyFile = (body) => {
    const f = `/tmp/ai_sdlc_drift_${Date.now()}_${Math.random().toString(36).slice(2, 8)}.md`;
    fs.writeFileSync(f, body, 'utf8');
    return f;
  };
  return {
    listIssues({ repo, state }) {
      const out = safe([
        'issue', 'list', '--repo', repo, '--state', state,
        '--limit', String(MAX_ISSUES_SCAN), '--json', 'number,title,body,labels,url,closedAt',
      ], '');
      try { return out ? JSON.parse(out) : []; }
      catch (e) { console.warn(`[!] gh issue list (${state}) failed for ${repo} — treating as empty`); return []; }
    },
    listComments({ repo, issueNumber }) {
      const out = safe(['api', `repos/${repo}/issues/${issueNumber}/comments?per_page=100`, '--jq', '.[] | {created_at, body}'], '');
      if (!out.trim()) return [];
      return out.trim().split('\n').map((l) => { try { return JSON.parse(l); } catch (e) { return null; } }).filter(Boolean);
    },
    addComment({ repo, issueNumber, body }) {
      const f = bodyFile(body);
      try { run(['issue', 'comment', String(issueNumber), '--repo', repo, '--body-file', f]); }
      finally { try { fs.unlinkSync(f); } catch (e) {} }
    },
    reopenIssue({ repo, issueNumber }) { run(['issue', 'reopen', String(issueNumber), '--repo', repo]); },
    closeIssue({ repo, issueNumber }) { run(['issue', 'close', String(issueNumber), '--repo', repo]); },
    ensureLabel({ repo, label }) {
      safe(['label', 'create', label, '--repo', repo, '--color', 'd4c5f9',
        '--description', 'AI-SDLC canonical contract-drift issue — deduped by repo signature'], '');
    },
    createIssue({ repo, title, body, labels }) {
      const f = bodyFile(body);
      try {
        let out = '';
        const base = ['issue', 'create', '--repo', repo, '--title', title, '--body-file', f];
        try {
          out = run([...base, '--label', (labels || []).join(',')]);
        } catch (e) {
          console.warn(`[!] issue create with labels failed — retrying without labels`);
          out = run(base);
        }
        const m = out.trim().match(/\/issues\/(\d+)/);
        return { number: m ? parseInt(m[1], 10) : null, url: out.trim() };
      } finally { try { fs.unlinkSync(f); } catch (e) {} }
    },
  };
}

/** yaml → json via python3 (PyYAML) or `npx js-yaml` — same split as
 *  load-config's yaml_to_json. */
function yamlToJson(file, exec) {
  const runExec = exec || ((cmd, args) => execFileSync(cmd, args, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'], timeout: 30000 }));
  try {
    runExec('python3', ['-c', 'import yaml']);
    return JSON.parse(runExec('python3', ['-c',
      'import yaml,json,sys; json.dump(yaml.safe_load(open(sys.argv[1])), sys.stdout)', file]));
  } catch (e) {
    return JSON.parse(execFileSync('npx', ['-y', '-q', 'js-yaml', file],
      { encoding: 'utf8', timeout: 60000 }));
  }
}

/** Fetch .ai-sdlc.yaml raw from the repo's default branch and parse it. */
function fetchContract(repo, exec) {
  const run = exec || defaultExec;
  const raw = run(['api', `repos/${repo}/contents/.ai-sdlc.yaml`,
    '-H', 'Accept: application/vnd.github.raw']);
  const tmp = `/tmp/ai_sdlc_contract_${Date.now()}_${Math.random().toString(36).slice(2, 8)}.yaml`;
  fs.writeFileSync(tmp, raw, 'utf8');
  try { return yamlToJson(tmp); }
  finally { try { fs.unlinkSync(tmp); } catch (e) {} }
}

/** Live collection → the same model shape --input consumes. */
function collect(repo, { limit, windowMs, endMs } = {}, exec) {
  const run = exec || defaultExec;
  const out = { repo, collectedAt: new Date().toISOString(), observed: {} };

  try { out.contract = fetchContract(repo, run); }
  catch (e) { console.warn(`[!] contract fetch failed for ${repo} — ${String(e.message).split('\n')[0]}`); }

  const since = new Date(endMs - windowMs).toISOString();
  const prs = JSON.parse(run([
    'pr', 'list', '-R', repo, '--state', 'all', '--limit', String(limit),
    '--json', 'number,title,state,headRefName,labels,createdAt,updatedAt,closedAt,mergedAt,isDraft,mergeCommit,statusCheckRollup',
  ]));
  const issues = JSON.parse(run([
    'issue', 'list', '-R', repo, '--state', 'all', '--limit', String(limit),
    '--json', 'number,title,state,labels,createdAt,closedAt',
  ]));
  const runs = JSON.parse(run([
    'run', 'list', '-R', repo, '--limit', String(RUNS_LIMIT),
    '--json', 'name,workflowName,status,conclusion,createdAt,headBranch',
  ]));

  // Merge-method evidence: probe merge-commit parents for the newest merged
  // pipeline PRs only (bounded — one commits API call each).
  const mergedPipeline = prs
    .filter((p) => isPipelinePr(p) && isMergedPr(p) && p.mergeCommit && p.mergeCommit.oid)
    .sort((a, b) => (parseTs(b.mergedAt) || 0) - (parseTs(a.mergedAt) || 0))
    .slice(0, MAX_MERGE_PROBES);
  for (const pr of mergedPipeline) {
    try {
      const n = run(['api', `repos/${repo}/commits/${pr.mergeCommit.oid}`, '--jq', '.parents | length']);
      pr.mergeParents = parseInt(String(n).trim(), 10);
    } catch (e) { /* leave unprobed — the check skips missing evidence */ }
  }

  out.observed = { workflowRuns: runs, pullRequests: prs, issues };
  out.window = { start: since, end: new Date(endMs).toISOString() };
  return out;
}

/** Merge supplemental observed data (e.g. ledger-derived stage engines)
 *  into a collected or replayed model. */
function mergeObserved(model, extra) {
  if (!extra || typeof extra !== 'object') return model;
  model.observed = model.observed || {};
  for (const [stage, rec] of Object.entries(extra.stages || {})) {
    const cur = model.observed.stages = model.observed.stages || {};
    const base = cur[stage] = cur[stage] || { runs: 0, engines: [] };
    base.runs = (base.runs || 0) + ((rec && rec.runs) || 0);
    base.engines = [...new Set([...(base.engines || []), ...((rec && rec.engines) || [])])];
  }
  for (const k of ['workflowRuns', 'pullRequests', 'issues', 'stageRuns']) {
    if (Array.isArray(extra[k])) {
      model.observed[k] = [...(model.observed[k] || []), ...extra[k]];
    }
  }
  return model;
}

// --- CLI --------------------------------------------------------------------------

function parseArgs(argv) {
  const opts = { _: [] };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a.startsWith('--')) {
      const k = a.slice(2);
      if (['dry-run', 'json', 'github-output'].includes(k)) opts[k] = true;
      else opts[k] = argv[++i];
    } else opts._.push(a);
  }
  return opts;
}

function main(argv) {
  const a = parseArgs(argv || []);
  const dryRun = !!a['dry-run'] || process.env.AI_SDLC_DRIFT_DRY_RUN === '1';
  const windowLabel = a.window || DEFAULT_WINDOW;
  const windowMs = parseDuration(windowLabel, 30 * 86400e3);
  const endMs = a.now ? Date.parse(a.now) : Date.now();
  if (!Number.isFinite(endMs)) throw new Error(`--now is not a valid timestamp: ${a.now}`);
  const limit = Math.min(parseInt(a.limit || String(DEFAULT_LIMIT), 10) || DEFAULT_LIMIT, LIMIT_CAP);

  let model;
  if (a.input) {
    model = JSON.parse(fs.readFileSync(a.input, 'utf8'));
  } else {
    const repo = a.repo || process.env.GITHUB_REPOSITORY;
    if (!repo || !/^[-A-Za-z0-9_.]+\/[-A-Za-z0-9_.]+$/.test(repo)) {
      console.error('usage: contract-drift.js --repo OWNER/NAME|--input FILE [--contract F|--contract-json F] [--registry F] [--observed F] [--window 30d] [--limit N] [--dry-run] [--json] [--github-output]');
      process.exit(2);
    }
    model = collect(repo, { limit, windowMs, endMs });
  }
  const repo = a.repo || model.repo || process.env.GITHUB_REPOSITORY;
  if (!repo) {
    console.error('contract-drift: repo unknown — pass --repo or set model.repo');
    process.exit(2);
  }

  // Contract/registry overrides beat collected/model state.
  if (a['contract-json']) model.contract = JSON.parse(fs.readFileSync(a['contract-json'], 'utf8'));
  else if (a.contract) model.contract = yamlToJson(a.contract);
  if (a.registry) model.registry = JSON.parse(fs.readFileSync(a.registry, 'utf8'));
  if (a.observed) mergeObserved(model, JSON.parse(fs.readFileSync(a.observed, 'utf8')));
  if (a['dump-input']) fs.writeFileSync(a['dump-input'], `${JSON.stringify(model, null, 2)}\n`);

  const startMs = endMs - windowMs;
  const normalized = normalizeObserved(model, { startMs, endMs });
  const findings = evaluateDrift({ ...model, __normalized: normalized }, { startMs, endMs });

  const client = a.client || ghClient();
  const result = handleDrift({
    repo, findings, now: new Date(endMs), dryRun, labels: a.labels,
  }, client);
  result.findings = findings;
  result.checks_evaluated = new Set(findings.map((f) => f.check)).size;

  console.error(`[contract-drift] ${result.action} repo=${repo} findings=${findings.length} fingerprint=${result.fingerprint}${result.issueNumber ? ` issue=#${result.issueNumber}` : ''}`);
  if (a['github-output'] && process.env.GITHUB_OUTPUT) {
    fs.appendFileSync(process.env.GITHUB_OUTPUT,
      `drift_action=${result.action}\ndrift_issue=${result.issueNumber || ''}\ndrift_findings=${findings.length}\ndrift_fingerprint=${result.fingerprint}\n`);
  }
  if (a.json || a['github-output']) console.log(JSON.stringify(result, null, 2));
  if (a['dump-report']) fs.writeFileSync(a['dump-report'], `${JSON.stringify(result, null, 2)}\n`);
  return result;
}

if (require.main === module) {
  try {
    main(process.argv.slice(2));
  } catch (e) {
    console.error(`contract-drift: ${e.message}`);
    process.exit(2);
  }
}

module.exports = {
  DRIFT_LABEL,
  DEFAULT_LABELS,
  DEFAULT_WINDOW,
  AUTONOMY_LEVELS,
  STAGES,
  PIPELINE_BRANCH_RE,
  PIPELINE_LABEL,
  isPipelinePr,
  isMergedPr,
  parseDuration,
  contractEngineForStage,
  declaredEngines,
  forbiddenPairs,
  stageNameOfRun,
  stageOfCheckName,
  normalizeObserved,
  rollupCheckNames,
  rollupHasFailures,
  evaluateDrift,
  fingerprintFindings,
  repoSignature,
  signatureMarker,
  metaMarker,
  issueSignatureOf,
  findCanonical,
  storedFingerprint,
  filingBody,
  updateComment,
  resolvedComment,
  handleDrift,
  ghClient,
  yamlToJson,
  fetchContract,
  collect,
  mergeObserved,
  parseArgs,
  main,
};
