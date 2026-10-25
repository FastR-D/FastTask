import assert from 'node:assert/strict';
import { test } from 'node:test';
import { FASTRESEARCH_WORKSPACE, launchModuleWorkspace, officialWorkspaceSystem, workspaceURL } from '../src/workspace-navigation.ts';

const pages = {fastwrite:'https://fastrd.io/write/',fastread:'https://fastrd.io/read/',fastnews:'https://47.110.133.67/news/',fastppt:'https://fastrd.io/fastppt/'};
const input = system => ({url:pages[system],system,ownerId:'ft-owner'});
function fixture(overrides = {}) {
  const events = [], hub = [], navigations = [];
  const popup = {opener:{},closed:false,location:{replace(url){navigations.push(url);events.push('navigate');}},close(){this.closed=true;events.push('close');}};
  const dependencies = {openPopup(){events.push('open');return popup;},assertOwner(){},readFastTaskIdentity:async()=>{events.push('ft-me');return {id:'ft-owner',identifier:'fastresearch:account-a'};},fetchHub:async(url,init)=>{
    events.push('hub');hub.push({url,init});
    return new Response(JSON.stringify(url.endsWith('/login-url')?{url:'https://fastrd.io/write/projects?token=fixture-ticket'}:{accountId:'account-a',session:'must-not-copy'}),{status:200});
  },...overrides};
  return {dependencies,events,hub,navigations,popup};
}

test('saved workspaces allow plain pages, reject identity/redirect selectors and unsafe URLs',()=>{
  assert.equal(workspaceURL('https://custom.example.org/read','fastread'),'https://custom.example.org/read');
  for(const url of ['https://fastrd.io/workspace?launch=fast-write','https://fastrd.io/write?key=secret','https://user@fastrd.io/write','javascript:alert(1)','https://fastrd.io/write#key','https://fastrd.io/\\evil'])assert.equal(workspaceURL(url,'fastwrite'),undefined,url);
  for(const [system,url]of Object.entries(pages))assert.equal(officialWorkspaceSystem(url,system),system);
  assert.equal(officialWorkspaceSystem(pages.fastwrite,'fastread'),undefined);
  assert.equal(officialWorkspaceSystem('https://custom.example.org/read','fastread'),undefined);
});

test('tab opens synchronously before identity requests; write uses the official cookie endpoints without FT Bearer',async()=>{
  const f=fixture();const pending=launchModuleWorkspace(input('fastwrite'),f.dependencies);
  assert.deepEqual(f.events,['open','ft-me']);assert.equal(f.popup.opener,null);
  await pending;
  assert.equal(f.hub.length,3);assert.equal(f.hub[0].url,'https://fastrd.io/api/content/me');assert.equal(f.hub[1].url,'https://fastrd.io/api/fastwrite/login-url');assert.equal(f.hub[2].url,f.hub[0].url);
  for(const call of f.hub){assert.equal(call.init.credentials,'include');assert.equal(call.init.redirect,'error');assert.equal(new Headers(call.init.headers).has('Authorization'),false);}
  assert.equal(f.hub[1].init.method,'POST');assert.equal(f.hub[1].init.body,'{}');
  assert.deepEqual(f.navigations,['https://fastrd.io/write/projects?token=fixture-ticket']);
});

test('read/news hand off through existing SSO audiences without next or credential query parameters',async()=>{
  for(const [system,audience]of [['fastread','fast-read'],['fastnews','fast-news']]){
    const f=fixture();await launchModuleWorkspace(input(system),f.dependencies);
    assert.equal(f.hub.length,1);const url=new URL(f.navigations[0]);assert.equal(url.origin,'https://fastrd.io');assert.equal(url.pathname,'/api/sso/launch');assert.deepEqual([...url.searchParams.entries()],[['audience',audience]]);
  }
});

test('ppt verifies the existing Hub identity and then opens its public catalog page',async()=>{
  const f=fixture();await launchModuleWorkspace(input('fastppt'),f.dependencies);assert.equal(f.hub.length,1);assert.deepEqual(f.navigations,[pages.fastppt]);
});

test('custom plain pages open directly without requesting either account service',async()=>{
  const f=fixture();await launchModuleWorkspace({url:'https://custom.example.org/read',system:'fastread',ownerId:'ft-owner'},f.dependencies);assert.deepEqual(f.events,['open','navigate']);assert.equal(f.hub.length,0);
});

test('Hub 401 opens the existing workspace for login and returns an actionable retry notice',async()=>{
  const f=fixture({fetchHub:async()=>new Response('{}',{status:401})});const result=await launchModuleWorkspace(input('fastwrite'),f.dependencies);
  assert.equal(result.loginRequired,true);assert.match(result.notice,/登录后.*重试/);assert.deepEqual(f.navigations,[FASTRESEARCH_WORKSPACE]);assert.equal(f.popup.closed,false);
});

test('linked account mismatch stops before write exchange or navigation',async()=>{
  const f=fixture({fetchHub:async()=>new Response(JSON.stringify({accountId:'other-account',session:'private'}),{status:200})});
  await assert.rejects(launchModuleWorkspace(input('fastwrite'),f.dependencies),/账号不一致/);assert.deepEqual(f.navigations,[]);assert.equal(f.popup.closed,true);
});

test('switching FT owner while identity or Hub validation is pending prevents module requests and navigation',async()=>{
  let changed=false,release;const waiting=new Promise(resolve=>{release=resolve;});
  const f=fixture({assertOwner(){if(changed)throw new Error('owner changed');},fetchHub:async()=>{await waiting;return new Response(JSON.stringify({accountId:'account-a'}),{status:200});}});
  const pending=launchModuleWorkspace(input('fastwrite'),f.dependencies);await new Promise(resolve=>setImmediate(resolve));changed=true;release();
  await assert.rejects(pending,/owner changed/);assert.deepEqual(f.navigations,[]);assert.equal(f.popup.closed,true);
});

test('outside-origin and prefix-confusion Write login URLs never reach a tab',async()=>{
  for(const target of ['https://evil.example/write/projects?token=secret','https://fastrd.io/write-evil/projects?token=secret','https://user@fastrd.io/write/projects?token=secret']){
    let count=0;const f=fixture({fetchHub:async()=>new Response(JSON.stringify(++count===1?{accountId:'account-a'}:{url:target}),{status:200})});
    await assert.rejects(launchModuleWorkspace(input('fastwrite'),f.dependencies),error=>!error.message.includes('secret'));assert.deepEqual(f.navigations,[]);
  }
});

test('changing the Hub cookie account during a write exchange discards its login URL',async()=>{
  let count=0;const f=fixture({fetchHub:async()=>new Response(JSON.stringify(++count===1?{accountId:'account-a'}:count===2?{url:'https://fastrd.io/write/projects?token=secret-ticket'}:{accountId:'account-b'}),{status:200})});
  await assert.rejects(launchModuleWorkspace(input('fastwrite'),f.dependencies),/登录账号已变化/);assert.deepEqual(f.navigations,[]);assert.equal(f.popup.closed,true);
});

test('timed-out requests close the waiting tab and report a Chinese retry message',async()=>{
  const f=fixture({fetchHub:async()=>{throw new DOMException('timeout','TimeoutError');}});
  await assert.rejects(launchModuleWorkspace(input('fastwrite'),f.dependencies),/连接 FastResearch 超时/);assert.equal(f.popup.closed,true);
});

test('offline and blocked-popup failures are visible and never leak tickets',async()=>{
  const offline=fixture({fetchHub:async()=>{throw new TypeError('Failed to fetch');}});await assert.rejects(launchModuleWorkspace(input('fastwrite'),offline.dependencies),/无法连接 FastResearch/);assert.equal(offline.popup.closed,true);
  const blocked=fixture({openPopup:()=>null});await assert.rejects(launchModuleWorkspace(input('fastwrite'),blocked.dependencies),/阻止了新标签页/);assert.deepEqual(blocked.hub,[]);
  const failed=fixture({fetchHub:async()=>new Response(JSON.stringify({error:'upstream token=secret-ticket failed'}),{status:503})});await assert.rejects(launchModuleWorkspace(input('fastwrite'),failed.dependencies),error=>error.message.includes('upstream')&&!error.message.includes('secret-ticket'));
});
