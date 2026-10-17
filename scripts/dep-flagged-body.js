#!/usr/bin/env node
// scripts/dep-flagged-body.js — flagged-update issue body renderer
// (issue #127)
//
// Extraction of the `Flag majors/findings for human review` heredoc from
// .github/workflows/ai-sdlc-dep-update.yml: renders the deduplicated
// escalation issue body (marker `<!-- ai-sdlc:dep-flagged -->`) from the
// dep-update worker's plan.json. Pure renderer — no I/O besides the plan
// file read and stdout write.
//
// CLI:
//   node scripts/dep-flagged-body.js [--plan plan.json]
// Prints the Markdown body to stdout (the workflow redirects it to a file).
'use strict';

const fs = require('fs');

const MARKER = '<!-- ai-sdlc:dep-flagged -->';

// Markdown-metachar escape — mirrors the heredoc's char set and 120-char
// cap so rendered bodies stay identical.
const esc = (s) => String(s == null ? '' : s).replace(/[<>`|\\\[\]]/g, (c) => '\\' + c).slice(0, 120);

function renderFlaggedBody(plan) {
  const lines = ['## Dependency updates flagged for human review', '',
    'The scheduled dep-update worker vetted these candidates and did **not** land them — majors, unknown specs, or supply-chain findings require human judgment (issue #177).', ''];
  for (const f of plan.flagged || []) {
    const why = [];
    if (!['patch', 'minor'].includes(f.kind)) why.push(`${f.kind} bump`);
    for (const g of f.findings || []) why.push(`${g.severity}: ${esc(g.detail)}`);
    lines.push(`- \`${esc(f.name)}\` ${esc(f.current)} → ${esc(f.target)} — ${why.join('; ') || 'policy flag'}`);
  }
  lines.push('', MARKER);
  return lines.join('\n') + '\n';
}

function parseArgs(argv) {
  const opts = { plan: 'plan.json' };
  for (let i = 0; i < argv.length; i += 1) {
    const a = argv[i];
    const next = () => {
      i += 1;
      if (i >= argv.length) throw new Error(`missing value for ${a}`);
      return argv[i];
    };
    if (a === '--plan') opts.plan = next();
    else if (a === '--help' || a === '-h') { console.log('node scripts/dep-flagged-body.js [--plan F]'); process.exit(0); }
    else throw new Error(`unknown argument: ${a}`);
  }
  return opts;
}

function main(argv, { stdout } = {}) {
  const opts = parseArgs(argv);
  const plan = JSON.parse(fs.readFileSync(opts.plan, 'utf8'));
  const body = renderFlaggedBody(plan);
  (stdout || ((s) => process.stdout.write(s)))(body);
  return body;
}

if (require.main === module) {
  try {
    main(process.argv.slice(2));
  } catch (e) {
    console.error(`dep-flagged-body: ${e.message}`);
    process.exit(2);
  }
}

module.exports = { MARKER, renderFlaggedBody, parseArgs, main };
