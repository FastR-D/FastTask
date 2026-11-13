import assert from 'node:assert/strict';
import { test } from 'node:test';
import { createComponentHarness,deferred,nodes,text } from './helpers/component-harness.mjs';

const payload=(week='2026-W46',title='真实进展')=>({week,timezone:'Asia/Shanghai',start_date:'2026-11-09',end_date:'2026-11-15',items:[{id:week,source:'fasttask',kind:'result',title,summary:'真实记录的摘要',status:'completed',occurred_at:'2026-11-12T12:00:00Z'}],focus_minutes:20,counts:{result:1},upcoming:[],warnings:[],total:1,has_more:false,next_offset:0,complete:true});
async function fixture(handler) {
  const calls=[];
  const h=await createComponentHarness(new URL('../src/Review.tsx',import.meta.url),'ReviewWorkspace',{mocks:{
    './api':{request:async(path,init,owner)=>{calls.push({path,owner,signal:init?.signal});const result=handler?.(path,init,owner);if(result!==undefined)return result;if(path.startsWith('/reviews/activity'))return {data:payload(new URL(path,'https://local').searchParams.get('week'))};throw Error(path)}},
    './Lens':{Review:()=>null,isoWeekOf:()=> '2026-W46',shiftWeek:(week,delta)=>delta<0?'2026-W45':'2026-W47'},
    './date':{localDateInTimezone:()=> '2026-11-12'},
  }});
  const props={userId:'owner-a',timezone:'Asia/Shanghai',refreshVersion:0,onNotice(){},view:'activity'};
  h.render(props);await h.flush();
  return {h,calls,props,button:label=>nodes(h.output,n=>n.type==='button'&&text(n)===label)[0],input:label=>nodes(h.output,n=>n.props['aria-label']===label)[0]};
}
test('out-of-order week responses cannot overwrite selected week and requests bind owner',async()=>{
  const old=deferred();
  const f=await fixture(path=>path.includes('/reviews/activity?week=2026-W46')?old.promise:undefined);
  f.input('复盘周次').props.onChange({target:{value:'2026-W45'}});await f.h.flush();
  assert.equal(f.input('复盘周次').props.value,'2026-W45');
  old.resolve({data:payload('2026-W46','过期的响应')});await f.h.flush();
  assert.equal(nodes(f.h.output,n=>n.type?.name==='OutcomeCard').some(n=>n.props.item.title==='过期的响应'),false);assert.ok(f.calls.every(call=>call.owner==='owner-a'));
  assert.equal(f.calls.find(call=>call.path.includes('2026-W46')).signal.aborted,true);f.h.unmount();
});
test('regeneration preserves edits until explicit replacement and permits restoration',async()=>{
  const f=await fixture();f.h.render({...f.props,view:'report'});await f.h.flush();
  f.button('生成周报草稿').props.onClick();await f.h.flush();
  assert.match(f.input('周报草稿').props.value,/真实进展/);
  f.input('周报草稿').props.onChange({target:{value:'手工补充不能丢失'}});await f.h.flush();
  f.button('从最新任务记录重新生成').props.onClick();await f.h.flush();
  assert.equal(f.input('周报草稿').props.value,'手工补充不能丢失');
  f.button('用新草稿替换').props.onClick();await f.h.flush();
  assert.match(f.input('周报草稿').props.value,/真实进展/);
  f.button('恢复替换前的草稿').props.onClick();await f.h.flush();
  assert.equal(f.input('周报草稿').props.value,'手工补充不能丢失');f.h.unmount();
});
test('review tabs keep unique stable sibling identities and retain the weekly draft',async()=>{
  // The hook harness does not reconcile DOM children. Check the actual parent's
  // reconciliation keys as well as the child's state across the parent events:
  // duplicate sibling keys previously left stale overview DOM in the browser.
  function ReviewOverview(){return {type:'section',props:{className:'review-page'}}}
  const hub=await createComponentHarness(new URL('../src/Review.tsx',import.meta.url),'Review',{
    mocks:{'./Lens':{Review:ReviewOverview},'./api':{},'./date':{}},
  });
  const f=await fixture();
  let workspaceKey,overviewKey;
  async function checkView(view){
    const panels=nodes(hub.output,n=>n.type?.name==='ReviewWorkspace');
    const overviews=nodes(hub.output,n=>n.type===ReviewOverview);
    assert.equal(panels.length,1,'keep a single mounted workspace');
    assert.equal(overviews.length,view==='overview'?1:0,'only render the current overview');
    const siblings=nodes(hub.output,n=>typeof n.type==='function');
    assert.equal(new Set(siblings.map(n=>n.key)).size,siblings.length,'sibling components need unique keys');
    workspaceKey??=panels[0].key;
    assert.equal(panels[0].key,workspaceKey,'tab changes must not remount the draft owner');
    if(overviews.length){overviewKey??=overviews[0].key;assert.equal(overviews[0].key,overviewKey)}
    assert.equal(panels[0].props.view,view);
    f.h.render(panels[0].props);await f.h.flush();
    assert.equal(nodes(f.h.output,n=>n.props.className==='review-workspace')[0].props.hidden,view==='overview');
  }
  async function select(label,view){
    nodes(hub.output,n=>n.type==='button'&&text(n)===label)[0].props.onClick();
    await hub.flush();await checkView(view);
  }
  try{
    hub.render(f.props);await hub.flush();await checkView('overview');
    await select('周报','report');
    f.button('生成周报草稿').props.onClick();await f.h.flush();
    assert.match(f.input('周报草稿').props.value,/真实进展/);
    f.input('周报草稿').props.onChange({target:{value:'跨子页保留的手工周报'}});await f.h.flush();
    for(let cycle=0;cycle<3;cycle++){
      await select('成果汇总','activity');
      await select('复盘概览','overview');
      await select('周报','report');
      assert.equal(f.input('周报草稿').props.value,'跨子页保留的手工周报');
    }
  }finally{hub.unmount();f.h.unmount()}
});

test('task review and weekly report never request connections or external outcomes',async()=>{
  const f=await fixture();
  f.h.render({...f.props,view:'report'});await f.h.flush();
  f.button('生成周报草稿').props.onClick();await f.h.flush();
  assert.match(f.input('周报草稿').props.value,/真实进展/);
  f.button('刷新记录').props.onClick();await f.h.flush();
  assert.ok(f.calls.length>=2);
  assert.ok(f.calls.every(call=>call.path.startsWith('/reviews/activity?')));
  assert.doesNotMatch(text(f.h.output),/管理成果来源|FastRead|FastWrite|待连接|所有来源/);
  f.h.unmount();
});
test('legacy imported records never appear in task cards or weekly drafts',async()=>{
  const f=await fixture(path=>path.startsWith('/reviews/activity')?{data:{...payload(),items:[...payload().items,
    {id:'external',source:'fastread',kind:'result',title:'外部私有内容',status:'completed'},
    {id:'material',source:'fasttask',kind:'material',title:'已导入的材料',status:'recorded',time_basis:'imported_at'},
  ]}}:undefined);
  assert.equal(nodes(f.h.output,n=>n.type?.name==='OutcomeCard').length,1);
  f.h.render({...f.props,view:'report'});await f.h.flush();
  f.button('生成周报草稿').props.onClick();await f.h.flush();
  assert.doesNotMatch(f.input('周报草稿').props.value,/外部私有内容|已导入的材料/);
  f.h.unmount();
});
test('weekly report waits for task records and paginates local history without duplicate cards',async()=>{
  const pending=deferred();
  const f=await fixture(path=>path.includes('offset=0')?pending.promise:{data:{...payload(),items:[...payload().items,{...payload().items[0],id:'second',title:'第二条进展'}]}});
  f.h.render({...f.props,view:'report'});await f.h.flush();
  assert.equal(f.button('生成周报草稿').props.disabled,true);
  pending.resolve({data:{...payload(),has_more:true,next_offset:100}});await f.h.flush();
  assert.equal(f.button('生成周报草稿').props.disabled,false);
  f.button('载入更多任务记录').props.onClick();await f.h.flush();
  assert.equal(nodes(f.h.output,n=>n.type?.name==='OutcomeCard').length,2);
  assert.ok(f.calls.every(call=>call.path.startsWith('/reviews/activity?')));
  f.h.unmount();
});
