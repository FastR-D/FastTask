import { registerHooks } from 'node:module'
import { readFile } from 'node:fs/promises'
registerHooks({resolve(specifier,context,next){if(context.parentURL?.includes('/src/harness/')&&specifier.startsWith('./')&&!/\.[a-z]+$/i.test(specifier))return next(`${specifier}.ts`,context);return next(specifier,context)}})
const { createFxAgent }=await import('../../web/node_modules/libfx/node.js')
const { driveSharedRun }=await import('../../web/src/harness/run-driver.ts')
let raw='';for await(const chunk of process.stdin)raw+=chunk
const input=JSON.parse(raw)
const report=await driveSharedRun({serverOrigin:input.serverOrigin,createAgent:createFxAgent,call:async(method,path,token,body,signal)=>{
 const res=await fetch(`${input.serverOrigin}/api/v1${path}`,{method,signal,headers:{Authorization:`Bearer ${token}`,'Content-Type':'application/json'},...(body===undefined?{}:{body:JSON.stringify(body)})})
 const data=await res.json();if(!res.ok)throw Object.assign(new Error(data.detail??`HTTP ${res.status}`),{status:res.status});return data
}},input.request)
process.stdout.write(JSON.stringify(report))
