import crypto from 'node:crypto'

const base = (process.env.FASTTASK_E2E_URL || 'http://127.0.0.1:10000').replace(/\/$/, '')
const username = process.env.FASTTASK_ADMIN_USER || 'admin'
const password = process.env.FASTTASK_ADMIN_PASSWORD

if (!password) throw new Error('FASTTASK_ADMIN_PASSWORD is required')

let accessToken = ''
let existingGoals = []
let existingTasks = []
let existingPlans = []
let existingSessions = []
let existingConversations = []
let existingDevices = []
const key = prefix => `demo-${prefix}-${crypto.randomUUID()}`

async function call(path, { method = 'GET', body, headers = {} } = {}) {
  const response = await fetch(base + path, {
    method,
    headers: {
      ...(body ? { 'Content-Type': 'application/json' } : {}),
      ...(accessToken ? { Authorization: `Bearer ${accessToken}` } : {}),
      ...headers,
    },
    body: body ? JSON.stringify(body) : undefined,
  })
  const text = await response.text()
  let data
  try { data = text ? JSON.parse(text) : null } catch { data = text }
  if (!response.ok) throw new Error(`${method} ${path}: ${response.status} ${JSON.stringify(data)}`)
  return { data, etag: response.headers.get('ETag') }
}

function etag(kind, id, revision) { return `"${kind}_${id}_rev_${revision}"` }

async function waitJob(id) {
  for (let attempt = 0; attempt < 300; attempt++) {
    await new Promise(resolve => setTimeout(resolve, 500))
    const { data } = await call(`/api/v1/agent-jobs/${id}`)
    if (['succeeded', 'failed', 'cancelled'].includes(data.status)) return data
  }
  throw new Error(`job ${id} timed out`)
}

async function createGoal(spec) {
  const found = existingGoals.find(goal => goal.title === spec.title)
  if (found) return found
  const { data } = await call('/api/v1/goals', {
    method: 'POST', headers: { 'Idempotency-Key': key('goal') }, body: spec,
  })
  existingGoals.push(data)
  return data
}

async function createTask(goalID, spec, position, parentID = null) {
  let task = existingTasks.find(item => item.goal_id === goalID && item.title === spec.title)
  const reused = Boolean(task)
  if (!task) {
    const { data } = await call('/api/v1/tasks', {
      method: 'POST', headers: { 'Idempotency-Key': key('task') }, body: {
        goal_id: goalID,
        parent_id: parentID,
        type: spec.type || 'task',
        title: spec.title,
        description: spec.description || '',
        success_criteria: spec.success,
        minimum_action: spec.minimum,
        estimate_minutes: spec.estimate || 50,
        priority: spec.priority,
        position,
      },
    })
    task = data
    existingTasks.push(task)
  }
  if (spec.status === 'completed' && task.status !== 'completed') {
    const completed = await call(`/api/v1/tasks/${task.id}/completions`, {
      method: 'POST', headers: {
        'If-Match': etag('task', task.id, task.revision), 'Idempotency-Key': key('task-complete'),
      }, body: { type: 'result', summary: spec.success },
    })
    task = completed.data
  } else if (spec.status && spec.status !== 'ready' && task.status !== spec.status) {
    const patched = await call(`/api/v1/tasks/${task.id}`, {
      method: 'PATCH', headers: { 'If-Match': etag('task', task.id, task.revision) },
      body: { status: spec.status, blocked_reason: spec.blocked_reason || '' },
    })
    task = patched.data
  }
  if (spec.coord) {
    try {
      await call(`/api/v1/tasks/${task.id}/coords`, {
        method: 'PUT', body: { x: spec.coord[0], y: spec.coord[1], rationale: spec.rationale || '' },
      })
    } catch (error) {
      if (!reused || !String(error).includes('428')) throw error
    }
  }
  return task
}

async function addSession(task, start, minutes, note, stopped = false) {
  if (existingSessions.some(session => session.note === note)) return
  const started = new Date(start)
  const ended = new Date(started.getTime() + minutes * 60_000)
  const { data } = await call('/api/v1/work-sessions', {
    method: 'POST', headers: { 'Idempotency-Key': key('session') }, body: {
      task_id: task.id, session_type: minutes <= 30 ? 'pomodoro' : 'deep_work',
      target_minutes: minutes, started_at: started.toISOString(),
    },
  })
  const action = stopped ? 'stoppings' : 'completions'
  const completed = await call(`/api/v1/work-sessions/${data.id}/${action}`, {
    method: 'POST', headers: {
      'If-Match': etag('work', data.id, data.revision), 'Idempotency-Key': key('finish'),
    }, body: { ended_at: ended.toISOString(), outcome: 'progressed', note },
  })
  existingSessions.push(completed.data)
}

async function createPlan(localDate, tasks, evidence) {
  const found = existingPlans.find(plan => plan.local_date === localDate)
  if (found) return found
  const { data } = await call('/api/v1/daily-plans', {
    method: 'POST', headers: { 'Idempotency-Key': key('plan') }, body: {
      local_date: localDate, timezone: 'Asia/Shanghai', available_minutes: 180,
      task_ids: tasks.map(task => task.id),
    },
  })
  for (let index = 0; index < data.items.length; index++) {
    const item = data.items[index]
    const completion = evidence[index]
    if (!completion) continue
    await call(`/api/v1/daily-plans/${data.plan.id}/items/${item.id}/completions`, {
      method: 'POST', headers: {
        'If-Match': etag('dpi', item.id, item.revision), 'Idempotency-Key': key('evidence'),
      }, body: completion,
    })
  }
  existingPlans.push(data.plan)
  return data
}

async function createConversation(title, goalID, messages) {
  let conversation = existingConversations.find(item => item.title === title)
  if (!conversation) {
    const created = await call('/api/v1/conversations', {
      method: 'POST', headers: { 'Idempotency-Key': key('conversation') }, body: { title, goal_id: goalID },
    })
    conversation = created.data
    existingConversations.push(conversation)
  }
  const current = await call(`/api/v1/conversations/${conversation.id}/messages`)
  const existingUserMessages = new Set(current.data.items.filter(item => item.role === 'user').map(item => item.content))
  for (const content of messages) {
    if (existingUserMessages.has(content)) continue
    const { data: job } = await call(`/api/v1/conversations/${conversation.id}/messages`, {
      method: 'POST', headers: { 'Idempotency-Key': key('message') }, body: { content },
    })
    const result = await waitJob(job.id)
    if (result.status !== 'succeeded') console.warn(`conversation job ${job.id}: ${result.status}`)
  }
  return conversation
}

async function main() {
  const login = await call('/api/v1/auth/login', { method: 'POST', body: { identifier: username, password } })
  accessToken = login.data.access_token

  const [existing, currentTasks, currentPlans, currentSessions, currentConversations, currentDevices] = await Promise.all([
    call('/api/v1/goals'), call('/api/v1/tasks'), call('/api/v1/daily-plans'),
    call('/api/v1/work-sessions'), call('/api/v1/conversations'), call('/api/v1/devices'),
  ])
  existingGoals = existing.data.items
  existingTasks = currentTasks.data.items
  existingPlans = currentPlans.data.items
  existingSessions = currentSessions.data.items
  existingConversations = currentConversations.data.items
  existingDevices = currentDevices.data.items

  const paperA = await createGoal({
    title: '论文 A｜面向固件分析的约束引导模糊测试',
    description: '11 月投稿主线。目标是用可解释的约束提取提升固件模糊测试的路径覆盖与漏洞触达效率。',
    success_criteria: '形成完整论文、可复现实验包，并通过一次组内预审',
    target_date: '2026-11-08',
  })
  const paperB = await createGoal({
    title: '论文 B｜大模型辅助漏洞根因定位',
    description: '第二篇投稿主线。聚焦检索增强、证据链生成与失败案例分析。',
    success_criteria: '完成可内审初稿、主实验与至少两组消融实验',
    target_date: '2026-11-22',
  })
  const firmware = await createGoal({
    title: '固件漏洞挖掘｜建立可复用实验流水线',
    description: '持续研究主线，为两篇论文提供真实样本、漏洞案例和可复现实验基础设施。',
    success_criteria: '跑通从固件收集、解包、静态筛选到动态验证的自动化流水线，并沉淀两个高质量案例',
    target_date: '2026-10-25',
  })

  const tasks = {}
  const aMilestone = await createTask(paperA.id, { type: 'milestone', title: 'A1｜完成主实验与消融', success: '主结果表、消融表和失败案例均可复现', minimum: '检查实验矩阵中尚未填写的一格', priority: 92, estimate: 240, coord: [55, 92], rationale: '直接决定论文实验可信度' }, 0)
  tasks.aHypothesis = await createTask(paperA.id, { title: '冻结研究问题与三条可检验假设', success: '问题定义与 H1/H2/H3 写入实验设计文档', minimum: '重读贡献段并改写一条可证伪假设', priority: 88, status: 'completed', estimate: 90, coord: [35, 88], rationale: '已完成的主推进准备工作' }, 1, aMilestone.id)
  tasks.aBaseline = await createTask(paperA.id, { title: '跑通 AFL++、FirmAE 与当前方法三组基线', success: '相同样本和预算下输出覆盖率、崩溃数与有效漏洞数', minimum: '启动最小样本上的 AFL++ 30 分钟 smoke test', priority: 99, status: 'in_progress', estimate: 360, coord: [72, 96], rationale: '方法仍有环境不确定性，直接决定验收' }, 2, aMilestone.id)
  tasks.aSensitivity = await createTask(paperA.id, { title: '完成约束阈值参数敏感性实验', success: '形成至少 5 个阈值点的曲线与解释', minimum: '复制基准配置并只修改一个阈值', priority: 91, estimate: 180, coord: [64, 83], rationale: '高贡献且参数行为仍需验证' }, 3, aMilestone.id)
  tasks.aCharts = await createTask(paperA.id, { title: '整理主结果图表与统计显著性', success: '产出投稿尺寸图表、置信区间与统计检验', minimum: '把最新 CSV 汇总进 results_master.csv', priority: 84, estimate: 150, coord: [28, 82], rationale: '路线明确，直接服务论文结果章节' }, 4, aMilestone.id)
  tasks.aWriting = await createTask(paperA.id, { title: '撰写方法与实验设置章节', success: '完成 1800 字可供合作者批注的版本', minimum: '写出方法总览图下的 5 句说明', priority: 79, estimate: 180, coord: [32, 76], rationale: '稳定产出的写作任务' }, 5, aMilestone.id)

  const bMilestone = await createTask(paperB.id, { type: 'milestone', title: 'B1｜形成可内审完整初稿', success: '六个章节完整、关键图表齐全且引用可追溯', minimum: '更新初稿目录中的章节完成度', priority: 86, estimate: 300, coord: [48, 90], rationale: '整合第二篇投稿所需全部成果' }, 0)
  tasks.bBoundary = await createTask(paperB.id, { title: '锁定核心贡献、适用边界与反例', success: '形成一页贡献边界说明并经合作者确认', minimum: '补写一个明确不解决的场景', priority: 82, status: 'completed', estimate: 100, coord: [38, 90], rationale: '已完成的重要定位工作' }, 1, bMilestone.id)
  tasks.bDataset = await createTask(paperB.id, { title: '构建 120 条漏洞根因标注集', success: '双人复核 120 条样本并记录一致性', minimum: '完成下一批 5 条样本标注', priority: 95, status: 'in_progress', estimate: 420, coord: [68, 94], rationale: '数据质量高度不确定且直接决定实验' }, 2, bMilestone.id)
  tasks.bRetrieval = await createTask(paperB.id, { title: '实现检索增强的根因证据链模块', success: '对每个候选输出代码位置、调用路径与证据摘要', minimum: '为一个样本保存检索 top-5 结果', priority: 93, estimate: 300, coord: [78, 91], rationale: '核心方法模块，技术路线尚需验证' }, 3, bMilestone.id)
  tasks.bAblation = await createTask(paperB.id, { title: '完成消融与失败案例分析', success: '覆盖无检索、无调用图、不同模型三组消融', minimum: '列出当前缺失的失败案例字段', priority: 89, status: 'blocked', blocked_reason: '标注集尚未达到 80 条，失败类型分布不稳定', estimate: 210, coord: [82, 72], rationale: '高贡献但受数据集依赖阻塞' }, 4, bMilestone.id)
  tasks.bIntro = await createTask(paperB.id, { title: '完成摘要与引言一页稿', success: '一页内讲清问题、缺口、方法和三点贡献', minimum: '写一个 120 字问题场景', priority: 76, estimate: 120, coord: [34, 80], rationale: '清晰可执行的写作推进' }, 5, bMilestone.id)

  const fMilestone = await createTask(firmware.id, { type: 'milestone', title: 'F1｜形成端到端漏洞挖掘流水线', success: '一条命令完成样本登记、解包、筛选、验证和报告', minimum: '检查流水线 README 的下一处空缺', priority: 83, estimate: 360, coord: [62, 88], rationale: '方法复杂且直接决定研究复用效率' }, 0)
  tasks.fSamples = await createTask(firmware.id, { title: '整理 30 个路由器固件样本与元数据', success: '样本来源、版本、架构、哈希与许可证完整', minimum: '补齐下一个样本的 SHA256 与版本号', priority: 68, status: 'completed', estimate: 150, coord: [22, 58], rationale: '流程明确，提供基础输入' }, 1, fMilestone.id)
  tasks.fUnpack = await createTask(firmware.id, { title: '自动化解包与文件系统识别', success: '30 个样本中至少 90% 自动识别架构和根文件系统', minimum: '对失败样本运行一次 binwalk 并保存日志', priority: 87, status: 'in_progress', estimate: 240, coord: [58, 78], rationale: '存在格式差异，需要尽早验证' }, 2, fMilestone.id)
  tasks.fCallgraph = await createTask(firmware.id, { title: '提取危险函数调用图并生成候选列表', success: '输出包含 source、sink、路径长度与置信度的候选表', minimum: '手工核对一个 strcpy 调用链', priority: 85, estimate: 240, coord: [74, 70], rationale: '分析精度仍有不确定性' }, 3, fMilestone.id)
  tasks.fValidate = await createTask(firmware.id, { title: '验证两个高风险漏洞候选', success: '得到崩溃证据、影响版本、最小 PoC 和修复建议', minimum: '为最高置信候选准备一次最小输入', priority: 98, estimate: 300, coord: [88, 97], rationale: '关键风险任务，直接提供论文案例' }, 4, fMilestone.id)
  tasks.fTemplate = await createTask(firmware.id, { title: '固化复现脚本与漏洞证据模板', success: '新案例可在 15 分钟内生成标准化证据包', minimum: '补充模板中的环境版本字段', priority: 72, estimate: 120, coord: [30, 66], rationale: '路线明确且能提高复用效率' }, 5, fMilestone.id)
  tasks.fPlugin = await createTask(firmware.id, { title: '比较三个反编译器插件的适用性', success: '记录各插件在 MIPS/ARM 样本上的优缺点', minimum: '只测试一个插件在一个样本上的导入结果', priority: 28, estimate: 120, coord: [84, 28], rationale: '不确定性高但当前贡献有限，需严格限时' }, 6, fMilestone.id)
  tasks.fCleanup = await createTask(firmware.id, { title: '整理历史固件下载目录', success: '重复文件归档并补齐命名规则', minimum: '处理下载目录中的前 10 个文件', priority: 20, estimate: 60, coord: [18, 20], rationale: '必要维护，但不应占用主要研究时间' }, 7, fMilestone.id)

  const sessions = [
    [tasks.aBaseline, '2026-09-08T02:30:00Z', 90, '完成 AFL++ 环境校准并定位 QEMU 卡死原因'],
    [tasks.aSensitivity, '2026-09-09T03:00:00Z', 90, '完成首轮阈值网格并导出覆盖率曲线'],
    [tasks.aCharts, '2026-09-10T03:00:00Z', 60, '统一三组实验结果字段与置信区间'],
    [tasks.bDataset, '2026-09-11T06:30:00Z', 90, '完成 20 条样本双人复核'],
    [tasks.fPlugin, '2026-09-12T07:00:00Z', 60, '比较插件导入效果，确认其中两款不适合批处理'],
    [tasks.fCleanup, '2026-09-13T07:30:00Z', 120, '清理历史下载目录并统一样本命名'],
    [tasks.aBaseline, '2026-09-14T02:30:00Z', 110, '三组基线在首批 8 个样本上跑通'],
    [tasks.fValidate, '2026-09-14T06:30:00Z', 90, '复现第一个高风险候选的稳定崩溃'],
    [tasks.bDataset, '2026-09-15T03:00:00Z', 100, '标注集推进到 65 条并修订分类准则'],
    [tasks.aCharts, '2026-09-15T06:30:00Z', 70, '生成主结果表 v2 与显著性标记'],
    [tasks.fUnpack, '2026-09-15T08:00:00Z', 45, '修复 SquashFS 变体识别问题'],
    [tasks.fPlugin, '2026-09-16T01:30:00Z', 30, '限时确认插件结论后停止继续投入', true],
    [tasks.bIntro, '2026-09-16T03:00:00Z', 50, '完成引言问题场景和相关缺口草稿'],
  ]
  for (const session of sessions) await addSession(...session)

  await createPlan('2026-09-14', [tasks.aBaseline, tasks.fValidate, tasks.aCharts], [
    { type: 'result', summary: '三组基线在 8 个样本上完成首轮可比结果' },
    { type: 'step', summary: '获得第一个稳定崩溃并保存输入与调用栈' },
    { type: 'minimum_action', summary: '建立主结果表字段并导入首批数据' },
  ])
  await createPlan('2026-09-15', [tasks.bDataset, tasks.aCharts, tasks.fUnpack], [
    { type: 'result', summary: '完成 65 条根因样本标注并修订准则' },
    { type: 'step', summary: '产出主结果表 v2 和统计显著性标记' },
    { type: 'time', summary: '专注修复 SquashFS 变体识别 45 分钟' },
  ])

  let today = await call('/api/v1/daily-plans/current')
  if (today.data.items.filter(item => item.kind === 'core').length === 0) {
    const { data: replanJob } = await call('/api/v1/daily-plans/generation-jobs', {
      method: 'POST', headers: { 'Idempotency-Key': key('replan') }, body: {
        local_date: '2026-09-16', timezone: 'Asia/Shanghai', available_minutes: 180,
        replace_existing: true, base_revision: today.data.plan.revision,
      },
    })
    const replan = await waitJob(replanJob.id)
    if (replan.status !== 'succeeded') throw new Error(`daily replan failed: ${replan.error_message}`)
    today = await call('/api/v1/daily-plans/current')
  }
  if (!today.data.items.some(item => item.status === 'satisfied') && today.data.items[0]) {
    const item = today.data.items[0]
    await call(`/api/v1/daily-plans/${today.data.plan.id}/items/${item.id}/completions`, {
      method: 'POST', headers: {
        'If-Match': etag('dpi', item.id, item.revision), 'Idempotency-Key': key('today'),
      }, body: { type: 'step', summary: '已完成上午第一轮推进，记录了可复现实验结果' },
    })
  }

  await createConversation('论文 A｜基线实验受阻', paperA.id, [
    '三组基线已经在 8 个样本上跑通，但 FirmAE 有两个样本一直卡在网络初始化。我下午 2 点之后状态最好，帮我判断今天应该继续排环境还是先扩大其他样本。',
    '我不想为了两个异常样本拖住整张结果表。请给我一个 15 分钟内能执行、并且能留下判断证据的动作。',
  ])
  await createConversation('论文 B｜贡献边界与写作', paperB.id, [
    '第二篇论文现在最担心的是贡献看起来像简单调用大模型。我们已经有调用图检索和证据链，但标注只有 65 条。请帮我把本周目标收敛成一个可验证结果。',
    '上午 9 点我确实起不来，稳定工作窗口是 10:30 到 12:00。请按这个现实约束给最小行动，不要安排满整天。',
  ])
  await createConversation('固件漏洞挖掘｜样本与验证', firmware.id, [
    '目前有一个 MIPS 路由器样本能稳定触发崩溃，但还没有最小 PoC，也不确定是否能越权。下一步应该先做影响确认还是继续找第二个漏洞？',
  ])

  if (!existingDevices.some(device => device.name === '实验室墨水屏｜演示设备')) {
    await call('/api/v1/devices', {
      method: 'POST', headers: { 'Idempotency-Key': key('device') }, body: {
        name: '实验室墨水屏｜演示设备', kind: 'eink_panel', timezone: 'Asia/Shanghai',
        capabilities: { width: 800, height: 480, color_mode: 'monochrome' },
      },
    })
  }

  const [goals, allTasks, review, currentPlan, conversations, workSessions] = await Promise.all([
    call('/api/v1/goals'), call('/api/v1/tasks'), call('/api/v1/reviews/weekly'),
    call('/api/v1/daily-plans/current'), call('/api/v1/conversations'), call('/api/v1/work-sessions'),
  ])
  console.log(JSON.stringify({
    result: 'PASS', goals: goals.data.items.length, tasks: allTasks.data.items.length,
    current_core_items: currentPlan.data.items.filter(item => item.kind === 'core').length,
    current_satisfied: currentPlan.data.items.filter(item => item.status === 'satisfied').length,
    conversations: conversations.data.items.length, work_sessions: workSessions.data.items.length,
    weekly_review: { week: review.data.week, rule: review.data.summary.rule, minutes: review.data.focus.total_minutes },
  }, null, 2))
}

main().catch(error => {
  console.error(error.stack || error.message || String(error))
  process.exit(1)
})
