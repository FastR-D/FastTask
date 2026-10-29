import assert from 'node:assert/strict'
import { registerHooks } from 'node:module'
import { test } from 'node:test'
registerHooks({resolve(specifier,context,next){if(context.parentURL?.endsWith('/harness/owner.ts')&&specifier==='../session-owner')return next('../session-owner.ts',context);return next(specifier,context)}})
const {assertHarnessOwner}=await import('../src/harness/owner.ts')
const access=user=>`header.${btoa(JSON.stringify({sub:user}))}.signature`
test('a late grant cannot start after an account handoff, including mismatched access',async()=>{
 let owner='A';const expected=owner
 await Promise.resolve();owner='B'
 assert.throws(()=>assertHarnessOwner(expected,owner,access(owner)),/账号已变化/)
 assert.throws(()=>assertHarnessOwner('A','A',access('B')),/账号已变化/)
 assert.throws(()=>assertHarnessOwner('A',undefined,null),/账号已变化/)
 assert.doesNotThrow(()=>assertHarnessOwner('A','A',access('A')))
})
