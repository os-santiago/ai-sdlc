import fs from 'node:fs';
import path from 'node:path';
import http from 'node:http';
import {spawn,execFileSync} from 'node:child_process';
// Isolated SCC process fixture; this server never calls an external model.
const results=[];
for(const maxSteps of [16,24]) {
 const root=fs.mkdtempSync('/tmp/scc-budget-'),repo=path.join(root,'repository'),state=path.join(root,'state');
 fs.mkdirSync(repo);fs.mkdirSync(state);fs.mkdirSync(path.join(state,'home'));fs.mkdirSync(path.join(state,'tmp'));
 for(let n=0;n<18;n++)fs.writeFileSync(path.join(repo,`context-${n}.txt`),`Read-only fixture context ${n}\n`);
 execFileSync('git',['init','-q',repo]);execFileSync('git',['-C',repo,'add','.']);execFileSync('git',['-C',repo,'-c','user.name=Sergio Canales','-c','user.email=sergio.canales.e@gmail.com','commit','-qm','test: initialize isolated budget fixture']);
 let requests=0, inputBytes=0, outputBytes=0;
 const server=http.createServer((req,res)=>{let input='';req.on('data',d=>{input+=d;if(Buffer.byteLength(input)>131072)req.destroy();});req.on('end',()=>{
  if(!req.url.endsWith('/chat/completions')){res.writeHead(404);res.end();return;}
  inputBytes+=Buffer.byteLength(input);
  const turn=requests++, calls=[];
  if(turn<6)for(let n=0;n<3;n++)calls.push({id:`call-${turn}-${n}`,type:'function',function:{name:'read_file',arguments:JSON.stringify({path:`context-${turn*3+n}.txt`})}});
  if(turn===6)calls.push({id:'write-fixture',type:'function',function:{name:'write_file',arguments:JSON.stringify({path:'result.txt',content:'validated fixture\n'})}});
  const message={role:'assistant',content:calls.length?null:'Completed the requested fixture change.'};if(calls.length)message.tool_calls=calls;
  res.setHeader('Content-Type','application/json');const response=JSON.stringify({id:`fixture-${turn}`,object:'chat.completion',created:Math.floor(Date.now()/1000),model:'codex-cli',choices:[{index:0,message,finish_reason:calls.length?'tool_calls':'stop'}],usage:{prompt_tokens:100,completion_tokens:30,total_tokens:130}});outputBytes+=Buffer.byteLength(response);res.end(response);
 });});
 await new Promise(r=>server.listen(0,'127.0.0.1',r));
 const config={model:{provider:'openai-compatible',baseUrl:`http://127.0.0.1:${server.address().port}/v1`,apiKey:'offline-fixture-placeholder',model:'codex-cli',temperature:0.2,maxTokens:4096,stream:false},permissions:{autoApprove:['read_file','write_file']}};
 fs.writeFileSync(path.join(state,'config.json'),JSON.stringify(config));fs.writeFileSync(path.join(state,'prompt.txt'),'Read the fixture context and create result.txt with a greeting. Do not make commits.');
 const env={PATH:'/usr/local/bin:/usr/bin:/bin',HOME:path.join(state,'home'),TMPDIR:path.join(state,'tmp'),LC_ALL:'C.UTF-8',NO_COLOR:'1',CI:'1',SC_CONFIG_PATH:path.join(state,'config.json'),SC_BASE_URL:config.model.baseUrl,SC_MODEL:'codex-cli',NODE_OPTIONS:'--max-old-space-size=256',GIT_CONFIG_NOSYSTEM:'1',GIT_CONFIG_GLOBAL:'/dev/null',GIT_TERMINAL_PROMPT:'0'};
 const child=spawn('/usr/local/bin/scc',['chat','-yq','--prompt-file',path.join(state,'prompt.txt'),'--no-commit','--summary-file',path.join(state,'summary.json'),'--max-seconds','60','--max-steps',String(maxSteps)],{cwd:repo,env});
 let output='', exceeded=false;const collect=d=>{if(Buffer.byteLength(output)+d.length>262144){exceeded=true;child.kill('SIGKILL');}else output+=d;};child.stdout.on('data',collect);child.stderr.on('data',collect);
 const timer=setTimeout(()=>child.kill('SIGKILL'),75000);
 const exitCode=await new Promise(r=>child.on('close',r));clearTimeout(timer);await new Promise(r=>server.close(r));
 const summary=JSON.parse(fs.readFileSync(path.join(state,'summary.json'),'utf8'));
 results.push({maxSteps,exitCode,modelCalls:requests,inputBytes,outputBytes,outputBoundExceeded:exceeded,toolCalls:summary.tool_calls_total,exitReason:summary.exit_reason,budgetMarkerSteps:output.includes('SC_BUDGET_EXCEEDED steps'),candidateFileExists:fs.existsSync(path.join(repo,'result.txt')),candidateContentMatches:fs.existsSync(path.join(repo,'result.txt'))&&fs.readFileSync(path.join(repo,'result.txt'),'utf8')==='validated fixture\n'});fs.rmSync(root,{recursive:true,force:true});
}
console.log(JSON.stringify({status:results[0].exitCode===22&&results[0].budgetMarkerSteps&&results.slice(1).every(r=>r.exitCode===0)?'passed':'failed',scope:'isolated actual published SCC process with deterministic mock provider',liveWorkflowInvoked:false,results}));
