import React from '../../web/node_modules/react/index.js'
import { createRoot } from '../../web/node_modules/react-dom/client.js'
import { RestoreScopeSummary } from '../../web/src/agent/tools/RestoreScopeSummary'
import { createFxAgent } from '../../web/src/harness/runtime'
import { driveSharedRun } from '../../web/src/harness/run-driver'
import { driveRun, harnessStatus, cancelActiveRun } from '../../web/src/harness/host'
import { stopWithFeedback } from '../../web/src/harness/cancellation'
import { token } from '../../web/src/api'

const fixture=globalThis as typeof globalThis & {fixtureInput: {serverOrigin:string;request:Parameters<typeof driveSharedRun>[1];ownerHandoff?:boolean;cancellationFeedback?:boolean;restoreScope?:unknown};fixtureRun:Promise<unknown>}
fixture.fixtureRun=(async()=>{
 const input=fixture.fixtureInput
 if(input.restoreScope){
  const container=document.createElement('div');document.body.append(container);const root=createRoot(container);root.render(React.createElement(RestoreScopeSummary,{value:input.restoreScope}));
  for(let tries=0;tries<100&&!container.querySelector('section');tries++)await new Promise(resolve=>setTimeout(resolve,10))
  const text=container.textContent??'';const count=(input.restoreScope as {count:number}).count;const tasks=(input.restoreScope as {tasks:{title:string}[]}).tasks
  const passed=text.includes(`实际恢复 ${count} 项`)&&tasks.every(task=>text.includes(task.title))&&text.includes('其他删除批次保持原样')
  root.unmount();return {restore_scope_card_passed:passed,visible_text:text}
 }
 if(input.cancellationFeedback){
  token.set(`header.${btoa(JSON.stringify({sub:'A',exp:Date.now()/1000+3600}))}.signature`)
  let requests=0,serverStopped=false
  globalThis.fetch=async()=>{requests++;if(requests===1)throw new TypeError('fixture connection lost');serverStopped=true;return new Response('{}',{headers:{'Content-Type':'application/json'}})}
  const button=document.createElement('button'),notice=document.createElement('output');button.textContent='停止';document.body.append(button,notice)
  let delivered=false
  button.onclick=()=>{void stopWithFeedback(()=>cancelActiveRun('fixture-run','A'),message=>{notice.textContent=message}).then(value=>{delivered=value;button.dataset.done=String(requests)})}
  const click=async()=>{button.click();while(button.dataset.done!==String(requests)||requests===0)await new Promise(resolve=>setTimeout(resolve,5))}
  await click();const failedVisible=notice.textContent?.includes('未送达')===true,notConfirmed=!delivered&&!serverStopped
  button.dataset.done='';await click()
  return {cancellation_feedback_passed:failedVisible&&notConfirmed&&delivered&&serverStopped&&requests===2,failure_notice_visible:failedVisible,failed_delivery_did_not_confirm_server:notConfirmed,retry_requests:requests,success_reports_delivery_only:delivered}
 }
 if(input.ownerHandoff){
  let current='A',modelRequests=0,grants=0,callbacks=0
  const access=(sub:string)=>`header.${btoa(JSON.stringify({sub,exp:Date.now()/1000+3600}))}.signature`
  token.set(access('A'))
  globalThis.fetch=async(url)=>{
   const path=String(url)
   if(path.endsWith('/agent/tools'))return new Response(JSON.stringify({tools:[]}),{headers:{ETag:'fixture','Content-Type':'application/json'}})
   if(path.endsWith('/agent/runs')){grants++;current='B';token.set(access('B'));return new Response(JSON.stringify({harness_token:'old-A-capability',thread_id:'A-thread',libfx_version:'0.0.10',prompt:'A prompt'}),{headers:{'Content-Type':'application/json'}})}
   modelRequests++;throw new Error('old owner must not drive a new host')
  }
  let refused=false
  try{await driveRun({ownerId:'A',getOwner:()=>current,runId:'A-run',text:'A prompt',forcedMode:'wasm',onDone:()=>{callbacks++}})}catch{refused=true}
  return {owner_handoff_passed:grants===1&&modelRequests===0&&callbacks===0&&!harnessStatus().running&&harnessStatus().runId===undefined,model_requests:modelRequests,grants,callbacks,refused,status:harnessStatus()}
 }
 return driveSharedRun({serverOrigin:input.serverOrigin,createAgent:options=>createFxAgent({...options,wasm:'http://fixture.test/fx-core.wasm'}),call:async(method,path,cap,body,signal)=>{
  const response=await fetch(`${input.serverOrigin}/api/v1${path}`,{method,signal,headers:{Authorization:`Bearer ${cap}`,'Content-Type':'application/json'},...(body===undefined?{}:{body:JSON.stringify(body)})});const data=await response.json();if(!response.ok)throw Object.assign(new Error(data.detail??`HTTP ${response.status}`),{status:response.status});return data
 }},input.request)
})()
