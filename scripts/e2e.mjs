const base = process.env.FASTTASK_E2E_URL || 'http://127.0.0.1:10001'
const username = process.env.FASTTASK_ADMIN_USER || 'admin'
const password = process.env.FASTTASK_ADMIN_PASSWORD || 'fasttask-admin'

function assert(condition, message) {
  if (!condition) throw new Error(message)
}

async function call(path, { method = 'GET', token, headers = {}, body } = {}) {
  const response = await fetch(`${base}${path}`, {
    method,
    headers: {
      ...(body && !(body instanceof FormData) ? { 'Content-Type': 'application/json' } : {}),
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
      ...headers,
    },
    body: body instanceof FormData ? body : body ? JSON.stringify(body) : undefined,
  })
  const text = await response.text()
  let data = null
  if (text) {
    try { data = JSON.parse(text) } catch { data = text }
  }
  return { response, data }
}

async function waitJob(id, token) {
  for (let attempt = 0; attempt < 80; attempt++) {
    await new Promise(resolve => setTimeout(resolve, 250))
    const result = await call(`/api/v1/agent-jobs/${id}`, { token })
    assert(result.response.ok, `job poll failed: ${result.response.status}`)
    if (['succeeded', 'failed', 'cancelled'].includes(result.data.status)) return result.data
  }
  throw new Error(`job ${id} timed out`)
}

async function main() {
  const health = await call('/health/ready')
  assert(health.response.status === 200, 'service is not ready')

  const login = await call('/api/v1/auth/login', { method: 'POST', body: { identifier: username, password } })
  assert(login.response.status === 200, `login failed: ${login.response.status}`)
  const token = login.data.access_token

  const suffix = Date.now().toString(36)
  const goalRequest = { title: `真实场景目标 ${suffix}`, description: '端到端验证长期目标推进闭环', success_criteria: '形成三个可执行任务并完成一次最小行动' }
  const goal = await call('/api/v1/goals', { method: 'POST', token, headers: { 'Idempotency-Key': `e2e-goal-${suffix}` }, body: goalRequest })
  assert(goal.response.status === 201, `goal creation failed: ${goal.response.status}`)

  const replay = await call('/api/v1/goals', { method: 'POST', token, headers: { 'Idempotency-Key': `e2e-goal-${suffix}` }, body: goalRequest })
  assert(replay.response.status === 201 && replay.response.headers.get('Idempotency-Replayed') === 'true', 'idempotency replay failed')

  const generation = await call(`/api/v1/goals/${goal.data.id}/task-tree/generation-jobs`, {
    method: 'POST', token, headers: { 'Idempotency-Key': `e2e-agent-${suffix}` }, body: { instruction: '生成三个今天可以开始、具有明确最小行动的科研任务' },
  })
  assert(generation.response.status === 202, `task-tree job failed: ${generation.response.status}`)
  const generatedJob = await waitJob(generation.data.id, token)
  assert(generatedJob.status === 'succeeded', `task-tree job ended as ${generatedJob.status}: ${generatedJob.error_message || ''}`)

  const tree = await call(`/api/v1/goals/${goal.data.id}/task-tree`, { token })
  assert(tree.response.status === 200 && tree.data.proposals.length === 1, 'proposal was not persisted')
  const proposal = tree.data.proposals[0]
  const apply = await call(`/api/v1/goals/${goal.data.id}/task-tree/proposals/${proposal.id}/application`, {
    method: 'POST', token, headers: { 'If-Match': tree.response.headers.get('ETag'), 'Idempotency-Key': `e2e-apply-${suffix}` },
  })
  assert(apply.response.status === 200 && apply.data.created_tasks >= 2, `proposal application failed: ${apply.response.status} ${JSON.stringify(apply.data)}`)

  const lenses = await call('/api/v1/lenses', { token })
  assert(lenses.response.status === 200 && lenses.data.items.length === 1, `lens list failed: ${lenses.response.status}`)
  const lens = lenses.data.items[0]
  assert(lens.id === 'research_risk' && lens.threshold === 50 && lens.quadrants.length === 4, 'lens definition incomplete')

  const agentMap = await call(`/api/v1/goals/${goal.data.id}/map`, { token })
  assert(agentMap.response.status === 200 && agentMap.data.threshold === 50, `goal map failed: ${agentMap.response.status}`)
  assert(agentMap.data.nodes.length > 0, 'goal map returned no leaf nodes')
  const agentNode = agentMap.data.nodes.find(node => node.coord_id && node.source === 'agent')
  assert(agentNode, `applied proposal did not persist agent coordinates: ${JSON.stringify(agentMap.data.nodes)}`)
  assert(agentNode.x >= 0 && agentNode.x <= 100 && agentNode.y >= 0 && agentNode.y <= 100, `agent coordinate out of range: ${JSON.stringify(agentNode)}`)

  const coordWithoutMatch = await call(`/api/v1/tasks/${agentNode.task_id}/coords`, { method: 'PUT', token, body: { x: 10, y: 20 } })
  assert(coordWithoutMatch.response.status === 428, `coord overwrite without If-Match returned ${coordWithoutMatch.response.status}`)
  const coordStale = await call(`/api/v1/tasks/${agentNode.task_id}/coords`, { method: 'PUT', token, headers: { 'If-Match': `"coord_${agentNode.coord_id}_rev_${agentNode.coord_revision + 9}"` }, body: { x: 10, y: 20 } })
  assert(coordStale.response.status === 412, `stale coord revision returned ${coordStale.response.status}`)
  const coordOutOfRange = await call(`/api/v1/tasks/${agentNode.task_id}/coords`, { method: 'PUT', token, headers: { 'If-Match': `"coord_${agentNode.coord_id}_rev_${agentNode.coord_revision}"` }, body: { x: 101, y: 20 } })
  assert(coordOutOfRange.response.status === 422, `out of range coord returned ${coordOutOfRange.response.status}`)
  const coord = await call(`/api/v1/tasks/${agentNode.task_id}/coords`, {
    method: 'PUT', token, headers: { 'If-Match': `"coord_${agentNode.coord_id}_rev_${agentNode.coord_revision}"` },
    body: { x: 80, y: 90, rationale: '真实场景：方法未定，直接决定验收' },
  })
  assert(coord.response.status === 200 && coord.data.revision === agentNode.coord_revision + 1, `coord overwrite failed: ${coord.response.status} ${JSON.stringify(coord.data)}`)
  assert(coord.data.source === 'user' && coord.data.pinned === true && coord.data.x === 80 && coord.data.y === 90, `coord overwrite payload incorrect: ${JSON.stringify(coord.data)}`)
  assert(coord.response.headers.get('ETag') === `"coord_${coord.data.id}_rev_${coord.data.revision}"`, 'coord ETag missing')

  const manual = await call('/api/v1/tasks', {
    method: 'POST', token, headers: { 'Idempotency-Key': `e2e-manual-${suffix}` },
    body: { goal_id: goal.data.id, type: 'task', title: `手工任务 ${suffix}`, success_criteria: '形成一条可验证记录', minimum_action: '写下第一步', estimate_minutes: 25, priority: 60, position: 9 },
  })
  assert(manual.response.status === 201, `manual task creation failed: ${manual.response.status}`)
  const manualCoord = await call(`/api/v1/tasks/${manual.data.id}/coords`, { method: 'PUT', token, body: { x: 20, y: 30 } })
  assert(manualCoord.response.status === 200 && manualCoord.data.revision === 1 && manualCoord.data.source === 'user' && manualCoord.data.pinned === true, `first coord write failed: ${manualCoord.response.status} ${JSON.stringify(manualCoord.data)}`)

  const map = await call(`/api/v1/goals/${goal.data.id}/map`, { token })
  assert(map.response.status === 200, `goal map reload failed: ${map.response.status}`)
  const mapped = map.data.nodes.find(node => node.task_id === agentNode.task_id)
  assert(mapped && mapped.x === 80 && mapped.y === 90 && mapped.quadrant === 'A' && mapped.pinned === true && mapped.coord_revision === coord.data.revision, `mapped node incorrect: ${JSON.stringify(mapped)}`)
  const manualNode = map.data.nodes.find(node => node.task_id === manual.data.id)
  assert(manualNode && manualNode.x === 20 && manualNode.y === 30 && manualNode.quadrant === 'D', `manual node incorrect: ${JSON.stringify(manualNode)}`)
  assert(map.data.unplotted === map.data.nodes.filter(node => node.source === 'default').length, 'unplotted count inconsistent')
  assert(map.data.nodes.every(node => node.coord_id === '' || node.coord_revision > 0), 'node coord revision missing')
  const missingMap = await call('/api/v1/goals/goal_e2e_missing/map', { token })
  assert(missingMap.response.status === 404, `unknown goal map returned ${missingMap.response.status}`)
  const badLensMap = await call(`/api/v1/goals/${goal.data.id}/map?lens=unknown`, { token })
  assert(badLensMap.response.status === 422, `unknown lens map returned ${badLensMap.response.status}`)

  const review = await call('/api/v1/reviews/weekly', { token })
  assert(review.response.status === 200, `weekly review failed: ${review.response.status}`)
  assert(review.data.focus.quadrants.length === 4 && review.data.summary.source === 'template' && review.data.summary.text.length > 0, `weekly review payload incomplete: ${JSON.stringify(review.data.summary)}`)
  assert(review.data.llm_note === '', 'llm_note must stay empty in L2')
  const badWeek = await call('/api/v1/reviews/weekly?week=2026-W99', { token })
  assert(badWeek.response.status === 422, `invalid week returned ${badWeek.response.status}`)
  const explicitWeek = await call('/api/v1/reviews/weekly?week=2026-W01&timezone=Europe/Berlin', { token })
  assert(explicitWeek.response.status === 200 && explicitWeek.data.start_date === '2025-12-29', `explicit week failed: ${JSON.stringify(explicitWeek.data.start_date)}`)

  const today = new Intl.DateTimeFormat('en-CA', { timeZone: 'Asia/Shanghai', year: 'numeric', month: '2-digit', day: '2-digit' }).format(new Date())
  const plan = await call('/api/v1/daily-plans', {
    method: 'POST', token, headers: { 'Idempotency-Key': `e2e-plan-${suffix}` }, body: { local_date: today, timezone: 'Asia/Shanghai', available_minutes: 120 },
  })
  assert(plan.response.status === 201, `daily plan failed: ${plan.response.status}`)
  assert(plan.data.items.length > 0 && plan.data.items.length <= 3, `invalid core count: ${plan.data.items.length}`)
  const item = plan.data.items[0]

  const completion = await call(`/api/v1/daily-plans/${plan.data.plan.id}/items/${item.id}/completions`, {
    method: 'POST', token, headers: { 'If-Match': `"dpi_${item.id}_rev_${item.revision}"`, 'Idempotency-Key': `e2e-complete-${suffix}` }, body: { type: 'minimum_action', summary: '真实场景已完成最小行动' },
  })
  assert(completion.response.status === 200 && completion.data.status === 'satisfied', 'daily item completion failed')
  const task = await call(`/api/v1/tasks/${item.task_id}`, { token })
  assert(task.response.status === 200 && task.data.status !== 'completed', 'daily completion incorrectly completed the underlying task')

	const replan = await call('/api/v1/daily-plans/generation-jobs', { method:'POST', token, headers:{'Idempotency-Key':`e2e-replan-${suffix}`}, body:{local_date:today,timezone:'Asia/Shanghai',available_minutes:120,replace_existing:true,base_revision:plan.data.plan.revision} })
	assert(replan.response.status === 202, 'replan job creation failed')
	const replanJob = await waitJob(replan.data.id, token)
	assert(replanJob.status === 'succeeded', `replan failed: ${replanJob.error_message || ''}`)
	const currentPlan = await call('/api/v1/daily-plans/current', { token })
	assert(currentPlan.response.status === 200 && currentPlan.data.plan.current_revision === 2, 'same-day replan did not create revision 2')
	assert(currentPlan.data.items.filter(value => value.kind === 'core').length <= 3, 'replan exceeded three core slots')

  const conversation = await call('/api/v1/conversations', { method: 'POST', token, headers: { 'Idempotency-Key': `e2e-conv-${suffix}` }, body: { title: `场景对话 ${suffix}` } })
  assert(conversation.response.status === 201, 'conversation creation failed')
  const message = await call(`/api/v1/conversations/${conversation.data.id}/messages`, { method: 'POST', token, headers: { 'Idempotency-Key': `e2e-msg-${suffix}` }, body: { content: '我已经开始，但任务仍然太大，请给出一个更小的下一步。' } })
  assert(message.response.status === 202, 'conversation message failed')
  const conversationJob = await waitJob(message.data.id, token)
  assert(conversationJob.status === 'succeeded', `conversation job failed: ${conversationJob.error_message || ''}`)

  const device = await call('/api/v1/devices', { method: 'POST', token, headers: { 'Idempotency-Key': `e2e-device-${suffix}` }, body: { name: `场景墨水屏 ${suffix}`, kind: 'eink_panel', timezone: 'Asia/Shanghai', capabilities: { width: 800, height: 480, color_mode: 'monochrome' } } })
  assert(device.response.status === 201, 'device registration failed')
  const poll = await call('/api/v1/devices/self/poll', { headers: { 'X-Device-Token': device.data.device_token } })
  assert(poll.response.status === 200 && poll.data.core_items.length <= 3, 'device poll failed')
  const notModified = await call('/api/v1/devices/self/poll', { headers: { 'X-Device-Token': device.data.device_token, 'If-None-Match': poll.response.headers.get('ETag') } })
  assert(notModified.response.status === 304, 'device conditional poll did not return 304')

  const panel = await call('/api/v1/panel/summary', { token })
  assert(panel.response.status === 200 && panel.data.core_total <= 3, 'panel summary failed')

  console.log(JSON.stringify({
    result: 'PASS',
    goal_id: goal.data.id,
    task_tree_provider: generatedJob.output_json ? JSON.parse(generatedJob.output_json).provider : 'unknown',
    core_items: plan.data.items.length,
		replanned_revision: currentPlan.data.plan.current_revision,
    daily_completion_preserved_task: true,
    device_etag_304: true,
    panel_summary: true,
    lens: lens.id,
    agent_coord_source: agentNode.source,
    user_coord_revision: coord.data.revision,
    map_nodes: map.data.nodes.length,
    map_unplotted: map.data.unplotted,
    weekly_review: { week: review.data.week, rule: review.data.summary.rule, total_minutes: review.data.focus.total_minutes },
  }, null, 2))
}

main().catch(error => {
  console.error(error.stack || error.message || String(error))
  process.exit(1)
})
