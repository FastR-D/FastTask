import assert from 'node:assert/strict';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { build } from 'rolldown';

const compiled=await build({input:fileURLToPath(new URL('../src/review-data.ts',import.meta.url)),write:false,output:{format:'esm'}});
const {reportFromRecords,mergeReviewEntries,taskReviewEntry,validReviewEntry}=await import('data:text/javascript;base64,'+Buffer.from(compiled.output.find(item=>item.type==='chunk').code).toString('base64'));
const event=(id,extra={})=>({id,source:'fasttask',kind:'result',title:id,summary:'确实保存的记录',status:'completed',occurred_at:'2026-11-12T10:00:00Z',...extra});
const activity=(items,extra={})=>({week:'2026-W46',timezone:'Asia/Shanghai',start_date:'2026-11-09',end_date:'2026-11-15',items,focus_minutes:32,counts:{},upcoming:[],warnings:[],has_more:false,total:items.length,...extra});

test('weekly report excludes reversed completions and keeps actual steps and actual focus distinct',()=>{
  const report=reportFromRecords(activity([event('论文初稿'),event('错误完成',{status:'retracted'}),event('继续实验',{status:'reopened'}),event('搭好数据管道',{kind:'step',status:'recorded'}),event('收藏资料',{kind:'material',status:'recorded'})]));
  assert.match(report,/论文初稿/);assert.match(report,/搭好数据管道/);assert.match(report,/32 分钟/);
  assert.doesNotMatch(report,/错误完成|继续实验|收藏资料/);
  assert.match(report,/暂无已保存的下周安排/);assert.match(report,/待补充/);
});
test('weekly reports accept only FastTask progress even when an older API includes imported records',()=>{
  const report=reportFromRecords(activity([
    event('本地任务成果'),
    event('外部笔记',{source:'fastread',kind:'note'}),
    event('外部成果',{source:'fastwrite'}),
    event('导入资料',{kind:'material',time_basis:'imported_at'}),
    event('伪装成果',{time_basis:'imported_at'}),
    event('自动保存',{kind:'activity'}),
  ],{upcoming:[{date:'2026-11-16',title:'核对实验结果',target_minutes:40}]}));
  assert.match(report,/本地任务成果/);assert.match(report,/核对实验结果/);
  assert.doesNotMatch(report,/外部笔记|外部成果|导入资料|伪装成果|自动保存|FastRead|FastWrite|跨产品|尚未同步/);
  assert.match(report,/# FastTask 任务周报/);
});
test('partial task loads and local warnings remain visible in exported report',()=>{
  const report=reportFromRecords(activity([],{has_more:true,complete:false,warnings:['扫描范围有限']}));
  assert.match(report,/部分任务记录尚未载入/);assert.match(report,/扫描范围有限/);
});
test('repeated reads replace a source record and preserve different source identities',()=>{
  const merged=mergeReviewEntries([event('same'),event('other')],[event('same',{summary:'新内容'}),event('same',{source:'fastread'})]);
  assert.equal(merged.length,3);assert.equal(merged.find(item=>item.id==='same'&&item.source==='fasttask').summary,'新内容');
});
test('task review includes small steps and focus but excludes imports',()=>{
  for(const kind of ['result','step','minimum_action','time'])assert.equal(taskReviewEntry(event(kind,{kind})),true);
  assert.equal(taskReviewEntry(event('import',{time_basis:'imported_at'})),false);
  assert.equal(taskReviewEntry(event('note',{source:'fastread',kind:'note'})),false);
  assert.equal(validReviewEntry(event('rollback',{status:'rolled-back'})),false);
});
