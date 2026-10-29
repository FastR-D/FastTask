import assert from 'node:assert/strict';
import { registerHooks } from 'node:module';
import { test } from 'node:test';
registerHooks({resolve(specifier, context, next){if(context.parentURL?.includes('/src/harness/') && specifier.startsWith('./') && !/\.[a-z]+$/i.test(specifier))return next(`${specifier}.ts`,context);return next(specifier,context);}});
const {driveSharedRun}=await import('../src/harness/run-driver.ts');
const {driveSidecarRun}=await import('../src/harness/sidecar-driver.ts');

function fixture(overrides={}){
 const events=[],calls=[];let closed=0;
 const deps={serverOrigin:'http://fixture.invalid',call:async(method,path,token,body,signal)=>{calls.push({method,path,token,body,signal});if(path.endsWith('/heartbeat'))return {harness_token:'rotated'};if(path==='/agent/tools')return {tools:[]};return {};},createAgent:async options=>({prompt(input,{signal}){events.push({input,signal,options});return {result:Promise.resolve({stopReason:'stop',usage:{inputTokens:3,outputTokens:2}}),cancel(){},async *[Symbol.asyncIterator](){yield {type:'text_delta',delta:'ok'};}}},checkpoint:async()=>new Uint8Array([1,2,3]),close:async()=>{closed++;}}),...overrides};
 return {deps,events,calls,get closed(){return closed;}};
}
const request={run_id:'run-1',thread_id:'thread-1',harness_token:'cap',prompt:'canonical',model:'fixture',instructions:'rules'};
test('browser and sidecar use the same driver and propagate canonical input, token rotation and usage',async()=>{
 assert.equal(driveSharedRun,driveSidecarRun);
 const f=fixture();const report=await driveSharedRun(f.deps,request);
 assert.equal(report.stop_reason,'stop');assert.equal(report.checkpoint,'AQID');assert.deepEqual(report.usage,{inputTokens:3,outputTokens:2});assert.equal(f.events[0].input,'canonical');assert.equal(f.events[0].options.apiKey,'rotated');assert.equal(f.closed,1);assert.ok(f.calls.every(c=>c.signal instanceof AbortSignal));assert.equal(f.calls.filter(c=>c.method==='PUT').length,0,'adapter completes checkpoint exactly once');
});
test('shared driver reports initialization errors and closes any created agent',async()=>{
 const f=fixture({createAgent:async()=>{throw new Error('fixture initialization failed');}});const report=await driveSharedRun(f.deps,request);assert.equal(report.stop_reason,'error');assert.match(report.error_message,/initialization/);assert.equal(f.closed,0);
});
test('an already cancelled run never starts an agent',async()=>{
 let created=false;const controller=new AbortController();controller.abort();
 const f=fixture({signal:controller.signal,createAgent:async()=>{created=true;throw new Error('should not start');}});const report=await driveSharedRun(f.deps,request);assert.equal(created,false);assert.equal(report.stop_reason,'cancelled');
});
