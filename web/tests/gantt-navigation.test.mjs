import assert from 'node:assert/strict';
import { test } from 'node:test';
import { createComponentHarness, nodes, text } from './helpers/component-harness.mjs';

class FixedDate extends Date {
  constructor(...args) { super(...(args.length ? args : ['2026-11-12T04:00:00Z'])); }
  static now() { return Date.parse('2026-11-12T04:00:00Z'); }
}
const task = id => ({task:{id, goal_id:'goal', title:id, type:'task', status:'ready', revision:1,
  planned_start:'2026-11-11', planned_end:'2026-11-15'}, children:[]});

async function fixture({ initialNodes = [task('a')] } = {}) {
  let hasToday = true;
  const viewport = {
    clientWidth:300, labelWidth:144, scrollLeft:0, calls:[],
    getBoundingClientRect: () => ({left:220}),
    querySelector(selector) {
      if (selector === '.gantt-heading') return {getBoundingClientRect:()=>({width:this.labelWidth})};
      if (selector === '.gantt-day.today' && hasToday) return {getBoundingClientRect:()=>({left:220+this.labelWidth+11*36-this.scrollLeft,width:36})};
      return null;
    },
    scrollTo({left}) { this.calls.push(left); this.scrollLeft=Math.max(0,Math.min(left,this.labelWidth+30*36-this.clientWidth)); },
  };
  const harness = await createComponentHarness(new URL('../src/GanttView.tsx', import.meta.url),'GanttView',{
    mocks:{'./gantt.css':{}},
    globals:{Date:FixedDate,window:{setInterval:()=>1,clearInterval:()=>{}}},
    commit(tree) {
      hasToday=nodes(tree,n=>n.props.className?.split(' ').includes('today')).length>0;
      for(const node of nodes(tree,n=>n.props.className==='gantt-scroll')) node.props.ref.current=viewport;
    },
  });
  const props={goal:{id:'goal',title:'论文计划'},nodes:initialNodes,timezone:'Asia/Shanghai',busy:false,onEdit:()=>{},onAdd:()=>{}};
  harness.render(props);
  const click = label => nodes(harness.output,n=>text(n)===label&&n.type==='mdui-button')[0].props.onClick();
  const visibleToday = () => {
    const x=viewport.labelWidth+11*36-viewport.scrollLeft;
    return x>=viewport.labelWidth && x+36<=viewport.clientWidth;
  };
  return {harness,props,viewport,click,visibleToday};
}

test('Gantt initially reveals today beyond the sticky labels in a narrow pane',async()=>{
  const {harness,viewport,visibleToday}=await fixture();
  assert.ok(viewport.scrollLeft>0);
  assert.equal(visibleToday(),true);
  harness.unmount();
});

test('return to today reveals the date even when its month is already selected',async()=>{
  const {harness,viewport,click,visibleToday}=await fixture();
  viewport.scrollLeft=0;
  assert.equal(visibleToday(),false);
  click('回到今天'); await harness.flush();
  assert.equal(visibleToday(),true);
  assert.equal(viewport.calls.length,2);
  harness.unmount();
});

test('return to today waits for the current month DOM and uses the current pane width',async()=>{
  const {harness,viewport,click,visibleToday}=await fixture();
  nodes(harness.output,n=>n.props['aria-label']==='下个月')[0].props.onClick();
  await harness.flush();
  assert.ok(text(harness.output).includes('12 月'));
  viewport.clientWidth=520; viewport.labelWidth=180; viewport.scrollLeft=0;
  click('回到今天'); await harness.flush();
  assert.ok(text(harness.output).includes('11 月'));
  assert.equal(visibleToday(),true);
  harness.unmount();
});

test('data refresh does not move a timeline the user has scrolled',async()=>{
  const {harness,props,viewport}=await fixture();
  viewport.scrollLeft=500;
  harness.render({...props,nodes:[task('a'),task('b')]}); await harness.flush();
  assert.equal(viewport.scrollLeft,500);
  assert.equal(viewport.calls.length,1);
  harness.unmount();
});

test('initially empty Gantt reveals today when rows arrive',async()=>{
  const {harness,props,viewport,visibleToday}=await fixture({initialNodes:[]});
  assert.equal(viewport.calls.length,0);
  harness.render({...props,nodes:[task('a')]}); await harness.flush();
  assert.equal(visibleToday(),true);
  harness.unmount();
});
