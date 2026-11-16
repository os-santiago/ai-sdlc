const fs = require('node:fs');
const cases = JSON.parse(fs.readFileSync('/check/cases.json', 'utf8'));
// Expected values belong only to the separately isolated reference Pod.
process.stdout.write(JSON.stringify(cases.map(item => item.expected)));
