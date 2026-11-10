import assert from 'node:assert/strict';
import { test } from 'node:test';
import { buildGanttRows, calendarDay, clipRange, monthRange, plannedDateError, shiftMonth, visibleGanttRows } from '../src/gantt.ts';
import { localDateInTimezone } from '../src/date.ts';

function node(id, fields = {}, children = []) {
  return {task:{id, goal_id:'goal', title:id, type:'task', status:'ready', revision:1, ...fields}, children};
}
const dates = (start, end = start) => ({planned_start:start, planned_end:end});
const range = (start, end = start) => ({start,end});

test('legacy tasks and empty stages stay unscheduled; projection does not mutate the task tree', () => {
  const nodes = [node('stage', {type:'milestone'}, [node('task')])];
  const before = JSON.stringify(nodes);
  const rows = buildGanttRows(nodes);
  assert.equal(rows.length, 2);
  assert.equal(rows[0].source, 'unscheduled');
  assert.equal(rows[0].unscheduledChildren, 1);
  assert.equal(rows[1].range, undefined);
  assert.equal(JSON.stringify(nodes), before);
});

test('nested parents derive inclusive ranges while preserving incomplete planning indicators', () => {
  const rows = buildGanttRows([node('root', {}, [node('phase', {}, [node('a', dates('2026-11-03', '2026-11-07')), node('b')]), node('c', dates('2026-12-01'))])]);
  assert.deepEqual(rows[0].range, range('2026-11-03', '2026-12-01'));
  assert.equal(rows[0].source, 'derived');
  assert.equal(rows[0].unscheduledChildren, 1);
  assert.deepEqual(rows[1].range, range('2026-11-03', '2026-11-07'));
});

test('manual phase range wins, overflow warns, and clearing dates restores automatic aggregation', () => {
  const phase = node('phase', {type:'milestone', ...dates('2026-11-01', '2026-11-10')}, [node('child', dates('2026-11-09', '2026-11-15'))]);
  let rows = buildGanttRows([phase]);
  assert.equal(rows[0].source, 'manual');
  assert.deepEqual(rows[0].range, range('2026-11-01', '2026-11-10'));
  assert.match(rows[1].warnings.join(), /超出「phase」/);
  phase.task.planned_start = null; phase.task.planned_end = null;
  rows = buildGanttRows([phase]);
  assert.equal(rows[0].source, 'derived');
  assert.deepEqual(rows[0].range, range('2026-11-09', '2026-11-15'));
  assert.deepEqual(rows[1].warnings, []);
});

test('overflow checks all manually bounded ancestors, even below a derived parent', () => {
  const rows = buildGanttRows([node('root', dates('2026-11-01', '2026-11-10'), [node('derived', {}, [node('child', dates('2026-11-12'))])])]);
  assert.match(rows[2].warnings.join(), /超出「root」/);
});

test('hard prerequisites inherit from ancestors and warn independently of target-date overflow', () => {
  const rows = buildGanttRows([node('prior', dates('2026-11-01', '2026-11-15')), node('phase', {depends_on:['prior']}, [node('child', dates('2026-11-10', '2026-11-20'))])], '2026-11-18');
  assert.deepEqual(rows[2].prerequisites, ['prior']);
  assert.match(rows[2].warnings.join(), /超过目标日期/);
  assert.match(rows[2].warnings.join(), /前置「prior」/);
  assert.equal(rows[2].task.status, 'ready');
});

test('missing prerequisite dates do not invent conflicts; same calendar day can contain both activities', () => {
  const rows = buildGanttRows([node('unknown'), node('prior', dates('2026-11-10')), node('next', {...dates('2026-11-10'), depends_on:['unknown','prior','deleted']})]);
  assert.deepEqual(rows[2].warnings, []);
});

test('collapsed rows keep their full derived ranges; expanding returns every descendant', () => {
  const rows = buildGanttRows([node('a', {}, [node('b', {}, [node('c', dates('2026-11-20'))])]), node('d')]);
  assert.deepEqual(visibleGanttRows(rows, new Set(['a'])).map(row=>row.task.id), ['a','d']);
  assert.deepEqual(rows[0].range, range('2026-11-20'));
  assert.deepEqual(visibleGanttRows(rows, new Set(['b'])).map(row=>row.task.id), ['a','b','d']);
  assert.equal(visibleGanttRows(rows, new Set()).length, 4);
});

test('deleted subtrees are excluded from display and aggregation', () => {
  const rows = buildGanttRows([node('phase', {}, [node('deleted', {...dates('2026-10-01'), deleted_at:'2026-10-02'}, [node('hidden', dates('2026-10-10'))]), node('live', dates('2026-11-12'))])]);
  assert.deepEqual(rows.map(row=>row.task.id), ['phase','live']);
  assert.deepEqual(rows[0].range, range('2026-11-12'));
});

test('date validation rejects partial, impossible, malformed, year-zero and reversed ranges', () => {
  assert.equal(plannedDateError('', ''), '');
  assert.equal(plannedDateError('2028-02-29', '2028-02-29'), '');
  for (const [start,end] of [['2026-11-01',''],['','2026-11-01'],['2026-02-29','2026-03-01'],['0000-01-01','0000-01-02'],['2026-2-01','2026-02-02'],['2026-11-02','2026-11-01']]) assert.ok(plannedDateError(start,end));
  assert.equal(calendarDay('2026-02-30'), undefined);
});

test('calendar month navigation handles leap years and year boundaries', () => {
  assert.deepEqual(monthRange('2028-02'), range('2028-02-01','2028-02-29'));
  assert.deepEqual(monthRange('2026-02'), range('2026-02-01','2026-02-28'));
  assert.equal(shiftMonth('2026-12', 1), '2027-01');
  assert.equal(shiftMonth('2026-01', -1), '2025-12');
  assert.equal(shiftMonth('0001-01', -1), '0001-01');
  assert.deepEqual(monthRange('9999-12'), range('9999-12-01','9999-12-31'));
});

test('bars include the last day, clip across months, and hide out-of-month ranges', () => {
  const window = monthRange('2026-11');
  assert.deepEqual(clipRange(range('2026-11-10'),window), {offset:9,days:1,before:false,after:false});
  assert.deepEqual(clipRange(range('2026-10-28','2026-12-02'),window), {offset:0,days:30,before:true,after:true});
  assert.equal(clipRange(range('2026-12-01'),window), undefined);
  assert.equal(clipRange(range('2026-10-31'),window), undefined);
});

test('today follows the account calendar across UTC midnight and DST transitions', () => {
  const instant = new Date('2026-11-01T00:30:00Z');
  assert.equal(localDateInTimezone(instant,'America/Los_Angeles'), '2026-10-31');
  assert.equal(localDateInTimezone(instant,'Asia/Singapore'), '2026-11-01');
  assert.equal(calendarDay('2026-11-02') - calendarDay('2026-11-01'), 1);
});
