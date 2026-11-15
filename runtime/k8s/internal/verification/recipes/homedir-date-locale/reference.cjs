const fs = require('node:fs');
const cases = JSON.parse(fs.readFileSync('/check/cases.json', 'utf8'));
const presets = {date:{year:'numeric',month:'short',day:'numeric'}, datetime:{month:'short',day:'2-digit',hour:'2-digit',minute:'2-digit'}, short:{month:'short',day:'2-digit'}};
const results = cases.map(item => {
  if (!item.raw) return '';
  try {
    const date = new Date(item.raw);
    if (isNaN(date.getTime())) return '';
    const options = typeof item.opts === 'string' ? presets[item.opts] || presets.date : item.opts || presets.date;
    return options.hour || options.minute ? date.toLocaleString(item.locale, options) : date.toLocaleDateString(item.locale, options);
  } catch (_) { return ''; }
});
process.stdout.write(JSON.stringify(results));
