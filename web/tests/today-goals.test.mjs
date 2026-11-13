import assert from 'node:assert/strict';
import { test } from 'node:test';
import { createComponentHarness, deferred, eventTarget, nodes, text } from './helpers/component-harness.mjs';

class ApiError extends Error { constructor(status, message) { super(message); this.status = status; } }
const goal = { id: 'goal', title: '科研论文计划', status: 'active', revision: 1, success_criteria: '形成可复核的论文初稿', description: '保持完整任务与依赖' };
const task = { id: 'task', goal_id: goal.id, title: '整理相关工作', type: 'task', status: 'ready', revision: 1, success_criteria: '交付对照表', minimum_action: '阅读一篇论文', description: '', estimate_minutes: 40, priority: 70, depends_on: [] };
const item = { id: 'item', logical_id: 'logical-item', daily_plan_id: 'plan', task_id: task.id, kind: 'core', title: task.title, commitment: '比较三篇论文', minimum_action: '先阅读摘要', status: 'planned', target_minutes: 25, revision: 1, allowed_types: 'minimum_action,step,result,time' };
const session = { id: 'session', task_id: task.id, daily_plan_item_id: item.id, status: 'running', target_minutes: 25, started_at: '2026-11-11T15:40:00Z', revision: 1, duration_seconds: 0 };
const response = data => ({ data, etag: '"revision-1"' });
const day = (date = '2026-11-11', items = [item]) => ({ plan: { id: 'plan', local_date: date, status: 'active', revision: 1 }, items, today_items: items, availability: { status: 'ready', reason: '有一项可以立即推进，不必凑满三项。', can_replan: true } });
const tree = (goalId = goal.id) => ({ goal_id: goalId, revision: 1, nodes: [{ task: { ...task, goal_id: goalId }, children: [] }], proposals: [] });

function clock() {
  let now = Date.parse('2026-11-11T15:55:00Z'), timerId = 0;
  const timers = new Map();
  class ClockDate extends Date { constructor(...args) { super(...(args.length ? args : [now])); } static now() { return now; } }
  const setInterval = (fn, ms) => { timers.set(++timerId, { fn, ms }); return timerId; };
  const clearInterval = id => timers.delete(id);
  const document = eventTarget({ visibilityState: 'visible' });
  const window = eventTarget({ setInterval, clearInterval });
  const navigator = { onLine: true };
  return { document, window, navigator, globals: { Date: ClockDate, document, window, navigator, setInterval, clearInterval },
    advanceTo(value) { now = Date.parse(value); },
    tickDay() { for (const { fn, ms } of [...timers.values()]) if (ms === 30_000) fn(); },
    timerCount() { return timers.size; },
  };
}

async function fixture(component = 'Today', options = {}) {
  const environment = clock(), calls = [], notices = [];
  let transport = options.request;
  let currentDay = options.day ?? day(), currentSessions = options.sessions ?? [];
  async function request(path, init = {}, owner) {
    calls.push({ path, init, owner });
    const override = transport?.(path, init, owner);
    if (override !== undefined) return override;
    if (path === '/daily-plans/current') return response(currentDay);
    if (path === '/work-sessions' && !init.method) return response({ items: currentSessions });
    if (path === '/planning-settings') return response({ daily_budget_minutes: 120, primary_goal_id: goal.id });
    if (path === '/goals') return response({ items: [goal] });
    if (path === '/tasks') return response({ items: [task] });
    if (path === '/tasks/task') return response(task);
    if (path === '/schedule') return response({ items: [] });
    if (path === '/goals/goal/task-tree') return response(tree());
    if (path === '/agent/availability') return response({ configured: true });
    throw new Error(`Unexpected request: ${init.method ?? 'GET'} ${path}`);
  }
  const harness = await createComponentHarness(new URL(`../src/${component}.tsx`, import.meta.url), component, {
    globals: environment.globals,
    mocks: {
      './api': { ApiError, request, idem: () => 'test-key' },
      './creation-attempt': { createCreationAttempt: () => ({ pending: () => false, reset() {}, recover() { throw new Error('Unexpected creation'); } }) },
      './mdui-react': { fieldValue: event => event.target.value, useMduiEvent() {} },
      './jobs': { waitJob() { throw new Error('Unexpected job'); } },
      './ModuleWorkspaceLink': { ModuleWorkspaceLink: () => null },
      './Lens': { GoalMapView: () => null },
      './GanttView': { GanttView: function TestGantt() {} },
      './agent/tools/ProposalApproval': { ProposalSummary: () => null },
    },
  });
  let props = { userId: 'user', timezone: 'Asia/Shanghai', onNotice: value => notices.push(value), onBusinessChanged() {}, refreshVersion: 0 };
  return { harness, environment, calls, notices,
    async mount() { harness.render(props); await harness.flush(); },
    async refresh() { props = { ...props, refreshVersion: props.refreshVersion + 1 }; harness.render(props); await harness.flush(); },
    setTransport(value) { transport = value; },
    setDay(value) { currentDay = value; },
    setSessions(value) { currentSessions = value; },
    get output() { return harness.output; },
    button(label) { const found = nodes(harness.output, node => ['button', 'mdui-button'].includes(node.type) && text(node) === label); assert.equal(found.length, 1, `button ${label}`); return found[0]; },
    field(label) { const found = nodes(harness.output, node => node.props.label === label); assert.equal(found.length, 1, `field ${label}`); return found[0]; },
    writes() { return calls.filter(call => call.init.method && call.init.method !== 'GET'); },
  };
}

for (const action of ['start', 'pause', 'resume', 'finish']) {
  test(`a delayed background snapshot cannot undo a successful focus ${action}`, async () => {
    const initial = action === 'start' ? [] : [{ ...session, status: action === 'resume' ? 'paused' : 'running', paused_at: '2026-11-11T15:45:00Z' }];
    const f = await fixture('Today', { sessions: initial });
    await f.mount();
    const pendingPlan = deferred();
    const updated = { ...session, revision: 2, status: action === 'pause' ? 'paused' : action === 'finish' ? 'completed' : 'running', duration_seconds: action === 'finish' ? 900 : 0 };
    f.setTransport((path, init) => {
      if (path === '/daily-plans/current') return pendingPlan.promise;
      if (path.startsWith('/work-sessions') && init.method === 'POST') return response(updated);
    });
    await f.refresh(); // old session list has returned, current plan is pending
    if (action === 'finish') { f.field('这轮实际推进了什么？').props.onInput({ target: { value: '完成实验对照' } }); await f.harness.flush(); }
    const label = { start: '开始专注', pause: '暂停计时', resume: '继续专注', finish: '结束并记录' }[action];
    assert.equal(Boolean(f.button(label).props.disabled), false);
    await f.button(label).props.onClick(); await f.harness.flush();
    pendingPlan.resolve(response(day())); await f.harness.flush();
    if (action === 'finish') {
      assert.equal(nodes(f.output, node => node.props.className === 'focus-strip').length, 0);
      assert.equal(f.field('实际推进摘要').props.value, '完成实验对照');
      assert.match(text(f.output), /已记录 15 分钟/);
    } else {
      assert.match(text(f.output), action === 'pause' ? /专注已暂停/ : /专注进行中/);
      assert.equal(Boolean(f.button('开始专注').props.disabled), true);
    }
    assert.equal(f.writes().length, 1);
    f.harness.unmount();
  });
}

test('a read begun during an in-flight focus write also cannot reset its result', async () => {
  const f = await fixture(); await f.mount();
  const mutation = deferred(), pendingPlan = deferred();
  f.setTransport((path, init) => {
    if (path === '/work-sessions' && init.method === 'POST') return mutation.promise;
    if (path === '/daily-plans/current') return pendingPlan.promise;
  });
  const start = f.button('开始专注').props.onClick();
  await f.refresh();
  mutation.resolve(response(session)); await start; await f.harness.flush();
  pendingPlan.resolve(response(day())); await f.harness.flush();
  assert.match(text(f.output), /专注进行中/);
  f.harness.unmount();
});

test('focus conflict recovery reads fresh server state after the failed write', async () => {
  const f = await fixture(); await f.mount();
  f.setTransport((path, init) => {
    if (path === '/work-sessions' && init.method === 'POST') { f.setSessions([session]); return Promise.reject(new ApiError(409, '已有运行中的专注')); }
  });
  await f.button('开始专注').props.onClick(); await f.harness.flush();
  assert.match(text(f.output), /专注进行中/);
  assert.equal(f.writes().length, 1);
  f.harness.unmount();
});

test('midnight rejects a previously rendered action, then reloads by the user timezone', async () => {
  const f = await fixture(); await f.mount();
  const oldStart = f.button('开始专注').props.onClick;
  f.field('实际推进摘要').props.onInput({ target: { value: '保留未提交的摘要' } }); await f.harness.flush();
  f.environment.advanceTo('2026-11-11T16:00:01Z'); // Nov 12 in Shanghai, still Nov 11 UTC
  await oldStart();
  assert.equal(f.writes().length, 0, 'click-time date check must not depend on a prior rerender');
  const pendingPlan = deferred();
  f.setTransport(path => path === '/daily-plans/current' ? pendingPlan.promise : undefined);
  f.environment.tickDay(); await f.harness.flush();
  assert.match(text(f.output), /当前显示 2026-11-11 的安排/);
  assert.equal(Boolean(f.button('开始专注').props.disabled), true);
  pendingPlan.resolve(response(day('2026-11-12'))); await f.harness.flush();
  assert.equal(Boolean(f.button('开始专注').props.disabled), false);
  assert.equal(f.field('实际推进摘要').props.value, '保留未提交的摘要');
  f.harness.unmount(); assert.equal(f.environment.timerCount(), 0);
});

test('returning to the foreground refreshes without dropping an edited settings draft', async () => {
  const f = await fixture(); await f.mount();
  f.field('每日专注预算（分钟）').props.onInput({ target: { value: '90' } }); await f.harness.flush();
  const readsBefore = f.calls.filter(call => call.path === '/daily-plans/current').length;
  f.setTransport(path => path === '/planning-settings' ? { data: { daily_budget_minutes: 180, primary_goal_id: goal.id }, etag: '"revision-2"' } : undefined);
  f.environment.document.visibilityState = 'hidden'; f.environment.document.dispatch('visibilitychange'); await f.harness.flush();
  assert.equal(f.calls.filter(call => call.path === '/daily-plans/current').length, readsBefore);
  f.environment.document.visibilityState = 'visible'; f.environment.document.dispatch('visibilitychange'); f.environment.window.dispatch('focus'); await f.harness.flush();
  assert.equal(f.calls.filter(call => call.path === '/daily-plans/current').length, readsBefore + 1);
  assert.equal(f.field('每日专注预算（分钟）').props.value, '90');
  assert.match(text(f.output), /计划设置已在另一处更新/);
  f.harness.unmount();
});

test('task titles lead, duplicate action text is omitted, and availability stays in a disclosure', async () => {
  const f = await fixture('Today', { day: day('2026-11-11', [{ ...item, commitment: ` ${item.title} `, minimum_action: item.title }]) });
  await f.mount();
  const article = nodes(f.output, node => node.type === 'article')[0];
  assert.equal(text(nodes(article, node => node.type === 'h2')[0]), item.title);
  assert.equal(nodes(article, node => ['today-item-commitment', 'today-item-minimum-action'].includes(node.props.className)).length, 0);
  const availability = nodes(f.output, node => node.props.className === 'today-availability')[0];
  assert.ok(availability);
  assert.equal(Boolean(nodes(availability, node => node.type === 'details')[0].props.open), false);
  assert.equal(f.button('开始专注').props.variant, 'filled');
  assert.equal(f.button('重新安排今天').props.variant, 'outlined');
  f.harness.unmount();
});

test('cached goals and task tree remain navigable when the uncached task index fails', async () => {
  const f = await fixture('GoalManager', { request: path => path === '/tasks' ? Promise.reject(new ApiError(0, '需要联网')) : undefined });
  await f.mount();
  assert.match(text(f.output), /科研论文计划/);
  assert.match(text(f.output), /整理相关工作/);
  assert.doesNotMatch(text(f.output), /从一个重要目标开始/);
  assert.match(text(f.output), /仅可查看/);
  assert.equal(Boolean(f.button('新建目标').props.disabled), true);
  assert.equal(Boolean(f.button('编辑目标').props.disabled), true);
  f.button('甘特图').props.onClick(); await f.harness.flush();
  const gantt = nodes(f.output, node => typeof node.type === 'function' && node.type.name === 'TestGantt')[0];
  assert.equal(gantt.props.nodes[0].task.id, task.id);
  assert.equal(gantt.props.busy, true);
  await f.button('新建目标').props.onClick(); await f.harness.flush();
  assert.equal(nodes(f.output, node => node.type === 'mdui-dialog').length, 0);
  assert.equal(f.writes().length, 0);
  f.setTransport(undefined); f.environment.window.dispatch('online'); await f.harness.flush();
  assert.equal(Boolean(f.button('编辑目标').props.disabled), false);
  f.harness.unmount();
});

test('compact goal selection uses the same tree-loading path and remains usable read-only', async () => {
  const other = { ...goal, id: 'other-goal', title: '另一个研究目标' };
  const f = await fixture('GoalManager', { request: path => {
    if (path === '/goals') return response({ items: [goal, other] });
    if (path === '/tasks') return Promise.reject(new ApiError(0, '需要联网'));
    if (path === '/goals/other-goal/task-tree') return response(tree(other.id));
  } });
  await f.mount();
  const select = nodes(f.output, node => node.type === 'select' && node.props['aria-label'] === '选择目标')[0];
  assert.equal(Boolean(select.props.disabled), false);
  select.props.onChange({ target: { value: other.id } }); await f.harness.flush();
  assert.equal(nodes(f.output, node => node.type === 'select')[0].props.value, other.id);
  assert.equal(nodes(f.output, node => node.props.className === 'goal-description')[0].props.open, undefined);
  assert.ok(f.calls.some(call => call.path === '/goals/other-goal/task-tree'));
  f.harness.unmount();
});

test('offline and reconnect preserve editor draft and the original revision conflict guard', async () => {
  const f = await fixture('GoalManager'); await f.mount();
  f.button('编辑目标').props.onClick(); await f.harness.flush();
  f.field('目标标题').props.onInput({ target: { value: '我的未保存标题' } }); await f.harness.flush();
  f.setTransport(path => path === '/tasks' ? Promise.reject(new ApiError(0, '需要联网')) : undefined);
  f.environment.navigator.onLine = false; f.environment.window.dispatch('offline'); await f.harness.flush();
  assert.equal(Boolean(f.button('保存').props.disabled), true);
  assert.equal(Boolean(f.button('取消').props.disabled), false);
  f.environment.navigator.onLine = true;
  f.setTransport(path => path === '/goals' ? response({ items: [{ ...goal, revision: 2, title: '另一处更新的标题' }] }) : undefined);
  f.environment.window.dispatch('online'); await f.harness.flush();
  assert.equal(f.field('目标标题').props.value, '我的未保存标题');
  assert.match(text(f.output), /内容已在另一处更新，你的草稿仍在/);
  assert.equal(f.writes().length, 0);
  f.harness.unmount();
});

test('a reachable local API permits editing despite a false browser offline flag', async () => {
  const f = await fixture('GoalManager');
  f.environment.navigator.onLine = false;
  await f.mount();
  assert.equal(Boolean(f.button('编辑目标').props.disabled), false);
  assert.equal(nodes(f.output, node => node.props.className === 'goal-readonly-status').length, 0);

  const tasksRead = deferred();
  f.setTransport(path => path === '/tasks' ? tasksRead.promise : undefined);
  f.environment.window.dispatch('offline'); await f.harness.flush();
  assert.equal(Boolean(f.button('编辑目标').props.disabled), true, 'event temporarily protects edits while actual reachability is checked');
  tasksRead.resolve(response({ items: [task] })); await f.harness.flush();
  assert.equal(Boolean(f.button('编辑目标').props.disabled), false);

  f.setTransport((path, init) => path === '/goals/goal' && init.method === 'PATCH' ? response({ ...goal, revision: 2, title: '本地仍可编辑' }) : undefined);
  f.button('编辑目标').props.onClick(); await f.harness.flush();
  f.field('目标标题').props.onInput({ target: { value: '本地仍可编辑' } }); await f.harness.flush();
  await f.button('保存').props.onClick(); await f.harness.flush();
  assert.equal(f.writes().length, 1);
  assert.equal(f.writes()[0].path, '/goals/goal');
  assert.equal(f.writes()[0].init.headers['If-Match'], '"goal_goal_rev_1"');
  f.harness.unmount();
});
