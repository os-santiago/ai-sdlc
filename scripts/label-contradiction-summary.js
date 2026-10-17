#!/usr/bin/env node
// scripts/label-contradiction-summary.js — step-summary renderer for the
// label-contradiction repair job (issue #127).
//
// Extraction of the heredoc in ai-sdlc-sweep.yml's "Repair label
// contradictions on open issues" step: reshapes the JSON verdict of
// scripts/label-contradiction.js into the Markdown step-summary block.
// Pure function over the repairs record — no I/O beyond file read and the
// summary append.
//
// CLI:
//   node scripts/label-contradiction-summary.js --repairs repairs.json
//        [--summary FILE]
// The summary appends to --summary, else $GITHUB_STEP_SUMMARY, else stdout.
'use strict';

const fs = require('fs');

// Render the step-summary lines for one label-contradiction result record.
// Handles both shapes the repair script emits: a sweep record
// ({repairs: [...], scanned, contradictions, repaired}) and a single-issue
// record ({contradictions: truthy, issue, removed, vetoes, ...}).
function renderSummary(r) {
  const repairs = r.repairs || (r.contradictions ? [r] : []);
  const fixed = typeof r.repaired === 'number' ? r.repaired : (r.repaired ? 1 : 0);
  return [
    '### ai-sdlc label-contradiction repair',
    `issues scanned: ${r.scanned ?? 1} · contradictions: ${r.contradictions || 0} · repaired: ${fixed}`,
    ...repairs.map((x) => `- #${x.issue}: removed \`${(x.removed || []).join('`, `')}\` — vetoed by \`${(x.vetoes || []).join('`, `')}\`${x.escalated ? ' · escalated needs-human (label-drift-loop)' : ''}${x.error ? ` · ERROR: ${x.error}` : ''}`),
  ];
}

function parseArgs(argv) {
  const opts = { repairs: 'repairs.json' };
  for (let i = 0; i < argv.length; i += 1) {
    const a = argv[i];
    const next = () => {
      i += 1;
      if (i >= argv.length) throw new Error(`missing value for ${a}`);
      return argv[i];
    };
    if (a === '--repairs') opts.repairs = next();
    else if (a === '--summary') opts.summary = next();
    else if (a === '--help' || a === '-h') { console.log('node scripts/label-contradiction-summary.js [--repairs F] [--summary F]'); process.exit(0); }
    else throw new Error(`unknown argument: ${a}`);
  }
  return opts;
}

function main(argv, { env } = {}) {
  const opts = parseArgs(argv);
  const e = env || process.env;
  const record = JSON.parse(fs.readFileSync(opts.repairs, 'utf8'));
  const lines = renderSummary(record);
  const target = opts.summary || e.GITHUB_STEP_SUMMARY;
  if (target) fs.appendFileSync(target, lines.join('\n') + '\n');
  else console.log(lines.join('\n'));
  return lines;
}

if (require.main === module) {
  try {
    main(process.argv.slice(2));
  } catch (e) {
    console.error(`label-contradiction-summary: ${e.message}`);
    process.exit(2);
  }
}

module.exports = { renderSummary, parseArgs, main };
