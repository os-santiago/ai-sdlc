const fs = require('node:fs');
const vm = require('node:vm');
const cases = JSON.parse(fs.readFileSync('/check/cases.json', 'utf8'));
const source = fs.readFileSync('/check/quarkus-app/src/main/resources/META-INF/resources/js/utils.js', 'utf8');
const results = cases.map(item => {
  const context = vm.createContext(Object.create(null), {codeGeneration:{strings:false, wasm:false}});
  vm.runInContext('var window={}; var document={documentElement:{lang:'+JSON.stringify(item.lang)+'}}; var navigator={language:'+JSON.stringify(item.browser)+'};', context, {timeout:500});
  vm.runInContext(source, context, {timeout:500});
  return vm.runInContext('window.HomeDirUtils.formatDate('+JSON.stringify(item.raw)+','+JSON.stringify(item.opts)+')', context, {timeout:500});
});
process.stdout.write(JSON.stringify(results));
