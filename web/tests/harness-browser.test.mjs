import assert from 'node:assert/strict';
import { spawn, spawnSync } from 'node:child_process';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { test } from 'node:test';
import { build } from 'vite';
const chrome=process.env.CHROME_BIN??'/usr/bin/chromium';
const available=spawnSync(chrome,['--version'],{encoding:'utf8'}).status===0;

test('real Chromium JSPI executes WASM and verified tools under the WASM CSP', {timeout:45000,skip:available?false:'Chromium unavailable; release gate requires an actual JSPI browser run'}, async()=>{
 const root=fileURLToPath(new URL('..',import.meta.url));const work=await mkdtemp(path.join(tmpdir(),'fasttask-wasm-'));let browser,ws;const waiting=new Map();
 try{
  await build({configFile:false,root,logLevel:'silent',build:{outDir:path.join(work,'assets'),emptyOutDir:true,minify:false,lib:{entry:path.join(root,'tests/harness-browser.ts'),formats:['iife'],name:'HarnessContract',fileName:()=> 'contract.js'}}});
  const js=await readFile(path.join(work,'assets/contract.js'),'utf8');const wasm=await readFile(path.join(root,'node_modules/libfx/fx-core.wasm'));
  browser=spawn(chrome,['--headless','--no-sandbox','--disable-gpu','--disable-background-networking','--disable-extensions','--disable-dev-shm-usage','--remote-debugging-port=0',`--user-data-dir=${path.join(work,'profile')}`,'about:blank'],{stdio:['ignore','ignore','pipe']});
  const endpoint=await new Promise((resolve,reject)=>{let log='';const timer=setTimeout(()=>reject(new Error('DevTools startup timeout')),10000);browser.stderr.on('data',b=>{log=(log+String(b)).slice(-16000);const match=log.match(/DevTools listening on (ws:\/\/[^\s]+)/);if(match){clearTimeout(timer);resolve(match[1]);}});browser.once('error',reject);});
  const debugOrigin=new URL(endpoint).origin.replace('ws:','http:');const targets=await(await fetch(`${debugOrigin}/json/list`)).json();const target=targets.find(t=>t.type==='page');assert.ok(target,'Chromium page target missing');
  ws=new WebSocket(target.webSocketDebuggerUrl);await new Promise((resolve,reject)=>{ws.addEventListener('open',resolve,{once:true});ws.addEventListener('error',reject,{once:true});});
  let seq=0;
  ws.addEventListener('message',event=>{const msg=JSON.parse(event.data);if(msg.id&&waiting.has(msg.id)){const job=waiting.get(msg.id);clearTimeout(job.timer);waiting.delete(msg.id);if(msg.error)job.reject(new Error(JSON.stringify(msg.error)));else job.resolve(msg.result);}});
  const command=(method,params={})=>new Promise((resolve,reject)=>{const id=++seq;const timer=setTimeout(()=>{waiting.delete(id);reject(new Error(`${method} timed out`));},25000);waiting.set(id,{resolve,reject,timer});ws.send(JSON.stringify({id,method,params}));});
  const evaluate=async expression=>{const result=await command('Runtime.evaluate',{expression,awaitPromise:true,returnByValue:true});assert.equal(result.exceptionDetails,undefined,JSON.stringify(result.exceptionDetails));return result.result?.value;};
  await command('Runtime.enable');
  // CDP injects the fixture bundle into an already-created blank page. Browser
  // networking is deliberately out of this test: Node's contract covers HTTP;
  // Chromium runs the actual WASM engine, SDK, shim, tools and CSP compile gate.
  await evaluate(`{const meta=document.createElement('meta');meta.httpEquiv='Content-Security-Policy';meta.content=${JSON.stringify("default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; connect-src 'self'")};document.head.append(meta);} true`);
  await evaluate(`globalThis.fixtureWasm=Uint8Array.from(atob(${JSON.stringify(wasm.toString('base64'))}),c=>c.charCodeAt(0)); globalThis.fixtureRequests=0; globalThis.fetch=async(input,options)=>{const url=String(input); if(url.endsWith('/fx-core.wasm'))return new Response(globalThis.fixtureWasm,{headers:{'Content-Type':'application/wasm'}}); if(!url.includes('/chat/completions'))throw new Error('unexpected fixture URL'); const body=JSON.parse(options.body); if(!body.messages)throw new Error('missing messages');const n=++globalThis.fixtureRequests;const delta=n===1?{tool_calls:[{index:0,id:'real-call',type:'function',function:{name:'lookup',arguments:'{"query":"fixture"}'}}]}:{content:'fixture complete'};return new Response('data: '+JSON.stringify({id:'fixture',choices:[{index:0,delta,finish_reason:null}]})+'\\n\\ndata: '+JSON.stringify({choices:[{index:0,delta:{},finish_reason:n===1?'tool_calls':'stop'}]})+'\\n\\ndata: [DONE]\\n\\n',{headers:{'Content-Type':'text/event-stream'}});};true`);
  await evaluate(js+'\n;true');
  const result=await evaluate('globalThis.contractResult');assert.equal(result?.pass,true,JSON.stringify({result,requests:await evaluate('globalThis.fixtureRequests')}));assert.equal(await evaluate('globalThis.fixtureRequests'),2);
  // A stricter subsequent policy must reject WebAssembly compilation. This
  // catches a DevTools fixture that accidentally bypasses the WASM CSP gate.
  const denied=await evaluate(`(async()=>{const meta=document.createElement('meta');meta.httpEquiv='Content-Security-Policy';meta.content="script-src 'self'";document.head.append(meta);try{await WebAssembly.compile(new Uint8Array([0,97,115,109,1,0,0,0]));return false;}catch{return true;}})()`);assert.equal(denied,true,'WASM CSP compile restriction was bypassed');
  process.stdout.write(`WASM contract: ${JSON.stringify(result)}; CSP deny verified\n`);
 }finally{
  for(const job of waiting.values())clearTimeout(job.timer);ws?.close();
  if(browser){browser.kill('SIGTERM');await new Promise(resolve=>{if(browser.exitCode!==null)resolve();else{const timer=setTimeout(()=>{browser.kill('SIGKILL');resolve();},2000);browser.once('exit',()=>{clearTimeout(timer);resolve();});}});}
  await rm(work,{recursive:true,force:true,maxRetries:5});
 }
});
