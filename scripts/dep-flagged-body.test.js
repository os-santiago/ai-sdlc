// scripts/dep-flagged-body.test.js — renderer tests for the dep-update
// flagged-issue body (issue #127).
'use strict';

const { describe, it } = require('node:test');
const assert = require('node:assert');

const { renderFlaggedBody, main, MARKER } = require('./dep-flagged-body');
const fs = require('fs');
const os = require('os');
const path = require('path');

describe('renderFlaggedBody', () => {
  it('renders header, one row per flagged candidate, and the dedup marker', () => {
    const body = renderFlaggedBody({
      flagged: [
        { name: 'lodash', current: '4.0.0', target: '5.0.0', kind: 'major', findings: [{ severity: 'high', detail: 'new maintainer' }] },
        { name: 'left-pad', current: '1.0.0', target: '1.0.1', kind: 'patch', findings: [{ severity: 'low', detail: 'x' }] },
        { name: 'ok-pkg', current: '1.0.0', target: '1.1.0', kind: 'minor', findings: [] },
      ],
    });
    assert.match(body, /^## Dependency updates flagged for human review/);
    assert.match(body, /- `lodash` 4\.0\.0 → 5\.0\.0 — major bump; high: new maintainer/);
    assert.match(body, /- `left-pad` 1\.0\.0 → 1\.0\.1 — low: x/);
    assert.match(body, /- `ok-pkg` 1\.0\.0 → 1\.1\.0 — policy flag/);
    assert.match(body, new RegExp(`${MARKER.replace(/[<>/-]/g, (c) => `\\${c}`)}\\n$`));
  });

  it('escapes Markdown metachars in untrusted fields', () => {
    const body = renderFlaggedBody({ flagged: [{ name: 'a`b|c', current: '1', target: '2', kind: 'major' }] });
    assert.match(body, /a\\`b\\|c/);
  });

  it('empty flagged list still renders header + marker', () => {
    const body = renderFlaggedBody({ flagged: [] });
    assert.match(body, /^## Dependency updates/);
    assert.match(body, /ai-sdlc:dep-flagged/);
  });
});

describe('main', () => {
  it('prints the body to stdout', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'dfb-'));
    const plan = path.join(dir, 'plan.json');
    fs.writeFileSync(plan, JSON.stringify({ flagged: [{ name: 'x', current: '1', target: '2', kind: 'major' }] }));
    let printed = '';
    main(['--plan', plan], { stdout: (s) => { printed += s; } });
    assert.match(printed, /- `x` 1 → 2 — major bump/);
  });
});
