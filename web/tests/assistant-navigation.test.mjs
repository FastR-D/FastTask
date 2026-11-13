import assert from 'node:assert/strict';
import { test } from 'node:test';
import { createComponentHarness, nodes, eventTarget } from './helpers/component-harness.mjs';

async function fixture() {
  const calls=[];
  const pane={scrollIntoView:options=>calls.push(['scroll',options.block]),focus:options=>calls.push(['focus',options.preventScroll])};
  const workspace={clientWidth:1000};
  const separator={getBoundingClientRect:()=>({width:8})};
  const harness=await createComponentHarness(new URL('../src/ResearchWorkspace.tsx',import.meta.url),'ResearchWorkspace',{
    mocks:{'./agent':{AgentChat:()=>null}},
    globals:{window:eventTarget({localStorage:{getItem:()=>null,setItem:()=>{}}}),ResizeObserver:class{observe(){} disconnect(){}}},
    commit(tree) {
      for(const node of nodes(tree,n=>n.props.ref)) {
        node.props.ref.current=node.props.className==='research-workspace'?workspace:node.props.className==='research-chat'?pane:separator;
      }
    },
  });
  const props={children:null,assistantOpen:true,assistantFocusRequest:0,onAssistantClose:()=>{},onNotice:()=>{},onBusinessChanged:()=>{}};
  harness.render(props);
  return {harness,props,calls};
}

test('default opening does not steal focus; explicit opens locate the already open assistant',async()=>{
  const {harness,props,calls}=await fixture();
  await harness.flush(); assert.deepEqual(calls,[]);
  harness.render({...props,assistantFocusRequest:1}); await harness.flush();
  assert.deepEqual(calls,[['scroll','start'],['focus',true]]);
  harness.render({...props,assistantFocusRequest:2}); await harness.flush();
  assert.equal(calls.length,4);
  harness.unmount();
});

test('a hidden assistant is not focused and the pending explicit open works after showing it',async()=>{
  const {harness,props,calls}=await fixture();
  harness.render({...props,assistantOpen:false,assistantFocusRequest:1}); await harness.flush();
  assert.deepEqual(calls,[]);
  harness.render({...props,assistantFocusRequest:1}); await harness.flush();
  assert.deepEqual(calls,[['scroll','start'],['focus',true]]);
  harness.unmount();
});
