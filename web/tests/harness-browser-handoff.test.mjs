import assert from 'node:assert/strict'
import { spawn,spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import { test } from 'node:test'
const chrome=process.env.CHROME_BIN??'/usr/bin/chromium'
const available=spawnSync(chrome,['--version'],{encoding:'utf8'}).status===0
const root=fileURLToPath(new URL('../../',import.meta.url))
const run=input=>new Promise((resolve,reject)=>{
 const child=spawn(process.execPath,['evals/agent/browser.mjs'],{cwd:root,stdio:['pipe','pipe','pipe']});let output='',error=''
 const timer=setTimeout(()=>{child.kill('SIGKILL');reject(new Error('browser contract timed out'))},25000)
 child.stdout.on('data',b=>{output+=b});child.stderr.on('data',b=>{error+=b});child.once('error',reject);child.once('close',code=>{clearTimeout(timer);if(code!==0){reject(new Error(error));return}try{resolve(JSON.parse(output))}catch(e){reject(e)}})
 child.stdin.end(JSON.stringify({...input,serverOrigin:'http://fixture.test',request:{}}))
})
test('real browser discards a late grant after the account changes', {timeout:30000,skip:!available},async()=>{
 const result=await run({ownerHandoff:true});assert.equal(result.owner_handoff_passed,true,JSON.stringify(result));assert.equal(result.model_requests,0);assert.equal(result.callbacks,0);assert.equal(result.status.running,false)
})
test('real browser stop button shows failed delivery and permits a retry', {timeout:30000,skip:!available},async()=>{
 const result=await run({cancellationFeedback:true});assert.equal(result.cancellation_feedback_passed,true,JSON.stringify(result));assert.equal(result.failure_notice_visible,true);assert.equal(result.failed_delivery_did_not_confirm_server,true);assert.equal(result.retry_requests,2)
})
