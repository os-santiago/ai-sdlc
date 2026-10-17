#!/usr/bin/env node
/**
 * tools/ai-sdlc-init.js — repo installer for the AI-SDLC pipeline (issue #185).
 *
 * The shared engine behind `gh ai-sdlc init` (the os-santiago/gh-ai-sdlc
 * extension) and the dispatchable installer workflow: run inside a checkout
 * of the target repo — or pass its path — and it writes:
 *
 *   .ai-sdlc.yaml                     starter contract, autonomy.level: shadow
 *   .github/workflows/ai-sdlc.yml     thin caller → reusable stages @<ref>
 *   .github/workflows/ai-sdlc-sweep.yml  vendored stall-sweep re-pointed at
 *                                      os-santiago/ai-sdlc automerge (the
 *                                      consumer pattern docs/runtime.md
 *                                      prescribes)
 *
 * The generated contract is validated against spec/ai-sdlc.schema.json with
 * tools/validate-ai-sdlc-config.sh BEFORE anything is written — a contract
 * that fails the schema or secret scan never lands. The caller workflow
 * carries a `labels-bootstrap` job (workflow_dispatch only) that creates the
 * canonical label set after merge: the bootstrap PR creates everything the
 * pipeline needs — the installer never writes labels via the API.
 *
 * Usage:
 *   node tools/ai-sdlc-init.js [target-dir] [flags]
 *
 * Flags:
 *   --level L        autonomy ceiling: shadow|suggest|auto-pr|auto-merge-low|
 *                    auto-merge-all|auto-deploy (default: shadow)
 *   --archetype A    repo archetype (default: app)
 *   --language L     primary language hint (default: other)
 *   --base B         base/default branch for triggers (default: origin/HEAD
 *                    when detectable, else main)
 *   --runtime R      runtime repo the caller pins (default: os-santiago/ai-sdlc)
 *   --ref R          ref of the runtime to pin (default: main — pin a tag for
 *                    stability)
 *   --no-sweep       do not vendor the stall-sweep workflow
 *   --pr             after writing, branch + commit + push + open the
 *                    bootstrap PR via git/gh (requires both on PATH and an
 *                    authenticated gh)
 *   --force          overwrite existing files that differ
 *   --dry-run        print the plan and validation result; write nothing
 *   --help           this text
 *
 * Idempotent: a re-run reports `unchanged` for identical files and `kept`
 * for divergent ones (nothing is overwritten without --force). Exit codes:
 * 0 ok, 1 validation/runtime failure, 2 usage error.
 *
 * @contract
 * purpose: onboard a repo to the AI-SDLC pipeline — starter contract + thin
 *          caller workflow (+ vendored sweep), schema-validated before write.
 * reads:   [target dir contents, .github/workflows/ai-sdlc-sweep.yml from the
 *           runtime checkout this script ships in, git remote HEAD (--base)]
 * writes:  [.ai-sdlc.yaml + .github/workflows/*.yml in the target; with --pr:
 *           branch/commit/push/PR on the target's origin via git+gh]
 * calls:   [tools/validate-ai-sdlc-config.sh via bash; git + gh when --pr]
 * invokedBy: [manual CLI, gh-ai-sdlc extension, installer workflow]
 * failureModes: [existing divergent files are kept unless --force, dry-run
 *                writes nothing, missing validator/schema = exit 2]
 */

const fs = require('fs');
const path = require('path');
const os = require('os');
const { spawnSync } = require('child_process');

const RUNTIME_DIR = path.resolve(__dirname, '..');
const SCHEMA = path.join(RUNTIME_DIR, 'spec', 'ai-sdlc.schema.json');
const VALIDATOR = path.join(RUNTIME_DIR, 'tools', 'validate-ai-sdlc-config.sh');
const SWEEP_SOURCE = path.join(RUNTIME_DIR, '.github', 'workflows', 'ai-sdlc-sweep.yml');

const DEFAULT_RUNTIME = 'os-santiago/ai-sdlc';
const DEFAULT_REF = 'main';
const LEVELS = ['shadow', 'suggest', 'auto-pr', 'auto-merge-low', 'auto-merge-all', 'auto-deploy'];
const ARCHETYPES = ['library', 'service', 'app', 'tooling', 'docs', 'infra'];
const LANGUAGES = ['javascript', 'typescript', 'python', 'go', 'rust', 'java', 'shell', 'other'];

// Canonical label set the caller's labels-bootstrap job creates post-merge
// (spec/labels.md bindings + risk taxonomy). Bootstrap creates missing labels
// only — existing labels are never modified (no --force clobbering).
const LABELS = [
  ['ready-to-implement', '0e8a16', 'Dispatch trigger — issue is DoR-ready for the ai-sdlc pipeline'],
  ['needs-refinement', 'd4c5f9', 'DoR hold — needs a clearer spec before dispatch'],
  ['needs-human', 'b60205', 'Human hold — vetoes dispatch and merge while present'],
  ['ai-sdlc', '1d76db', 'Opt-in — marks a human-authored PR pipeline-managed'],
  ['scc:queued', 'fbca04', 'Run state — run queued'],
  ['scc:in-progress', 'fbca04', 'Run state — run in progress'],
  ['scc:pr-opened', '0e8a16', 'Run state — run opened a PR'],
  ['scc:failed', 'd93f0b', 'Run state — run failed (see issue comments)'],
  ['scc:not-ready', 'd4c5f9', 'DoR/config hold — refinement required'],
  ['scc:no-changes', 'd4c5f9', 'Run state — finished with no workspace mutations'],
  ['pr:risk-low', '0e8a16', 'Risk tier: low'],
  ['pr:risk-medium', 'fbca04', 'Risk tier: medium'],
  ['pr:risk-high', 'd93f0b', 'Risk tier: high'],
  ['pr:risk-critical', 'b60205', 'Risk tier: critical — merge requires pr:risk-accepted'],
  ['pr:risk-accepted', '5319e7', 'Human acceptance of a critical-risk PR'],
];

// ---------------------------------------------------------------------------
// Generated artifacts (pure functions — unit-testable)
// ---------------------------------------------------------------------------

function starterConfig({ level = 'shadow', archetype = 'app', language = 'other' } = {}) {
  return `# .ai-sdlc.yaml — AI-SDLC pipeline contract for this repo.
# Generated by \`ai-sdlc init\` (issue #185). Validate against the spec:
#   tools/validate-ai-sdlc-config.sh .ai-sdlc.yaml spec/ai-sdlc.schema.json
# (run from a checkout of os-santiago/ai-sdlc — the schema + validator live
# there).
spec: "1.0.0"

repo:
  archetype: ${archetype}      # library | service | app | tooling | docs | infra
  primary_language: ${language}   # javascript | typescript | python | go | rust | java | shell | other
  deploys_to: none

# Trust ladder (spec/trust-ladder.md): level is a CEILING — the pipeline may
# run below it, never above. Starts at shadow: report-only, zero mutations.
# Promotion arrives as a reviewed contract PR once run evidence passes the
# gates declared in promotion: below — evidence accrues from the first run.
autonomy:
  level: ${level}
  merge_requires_checks: true
  freeze_env: AI_SDLC_FREEZE   # set this env/var to pin the effective level to shadow

# Self-accrediting ramp (spec/trust-ladder.md): the promotion gate measures
# clean merges over the rolling window of completed pipeline runs.
promotion:
  clean_merges_threshold: 3
  window: 30

demotion:
  escalation_threshold: 1      # needs-human escalations in the window → drop one level
  window: 30

triggers:
  issue_label: ready-to-implement
  skip_labels:
    - epic
    - wontfix
    - invalid
    - duplicate
    - question
    - dependencies
    - needs-human
  authorized_labelers: []      # logins whose labels arm dispatch; empty = anyone

verify:
  commands: []                 # e.g. [npm ci, npm test] — must stay green
  required_checks: []
  timeout_seconds: 600

review:
  mode: ai                     # ai | human | hybrid
  max_fix_iterations: 3        # bounded review→fix convergence, then needs-human
  wait_max_minutes: 15         # pending-verdict cap before review_timeout merge
  risk_labels:
    - pr:risk-low
    - pr:risk-medium
    - pr:risk-high
    - pr:risk-critical
    - pr:risk-accepted
  critical_requires_acceptance: true

merge:
  method: squash
  delete_branch: true
  max_repairs: 3
  pending_max_minutes: 60
  grace_minutes: 10
  skip_labels:
    - needs-human
    - hold
    - do-not-merge
    - wip

labels:
  queued: scc:queued
  in-progress: scc:in-progress
  pr-opened: scc:pr-opened
  failed: scc:failed
  not-ready: scc:not-ready
  no-changes: scc:no-changes

# Inference for engine runs on GitHub Actions: endpoint from repo variable
# AI_SDLC_PROVIDER_URL, key from secret MODEL_API_KEY (GitHub Models was
# retired 2026-07-30). OmniRoute routes (auto/*, devin/*) are control-plane
# only — unreachable from GHA runners (ADR-0001). Adjust the ids to what
# your provider serves.
model:
  primary: nvidia/nemotron-3-super-120b-a12b
  fallback: nvidia/nemotron-3.5-lightning-30b-a3b

engine:
  name: scc                    # sc-agent-cli — the certified reference engine
  max_steps: 200
  max_seconds: 900

escalation:
  label: needs-human
  contacts: []                 # e.g. ["@your-handle"] — notified on needs-human
  notify_channels: []
`;
}

function callerWorkflow({ runtime = DEFAULT_RUNTIME, ref = DEFAULT_REF, base = 'main' } = {}) {
  const uses = (wf) => `${runtime}/.github/workflows/${wf}@${ref}`;
  const labelSteps = LABELS
    .map(([n, c, d]) => `            create ${JSON.stringify(n)} ${c} ${JSON.stringify(d)}`)
    .join('\n');
  return `# .github/workflows/ai-sdlc.yml — thin caller for the AI-SDLC pipeline.
# Generated by \`ai-sdlc init\`; reusable stages live in ${runtime}
# (pinned @${ref} — pin a tag for stability). Docs: docs/runtime.md +
# docs/onboarding.md in that repo.
name: ai-sdlc

on:
  issues:
    types: [labeled]
  pull_request:
    types: [opened, synchronize]
  check_run:
    types: [completed]
  push:
    branches: [${base}]
  workflow_dispatch:
    inputs:
      pr:
        description: 'PR number to re-drive through the merge gate'
        type: number
        required: false

# Called stages declare their own job permissions; a caller grant can narrow
# them, never widen — this union covers every stage (runtime.md §Inputs).
permissions:
  contents: write
  pull-requests: write
  issues: write
  checks: read
  actions: read
  statuses: read
  models: read

jobs:
  # One-shot label bootstrap (spec/labels.md): run once after merge —
  # Actions → ai-sdlc → Run workflow. Creates missing labels only; existing
  # labels are never modified. The bootstrap PR carries this job, so no
  # direct API label writes happen before merge.
  labels-bootstrap:
    if: github.event_name == 'workflow_dispatch'
    runs-on: ubuntu-latest
    timeout-minutes: 5
    permissions:
      issues: write
    steps:
      - name: Create canonical ai-sdlc labels (idempotent)
        env:
          GH_TOKEN: \${{ github.token }}
        run: |
          set -uo pipefail
          create() {
            gh label create "$1" --color "$2" --description "$3" >/dev/null 2>&1 \\
              && echo "created: $1" || echo "kept:    $1 (already exists)"
          }
${labelSteps}

  # Intake: 'ready-to-implement' → DoR check → scc:queued. Keep the label
  # name in sync with triggers.issue_label in .ai-sdlc.yaml.
  intake:
    if: github.event_name == 'issues' && github.event.label.name == 'ready-to-implement'
    uses: ${uses('ai-sdlc-intake.yml')}
    with:
      repo: \${{ github.repository }}
      issue_number: \${{ github.event.issue.number }}
    secrets: inherit

  implement:
    needs: intake
    if: needs.intake.outputs.dispatch == 'true'
    uses: ${uses('ai-sdlc-implement.yml')}
    with:
      repo: \${{ github.repository }}
      issue_number: \${{ github.event.issue.number }}
      ledger_head: \${{ needs.intake.outputs.ledger_head }}
    secrets: inherit

  # Deterministic pr:risk-* labeling on every same-repo PR head.
  classify:
    if: >-
      github.event_name == 'pull_request' &&
      github.event.pull_request.head.repo.full_name == github.repository
    uses: ${uses('ai-sdlc-classify.yml')}
    with:
      repo: \${{ github.repository }}
      pr_number: \${{ github.event.pull_request.number }}
    secrets: inherit

  # AI review + bounded review-fix convergence. Fork heads carry no
  # secrets — filtered here so they skip cleanly.
  review:
    if: >-
      github.event_name == 'pull_request' &&
      github.event.pull_request.head.repo.full_name == github.repository
    uses: ${uses('ai-sdlc-review.yml')}
    with:
      repo: \${{ github.repository }}
      pr_number: \${{ github.event.pull_request.number }}
    secrets: inherit

  # Merge gate: on PR events, on check completion, and manually
  # (Actions → ai-sdlc → Run workflow with a PR number).
  automerge:
    if: >-
      (github.event_name == 'pull_request' &&
       github.event.pull_request.head.repo.full_name == github.repository) ||
      (github.event_name == 'check_run' &&
       github.event.check_run.pull_requests[0] != null) ||
      (github.event_name == 'workflow_dispatch' && github.event.inputs.pr)
    uses: ${uses('ai-sdlc-automerge.yml')}
    with:
      repo: \${{ github.repository }}
      pr_number: \${{ github.event.inputs.pr || github.event.pull_request.number || github.event.check_run.pull_requests[0].number }}
    secrets: inherit

  # Bounded CI repair on red checks associated with a PR.
  ci-repair:
    if: >-
      github.event_name == 'check_run' &&
      github.event.check_run.conclusion == 'failure' &&
      github.event.check_run.pull_requests[0] != null
    uses: ${uses('ai-sdlc-ci-repair.yml')}
    with:
      repo: \${{ github.repository }}
      pr_number: \${{ github.event.check_run.pull_requests[0].number }}
    secrets: inherit

  # Post-merge verify of the contract's verify.commands on the base branch.
  verify:
    if: github.event_name == 'push'
    uses: ${uses('ai-sdlc-verify.yml')}
    with:
      repo: \${{ github.repository }}
      base: ${base}
    secrets: inherit
`;
}

// The stall-sweep is a top-level workflow (schedule + its own discovery job),
// not a workflow_call — consumers vendor it and re-point the gate job at the
// cross-repo automerge (docs/runtime.md §Consuming repos). Read the runtime
// copy so the vendored file never drifts from the source.
function sweepWorkflow({ runtime = DEFAULT_RUNTIME, ref = DEFAULT_REF, sweepSource = SWEEP_SOURCE } = {}) {
  let src;
  try {
    src = fs.readFileSync(sweepSource, 'utf8');
  } catch {
    return null;
  }
  const rel = 'uses: ./.github/workflows/ai-sdlc-automerge.yml';
  if (!src.includes(rel)) return null;
  src = src.replace(rel, `uses: ${runtime}/.github/workflows/ai-sdlc-automerge.yml@${ref}`);
  const header = `# Vendored from ${runtime}@${ref} by \`ai-sdlc init\` (issue #185) —
# re-pointed at the cross-repo automerge gate per docs/runtime.md
# (§Stall-sweep → Consuming repos). Re-run init to refresh.
`;
  return header + src;
}

function prBody({ runtime = DEFAULT_RUNTIME, ref = DEFAULT_REF, level = 'shadow' } = {}) {
  return `## AI-SDLC onboarding

Adds the AI-SDLC pipeline to this repo — **merging this PR is the onboarding act**.

### What this adds
- \`.ai-sdlc.yaml\` — declarative contract, ceiling pinned at \`autonomy.level: ${level}\` (observe-only; promotion arrives as a reviewed contract PR once run evidence passes the gates — spec/trust-ladder.md)
- \`.github/workflows/ai-sdlc.yml\` — thin caller dispatching the reusable stages in ${runtime}@${ref} (intake → implement → review → automerge → ci-repair → verify) plus a manual labels-bootstrap job
- \`.github/workflows/ai-sdlc-sweep.yml\` — vendored stall-sweep re-pointed at the runtime automerge gate

### Action needed before first use
- [ ] Install the **ai-sdlc GitHub App** on this repo (or org) and configure secrets — scoped org secrets are the intended setup:
  - \`AI_SDLC_APP_ID\` + \`AI_SDLC_APP_PRIVATE_KEY\` — **required**; every mutation stage mints a per-run App installation token (fail-closed, no PAT/GITHUB_TOKEN fallback)
  - \`MODEL_API_KEY\` — **required for inference**; API key for the resolved provider endpoint
  - \`AI_SDLC_PROVIDER_URL\` (repo **variable**) — OpenAI-compatible inference endpoint
  - optional: \`AI_SDLC_TOKEN\` (legacy slot), \`AI_SDLC_LEDGER_KEY\` (ledger signing), \`AI_SDLC_OFF=1\` variable (emergency stop), \`AI_SDLC_FREEZE\` (pins effective level to shadow)
- [ ] Bootstrap labels once: **Actions → ai-sdlc → Run workflow** (the \`labels-bootstrap\` job creates the canonical set — creates only, never modifies existing)
- [ ] Label a well-scoped issue \`ready-to-implement\` — at \`${level}\` the pipeline reports what it *would* do and mutates nothing

> Secretless end-state (design, issue #185): a shared public App whose tokens
> are OIDC-minted per run — the org installs the App and manages **zero**
> secrets. Self-managed per-org secrets remain the path for regulated or
> air-gapped environments.

_Generated by ai-sdlc init · ${runtime}@${ref} · closes the bootstrap loop for this repo_`;
}

function nextSteps({ dir, wrote, prUrl, level, runtime, ref }) {
  const lines = [
    '',
    'Next steps:',
  ];
  if (prUrl) {
    lines.push(`  1. Review and merge the bootstrap PR: ${prUrl}`);
  } else {
    lines.push(
      '  1. Review the generated files, then commit + open + merge a PR —',
      '     merging is the onboarding act (re-run with --pr to have init open it).',
    );
  }
  lines.push(
    '  2. Install the ai-sdlc GitHub App on the repo (or org) and set:',
    '       secrets   AI_SDLC_APP_ID + AI_SDLC_APP_PRIVATE_KEY  (required — per-run App tokens)',
    '       secret    MODEL_API_KEY                             (required for inference)',
    '       variable  AI_SDLC_PROVIDER_URL                      (OpenAI-compatible endpoint)',
    '       optional  AI_SDLC_TOKEN, AI_SDLC_LEDGER_KEY, AI_SDLC_OFF=1, AI_SDLC_FREEZE',
    '     Secretless end-state (issue #185): a shared public App with OIDC-minted',
    '     per-run tokens — org installs the App, manages zero secrets.',
    '  3. Bootstrap labels: Actions → ai-sdlc → Run workflow (labels-bootstrap job).',
    `  4. Label a well-scoped issue 'ready-to-implement' — at '${level}' the`,
    '     pipeline reports what it would do and mutates nothing.',
    '',
    `Runtime pinned: ${runtime}@${ref} · docs: docs/onboarding.md + docs/runtime.md there.`,
  );
  if (!wrote) lines.push('', '(dry-run — nothing written)');
  return lines.join('\n');
}

// ---------------------------------------------------------------------------
// Installer
// ---------------------------------------------------------------------------

function detectBaseBranch(dir, deps) {
  const run = deps.spawnSync || spawnSync;
  const r = run('git', ['symbolic-ref', '--short', 'refs/remotes/origin/HEAD'], {
    cwd: dir, encoding: 'utf8', timeout: 10000,
  });
  if (r.status === 0 && r.stdout) {
    const b = r.stdout.trim().replace(/^origin\//, '');
    if (/^[A-Za-z0-9._-]+$/.test(b)) return b;
  }
  return 'main';
}

function validateContract(yaml, deps) {
  const run = deps.spawnSync || spawnSync;
  if (!fs.existsSync(VALIDATOR)) return { ok: false, code: 2, output: `validator not found: ${VALIDATOR}` };
  if (!fs.existsSync(SCHEMA)) return { ok: false, code: 2, output: `schema not found: ${SCHEMA}` };
  const tmp = path.join(os.tmpdir(), `ai-sdlc-init-${process.pid}-${Date.now()}.yaml`);
  try {
    fs.writeFileSync(tmp, yaml);
    const r = run('bash', [VALIDATOR, tmp, SCHEMA], { encoding: 'utf8', timeout: 120000 });
    return { ok: r.status === 0, code: r.status, output: `${r.stdout || ''}${r.stderr || ''}`.trim() };
  } finally {
    try { fs.unlinkSync(tmp); } catch { /* best effort */ }
  }
}

function planFiles(opts, deps) {
  const cfg = starterConfig(opts);
  const caller = callerWorkflow(opts);
  const files = [
    { rel: '.ai-sdlc.yaml', content: cfg },
    { rel: path.join('.github', 'workflows', 'ai-sdlc.yml'), content: caller },
  ];
  if (opts.sweep !== false) {
    const sweep = sweepWorkflow(opts);
    if (sweep) files.push({ rel: path.join('.github', 'workflows', 'ai-sdlc-sweep.yml'), content: sweep });
    else files.push({ rel: path.join('.github', 'workflows', 'ai-sdlc-sweep.yml'), content: null });
  }
  return files;
}

function applyPlan(dir, files, { force = false, dryRun = false } = {}) {
  const out = [];
  for (const f of files) {
    const abs = path.join(dir, f.rel);
    if (f.content == null) {
      out.push({ rel: f.rel, status: 'skipped', note: 'sweep source unavailable in this runtime checkout' });
      continue;
    }
    const exists = fs.existsSync(abs);
    if (exists) {
      const cur = fs.readFileSync(abs, 'utf8');
      if (cur === f.content) {
        out.push({ rel: f.rel, status: 'unchanged' });
        continue;
      }
      if (!force) {
        out.push({ rel: f.rel, status: 'kept', note: 'exists and differs — pass --force to overwrite' });
        continue;
      }
    }
    if (dryRun) {
      out.push({ rel: f.rel, status: exists ? 'would overwrite (--force)' : 'would write' });
      continue;
    }
    fs.mkdirSync(path.dirname(abs), { recursive: true });
    fs.writeFileSync(abs, f.content);
    out.push({ rel: f.rel, status: exists ? 'overwritten' : 'written' });
  }
  return out;
}

function openPr(dir, opts, deps) {
  const run = deps.spawnSync || spawnSync;
  const env = Object.assign({}, process.env);
  const runGit = (a) => {
    const r = run('git', a, { cwd: dir, encoding: 'utf8', env, timeout: 120000 });
    if (r.error) throw r.error;
    if (r.status !== 0) throw new Error(`git ${a[0]} failed (${r.status}): ${(r.stderr || '').slice(0, 300)}`);
    return (r.stdout || '').trim();
  };
  const runGh = (a) => {
    const r = run('gh', a, { cwd: dir, encoding: 'utf8', env, timeout: 120000 });
    if (r.error) throw r.error;
    if (r.status !== 0) throw new Error(`gh ${a[0]} failed (${r.status}): ${(r.stderr || '').slice(0, 300)}`);
    return (r.stdout || '').trim();
  };

  const branch = 'chore/ai-sdlc-onboarding';
  runGit(['rev-parse', '--is-inside-work-tree']);
  runGit(['checkout', '-b', branch]);
  runGit(['add', '.ai-sdlc.yaml', '.github/workflows/ai-sdlc.yml', '.github/workflows/ai-sdlc-sweep.yml']);
  runGit(['-c', 'user.name=ai-sdlc[bot]', '-c', 'user.email=ai-sdlc[bot]@users.noreply.github.com',
    'commit', '-m',
    `chore(ai-sdlc): onboard repo — contract + caller workflow\n\nStarter .ai-sdlc.yaml (autonomy.level: ${opts.level || 'shadow'}), thin\ncaller workflow → ${opts.runtime || DEFAULT_RUNTIME}@${opts.ref || DEFAULT_REF}, vendored stall-sweep.\nGenerated by tools/ai-sdlc-init.js (issue #185).`]);
  runGit(['push', '-u', 'origin', branch]);
  const bodyFile = path.join(dir, '.ai-sdlc-pr-body.md');
  fs.writeFileSync(bodyFile, prBody(opts));
  try {
    return runGh(['pr', 'create', '--base', opts.base || 'main', '--head', branch,
      '--title', 'chore(ai-sdlc): onboard to the AI-SDLC pipeline', '--body-file', bodyFile]);
  } finally {
    try { fs.unlinkSync(bodyFile); } catch { /* best effort */ }
  }
}

function init(opts = {}, deps = {}) {
  const dir = path.resolve(opts.dir || process.cwd());
  if (!fs.existsSync(dir) || !fs.statSync(dir).isDirectory()) {
    throw new Error(`target directory not found: ${dir}`);
  }
  if (!opts.base) opts.base = detectBaseBranch(dir, deps);

  const cfg = starterConfig(opts);
  const v = validateContract(cfg, deps);
  const log = deps.log || console.log;
  if (v.output) log(v.output);
  if (!v.ok) {
    const err = new Error(`generated contract failed validation (exit ${v.code})`);
    err.code = v.code;
    throw err;
  }
  log(`[✓] generated .ai-sdlc.yaml conforms to spec/ai-sdlc.schema.json`);

  const files = planFiles(opts, deps);
  const results = applyPlan(dir, files, { force: !!opts.force, dryRun: !!opts.dryRun });
  for (const r of results) {
    const tag = { written: '[+]', overwritten: '[~]', unchanged: '[=]', kept: '[!]', skipped: '[-]' }[r.status] || '[ ]';
    log(`${tag} ${r.rel} — ${r.status}${r.note ? ` (${r.note})` : ''}`);
  }

  let prUrl = null;
  if (opts.pr && !opts.dryRun) {
    const blocked = results.filter((r) => r.status === 'kept');
    if (blocked.length) {
      throw new Error(`--pr refused: ${blocked.map((b) => b.rel).join(', ')} diverge (pass --force or resolve manually)`);
    }
    prUrl = openPr(dir, opts, deps);
    log(`[✓] bootstrap PR → ${prUrl}`);
  }

  log(nextSteps({ dir, wrote: !opts.dryRun, prUrl, level: opts.level || 'shadow', runtime: opts.runtime || DEFAULT_RUNTIME, ref: opts.ref || DEFAULT_REF }));
  return { dir, results, prUrl, validation: v };
}

// ---------------------------------------------------------------------------
// CLI
// ---------------------------------------------------------------------------

const USAGE = `Usage: node tools/ai-sdlc-init.js [target-dir] [flags]

  --level L        autonomy ceiling (default: shadow)
  --archetype A    library|service|app|tooling|docs|infra (default: app)
  --language L     javascript|typescript|python|go|rust|java|shell|other
  --base B         base branch (default: origin/HEAD, else main)
  --runtime R      runtime repo (default: ${DEFAULT_RUNTIME})
  --ref R          runtime ref to pin (default: ${DEFAULT_REF})
  --no-sweep       skip the vendored stall-sweep workflow
  --pr             branch + commit + push + open the bootstrap PR (needs git+gh)
  --force          overwrite existing files that differ
  --dry-run        plan + validate only; write nothing
  --help           this text`;

function parseArgs(argv) {
  const opts = {};
  const positional = [];
  const take = (i) => {
    if (i + 1 >= argv.length) throw new Error(`missing value for ${argv[i]}`);
    return argv[i + 1];
  };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    switch (a) {
      case '--help': case '-h': opts.help = true; break;
      case '--dry-run': opts.dryRun = true; break;
      case '--force': opts.force = true; break;
      case '--pr': opts.pr = true; break;
      case '--no-sweep': opts.sweep = false; break;
      case '--level': opts.level = take(i); i++; break;
      case '--archetype': opts.archetype = take(i); i++; break;
      case '--language': opts.language = take(i); i++; break;
      case '--base': opts.base = take(i); i++; break;
      case '--runtime': opts.runtime = take(i); i++; break;
      case '--ref': opts.ref = take(i); i++; break;
      case '--dir': opts.dir = take(i); i++; break;
      default:
        if (a.startsWith('--')) throw new Error(`unknown flag: ${a}`);
        positional.push(a);
    }
  }
  if (positional.length > 1) throw new Error(`unexpected arguments: ${positional.slice(1).join(' ')}`);
  if (!opts.dir && positional.length) opts.dir = positional[0];
  if (opts.level && !LEVELS.includes(opts.level)) {
    throw new Error(`invalid --level '${opts.level}' (allowed: ${LEVELS.join(', ')})`);
  }
  if (opts.archetype && !ARCHETYPES.includes(opts.archetype)) {
    throw new Error(`invalid --archetype '${opts.archetype}' (allowed: ${ARCHETYPES.join(', ')})`);
  }
  if (opts.language && !LANGUAGES.includes(opts.language)) {
    throw new Error(`invalid --language '${opts.language}' (allowed: ${LANGUAGES.join(', ')})`);
  }
  return opts;
}

function main(argv) {
  let opts;
  try {
    opts = parseArgs(argv);
  } catch (e) {
    console.error(`[X] ${e.message}\n\n${USAGE}`);
    process.exit(2);
  }
  if (opts.help) { console.log(USAGE); process.exit(0); }
  try {
    init(opts);
  } catch (e) {
    console.error(`[X] init failed: ${e.message}`);
    process.exit(1);
  }
}

if (require.main === module) main(process.argv.slice(2));

module.exports = {
  starterConfig, callerWorkflow, sweepWorkflow, prBody, nextSteps,
  detectBaseBranch, validateContract, planFiles, applyPlan, init, parseArgs,
  LEVELS, ARCHETYPES, LANGUAGES, LABELS, USAGE,
};
