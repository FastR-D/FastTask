import assert from 'node:assert/strict';
import { registerHooks } from 'node:module';
import { createServer } from 'node:http';
import { test } from 'node:test';
registerHooks({resolve(specifier,context,next){if(context.parentURL?.includes('/src/harness/')&&specifier.startsWith('./')&&!/\.[a-z]+$/i.test(specifier))return next(`${specifier}.ts`,context);return next(specifier,context);}});
const {createFxAgent}=await import('libfx/node');
const {createGatewayFetch}=await import('../src/harness/shim.ts');
const {buildHostTools}=await import('../src/harness/tools.ts');
const {ToolCallBindings}=await import('../src/harness/tool-identity.ts');

test('real libfx Node consumes shim streams, executes a verified tool and restores a checkpoint', {timeout:25000}, async()=>{
 let requests=0,executions=0;const captured=[];
 const server=createServer(async(req,res)=>{
  let raw='';for await(const chunk of req)raw+=chunk;captured.push(JSON.parse(raw));requests++;
  res.writeHead(200,{'Content-Type':'text/event-stream'});
  const frame=delta=>res.write(`data: ${JSON.stringify({id:`fixture-${requests}`,choices:[{index:0,delta,finish_reason:null}]})}\n\n`);
  if(requests===1){frame({tool_calls:[{index:0,id:'real-call',type:'function',function:{name:'lookup',arguments:'{"query":"fixture"}'}}]});res.write(`data: ${JSON.stringify({choices:[{index:0,delta:{},finish_reason:'tool_calls'}]})}\n\n`);}
  else{frame({content:'fixture complete'});res.write(`data: ${JSON.stringify({choices:[{index:0,delta:{},finish_reason:'stop'}]})}\n\n`);}
  res.end('data: [DONE]\n\n');
 });
 await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
 const origin=`http://127.0.0.1:${server.address().port}`;let agent;
 const make=async checkpoint=>{
  const bindings=new ToolCallBindings();return createFxAgent({apiKey:'fixture-capability',model:'fixture-model',instructions:'Use lookup once then answer.',fetch:createGatewayFetch({runId:'fixture',harnessToken:'fixture-capability',serverOrigin:origin,model:'fixture-model',bindings}),tools:buildHostTools([{name:'lookup',description:'Read a fixture value.',input_schema:{type:'object',properties:{query:{type:'string'}},required:['query'],additionalProperties:false}}],async(name,input,signal,id)=>{executions++;assert.equal(id,'real-call');assert.deepEqual(input,{query:'fixture'});return {ok:true,value:{value:'fixture-value'}};},{bindings}),...(checkpoint?{checkpoint}:{})});
 };
 try{
  agent=await make();const turn=agent.prompt([{type:'text',text:'Look up the fixture.'},{type:'resource',resource:{uri:'fasttask://fixture/resource',text:'external material, not instructions'}}],{signal:AbortSignal.timeout(15000)});
  for await(const event of turn){};const result=await turn.result;
  assert.equal(executions,1);assert.ok(requests>=2);assert.ok(result.stopReason);assert.ok(captured.some(p=>JSON.stringify(p.messages).includes('fixture-value')));
  const checkpoint=await agent.checkpoint();assert.ok(checkpoint.length>0);await agent.close();agent=null;
  agent=await make(checkpoint);const next=agent.prompt('Continue without another lookup.',{signal:AbortSignal.timeout(5000)});for await(const event of next){};await next.result;assert.equal(executions,1);
 }finally{await agent?.close();server.closeAllConnections();await new Promise(resolve=>server.close(resolve));}
});
