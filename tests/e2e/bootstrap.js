// tests/e2e/bootstrap.js
// Simulated `gh ai-sdlc init` output for the bootstrap e2e test (issue
// #208, epic #153). The installer itself ships separately (issue #185,
// os-santiago/gh-ai-sdlc); this module builds the payload the documented
// path produces — docs/onboarding.md "What the bootstrap PR carries" —
// and drives a mockable `gh` layer so the payload shape can be asserted
// end-to-end with zero network.
//
// CLI:
//   node tests/e2e/bootstrap.js --repo owner/name --dir <fixture> --gh <bin>
//
// Writes the PR-carried files into <dir> (the simulated target repo),
// then invokes `<gh> pr create --repo owner/name --title ... --label
// bootstrap --body-file ...` through the binary named by --gh (default
// "gh"; tests point it at a recording stub). Prints the payload as JSON
// on stdout.

'use strict';

const fs = require('node:fs');
const path = require('node:path');
const { spawnSync } = require('node:child_process');

// The starter contract, exactly as docs/onboarding.md "The trust ramp"
// shows `gh ai-sdlc init` writing it: lowest rung of spec/trust-ladder.md.
function starterContract() {
  return [
    '# .ai-sdlc.yaml — as written by gh ai-sdlc init',
    'spec: "1.0.0"',
    'autonomy:',
    '  level: shadow   # observe-only: reports, never mutates',
    '',
  ].join('\n');
}

// Thin caller workflow the bootstrap PR carries — mirrors the caller
// block in README.md / docs/runtime.md ("Adopt this runtime").
function callerWorkflow() {
  return [
    'name: ai-sdlc',
    'on:',
    '  issues: { types: [labeled] }',
    '',
    'permissions:',
    '  contents: write',
    '  issues: write',
    '  pull-requests: write',
    '',
    'jobs:',
    '  intake:',
    "    if: github.event.label.name == 'ready-to-implement'",
    '    uses: os-santiago/ai-sdlc/.github/workflows/ai-sdlc-intake.yml@main',
    '    with:',
    '      repo: ${{ github.repository }}',
    '      issue_number: ${{ github.event.issue.number }}',
    '    secrets: inherit',
    '',
    '  implement:',
    '    needs: intake',
    "    if: needs.intake.outputs.dispatch == 'true'",
    '    uses: os-santiago/ai-sdlc/.github/workflows/ai-sdlc-implement.yml@main',
    '    with:',
    '      repo: ${{ github.repository }}',
    '      issue_number: ${{ github.event.issue.number }}',
    '    secrets: inherit',
    '',
  ].join('\n');
}

// Canonical label set the bootstrap PR creates — docs/onboarding.md:
// "ready-to-implement, scc:* run states, pr:risk-* tiers, needs-human,
// skip labels (spec/labels.md)".
function labelBootstrap() {
  return {
    trigger: ['ready-to-implement'],
    refinement: ['needs-refinement'],
    run_states: [
      'scc:queued',
      'scc:in-progress',
      'scc:pr-opened',
      'scc:failed',
      'scc:not-ready',
      'scc:no-changes',
    ],
    risk_tiers: [
      'pr:risk-low',
      'pr:risk-medium',
      'pr:risk-high',
      'pr:risk-critical',
      'pr:risk-accepted',
    ],
    holds: ['needs-human'],
    skip: [
      'epic',
      'wontfix',
      'invalid',
      'duplicate',
      'question',
      'dependencies',
      'umbrella',
      'scc:child',
    ],
  };
}

// Secrets checklist the PR body walks through — docs/onboarding.md table.
const SECRETS_CHECKLIST = [
  ['AI_SDLC_APP_ID', 'yes', 'GitHub App id; every mutation stage mints a per-run installation token'],
  ['AI_SDLC_APP_PRIVATE_KEY', 'yes', 'GitHub App private key (fail-closed — no PAT/GITHUB_TOKEN fallback)'],
  ['MODEL_API_KEY', 'yes', 'API key for the resolved provider endpoint'],
  ['AI_SDLC_PROVIDER_URL', 'yes (repo variable)', 'OpenAI-compatible inference endpoint the engine resolves'],
  ['AI_SDLC_OFF', 'optional (repo variable)', '=1 halts dispatch per run — the emergency stop'],
  ['AI_SDLC_FREEZE', 'optional', 'contract-named freeze env; pins the effective level to shadow while set'],
];

// The full bootstrap PR payload for a target repo, per docs/onboarding.md.
function buildBootstrapPR(repoFullName) {
  const labels = labelBootstrap();
  const allLabels = Object.values(labels).flat();
  const body = [
    `Onboards \`${repoFullName}\` onto the AI-SDLC runtime.`,
    '',
    'Merging this PR wires the contract, the caller workflow, and the',
    'canonical label set. The repo enters the trust ladder at `shadow`',
    'and earns every level above it on run evidence',
    '(spec/trust-ladder.md).',
    '',
    '## Files',
    '',
    '- `.ai-sdlc.yaml` — starter contract, `autonomy.level: shadow`',
    '- `.github/workflows/ai-sdlc.yml` — thin caller dispatching the reusable stages',
    '',
    '## Labels this PR creates',
    '',
    ...allLabels.map((l) => `- \`${l}\``),
    '',
    '## Secrets checklist',
    '',
    '| Secret / variable | Required | Purpose |',
    '|---|---|---|',
    ...SECRETS_CHECKLIST.map(([name, req, purpose]) => `| \`${name}\` | ${req} | ${purpose} |`),
    '',
    '## Post-merge',
    '',
    'Install the `ai-sdlc` GitHub App, set the secrets above, then label a',
    'well-scoped issue `ready-to-implement`. At `shadow` the pipeline',
    'reports what it *would* do and mutates nothing.',
    '',
  ].join('\n');

  return {
    title: `chore(ai-sdlc): onboard ${repoFullName}`,
    pr_labels: ['bootstrap'],
    label_set: allLabels,
    files: {
      '.ai-sdlc.yaml': starterContract(),
      '.github/workflows/ai-sdlc.yml': callerWorkflow(),
    },
    body,
  };
}

// Materialize the payload into a fixture repo dir and invoke the gh layer.
// Returns { payload, ghArgv, ghStatus }. `ghBin` is any executable — the
// test passes a recording stub, so no network is ever touched. `env` adds
// or overrides variables for the gh subprocess (e.g. the stub's log path).
function runBootstrapInit({ repo, dir, ghBin = 'gh', env = {} }) {
  const payload = buildBootstrapPR(repo);
  fs.mkdirSync(dir, { recursive: true });
  for (const [rel, content] of Object.entries(payload.files)) {
    const abs = path.join(dir, rel);
    fs.mkdirSync(path.dirname(abs), { recursive: true });
    fs.writeFileSync(abs, content);
  }
  const bodyFile = path.join(dir, '.bootstrap-pr-body.md');
  fs.writeFileSync(bodyFile, payload.body);

  const ghArgv = [
    'pr', 'create',
    '--repo', repo,
    '--title', payload.title,
    ...payload.pr_labels.flatMap((l) => ['--label', l]),
    '--body-file', bodyFile,
  ];
  const res = spawnSync(ghBin, ghArgv, {
    encoding: 'utf8',
    env: { ...process.env, ...env },
  });
  return { payload, ghArgv, ghStatus: res.status, ghStderr: res.stderr };
}

function main(argv) {
  const args = { gh: 'gh' };
  for (let i = 0; i < argv.length; i += 2) {
    args[argv[i].replace(/^--/, '')] = argv[i + 1];
  }
  if (!args.repo || !args.dir) {
    console.error('usage: bootstrap.js --repo owner/name --dir <fixture> [--gh <bin>]');
    process.exit(2);
  }
  const { payload, ghStatus, ghStderr } = runBootstrapInit({
    repo: args.repo,
    dir: args.dir,
    ghBin: args.gh,
  });
  console.log(JSON.stringify({ repo: args.repo, dir: args.dir, ghStatus, payload }, null, 2));
  if (ghStatus !== 0) {
    console.error(`gh layer exited ${ghStatus}: ${ghStderr || ''}`);
    process.exit(1);
  }
}

if (require.main === module) {
  main(process.argv.slice(2));
}

module.exports = {
  starterContract,
  callerWorkflow,
  labelBootstrap,
  buildBootstrapPR,
  runBootstrapInit,
};
