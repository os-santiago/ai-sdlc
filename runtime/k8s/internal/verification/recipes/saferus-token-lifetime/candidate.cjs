const fs = require('node:fs');
const vm = require('node:vm');
const cases = JSON.parse(fs.readFileSync('/check/cases.json', 'utf8'));
const source = fs.readFileSync('/check/web/static/ttl.js', 'utf8');
async function main() {
  const results = [];
  for (const item of cases) {
    const context = vm.createContext(Object.create(null), {codeGeneration: {strings: false, wasm: false}});
    const module = new vm.SourceTextModule(source, {context});
    await module.link(() => { throw new Error('Imports are outside this recipe'); });
    await module.evaluate({timeout: 500});
    if (typeof module.namespace.formatTTL !== 'function') throw new Error('Formatter export required');
    Object.defineProperty(context, 'formatTTL', {value: module.namespace.formatTTL});
    const special = {nan: 'NaN', infinity: 'Infinity', undefined: 'undefined'};
    const value = item.kind ? special[item.kind] : JSON.stringify(item.input);
    if (typeof value !== 'string') throw new Error('Invalid trusted case');
    const result = vm.runInContext('formatTTL(' + value + ')', context, {timeout: 500});
    if (typeof result !== 'string' || result.length > 1024) throw new Error('Bounded string required');
    results.push(result);
  }
  process.stdout.write(JSON.stringify(results));
}
main().catch(() => { process.stderr.write('Formatter verification failed\n'); process.exitCode = 1; });
