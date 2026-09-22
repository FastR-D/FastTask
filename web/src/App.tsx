import { CSSProperties, FormEvent, KeyboardEvent, useCallback, useEffect, useRef, useState } from 'react'
import { ApiError, ensureFreshAccessToken, hasRefreshToken, idem, login, logout, request, token } from './api'
import type { Device, Goal, Job, Plan, PlanItem, Proposal, Task, TaskTree, User, WorkSession } from './types'
import { fieldValue, useMduiEvent } from './mdui-react'
import { Admin } from './Admin'
import { GoalMapView, Review } from './Lens'
import { PwaUpdate } from './PwaUpdate'
import { AgentChat } from './agent'
import { localDateInTimezone } from './date'

type Tab = 'today' | 'goals' | 'dialogue' | 'jobs' | 'review' | 'devices' | 'admin'

const NAV_ICONS: Record<Tab, string> = {
  today: 'today', goals: 'account_tree', dialogue: 'forum', jobs: 'pending',
  review: 'fact_check', devices: 'devices', admin: 'admin_panel_settings',
}
const BAR_TABS: Tab[] = ['today', 'goals', 'dialogue', 'jobs', 'review']

export function App() {
  const [authenticated, setAuthenticated] = useState(Boolean(token.get()))
  // Cold start of an installed PWA has an empty in-memory access token but a
  // persisted refresh token (pwa.md §4). Restore the session silently rather
  // than flashing the login screen; if the refresh token is gone or rejected the
  // user lands on Login as before.
  const [restoring, setRestoring] = useState(() => !token.get() && hasRefreshToken())
  useEffect(() => {
    if (!restoring) return
    let alive = true
    ensureFreshAccessToken().then(restored => {
      if (!alive) return
      setAuthenticated(Boolean(restored))
      setRestoring(false)
    })
    return () => { alive = false }
  }, [restoring])
  const handleLogout = useCallback(async () => { await logout(); setAuthenticated(false) }, [])
  if (restoring) return null
  return (
    <>
      {authenticated
        ? <Workspace onLogout={handleLogout} />
        : <Login onLogin={() => setAuthenticated(true)} />}
      <PwaUpdate />
    </>
  )
}

function Login({ onLogin }: { onLogin: () => void }) {
  const [identifier, setIdentifier] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  async function submit() {
    if (busy || !identifier.trim() || !password) return
    setBusy(true); setError('')
    try { await login(identifier.trim(), password); onLogin() }
    catch (e) { setError(e instanceof Error ? e.message : '登录失败') }
    finally { setBusy(false) }
  }
  function onSubmit(event: FormEvent<HTMLFormElement>) { event.preventDefault(); submit() }
  function onKeyDown(event: KeyboardEvent<HTMLFormElement>) { if (event.key === 'Enter') { event.preventDefault(); submit() } }
  return <main className="login-shell">
    <section className="login-story">
      <p className="eyebrow">FAST RESEARCH / 3SIGNALS</p>
      <h1>把漫长的研究，<br/>压缩成今天能动手的三件事。</h1>
      <p className="story-sub">目标不是另一张待办清单。目标应该每天产生清晰、可验证、足够小的行动。</p>
      <div className="signal-row"><span>01 选择</span><span>02 专注</span><span>03 证据</span></div>
      <span className="story-glyph" aria-hidden="true">03</span>
    </section>
    <div className="login-panel">
      <form className="login-card" onSubmit={onSubmit} onKeyDown={onKeyDown}>
        <div><p className="eyebrow">WELCOME BACK</p><h2 className="ts-headline-small">进入工作台</h2></div>
        <mdui-text-field label="账号" variant="outlined" required autocomplete="username" value={identifier} onChange={e => setIdentifier(fieldValue(e))}/>
        <mdui-text-field label="密码" type="password" toggle-password variant="outlined" required autocomplete="current-password" value={password} onChange={e => setPassword(fieldValue(e))}/>
        {error && <p className="login-error" role="alert"><mdui-icon name="error_outline"/>{error}</p>}
        <mdui-button variant="filled" full-width disabled={busy} loading={busy} onClick={submit}>{busy ? '正在验证…' : '开始今天'}</mdui-button>
        <small>生产环境请使用管理员下发账号，登录状态会安全地保留在此设备上，退出登录时一并清除。</small>
      </form>
    </div>
  </main>
}

function Workspace({ onLogout }: { onLogout: () => void | Promise<void> }) {
  const [tab, setTab] = useState<Tab>('today')
  const [user, setUser] = useState<User | null>(null)
  const [notice, setNotice] = useState('')
  const [drawerOpen, setDrawerOpen] = useState(false)
  const drawerRef = useRef<HTMLElement>(null)
  const snackbarRef = useRef<HTMLElement>(null)
  useEffect(() => { request<User>('/me').then(r => setUser(r.data)).catch(e => { if (e instanceof ApiError && e.status === 401) onLogout() }) }, [onLogout])
  useMduiEvent(drawerRef, 'close', () => setDrawerOpen(false))
  useMduiEvent(snackbarRef, 'close', () => setNotice(''))
  const items = navItems(user)
  const currentLabel = items.find(([id]) => id === tab)?.[1] ?? ''
  return <mdui-layout className="app-layout">
    <mdui-navigation-drawer ref={drawerRef} className="app-drawer" modal close-on-esc close-on-overlay-click open={drawerOpen}>
      <div className="drawer-inner">
        <div className="drawer-head"><span className="brand-mark">3</span><div><b>FastTask</b><small>research momentum</small></div></div>
        <mdui-list className="drawer-nav">
          {items.map(([id, label]) => <mdui-list-item key={id} icon={NAV_ICONS[id]} active={tab === id} onClick={() => { setTab(id); setDrawerOpen(false) }}>{label}</mdui-list-item>)}
        </mdui-list>
        <div className="drawer-foot">
          <mdui-divider/>
          <mdui-list>
            <mdui-list-item icon="logout" onClick={onLogout}>退出登录</mdui-list-item>
          </mdui-list>
        </div>
      </div>
    </mdui-navigation-drawer>
    <mdui-navigation-rail className="app-rail" value={tab} divider>
      <div slot="top" className="rail-brand"><span className="brand-mark">3</span></div>
      {items.map(([id, label]) => <mdui-navigation-rail-item key={id} value={id} icon={NAV_ICONS[id]} onClick={() => setTab(id)}>{label}</mdui-navigation-rail-item>)}
      <div slot="bottom" className="rail-profile">
        <mdui-tooltip content={`${user?.display_name || '加载中'} · ${user?.timezone || ''}`} placement="left">
          <span className="rail-avatar">{user?.display_name?.slice(0, 1) || 'F'}</span>
        </mdui-tooltip>
        <mdui-button-icon icon="logout" onClick={onLogout}/>
      </div>
    </mdui-navigation-rail>
    <mdui-top-app-bar className="app-bar">
      <mdui-button-icon icon="menu" onClick={() => setDrawerOpen(true)}/>
      <mdui-top-app-bar-title>FastTask · {currentLabel}</mdui-top-app-bar-title>
      <mdui-button-icon icon="logout" onClick={onLogout}/>
    </mdui-top-app-bar>
    <mdui-layout-main className="app-main">
      <div className={tab==='dialogue' ? 'page page-dialogue' : 'page'}>
        {tab==='today' && <Today onNotice={setNotice} timezone={user?.timezone}/>}
        {tab==='goals' && <Goals onNotice={setNotice}/>}
        {tab==='dialogue' && <AgentChat goalId={null} onNotice={setNotice}/>}
        {tab==='jobs' && <JobQueue onNotice={setNotice}/>}
        {tab==='review' && <Review onNotice={setNotice}/>}
        {tab==='devices' && <Devices onNotice={setNotice} timezone={user?.timezone}/>}
        {tab==='admin' && user?.role==='admin' && <Admin user={user} onNotice={setNotice}/>}
      </div>
    </mdui-layout-main>
    <mdui-navigation-bar className="app-nav" value={tab} label-visibility="labeled">
      {items.filter(([id]) => BAR_TABS.includes(id)).map(([id, label]) => <mdui-navigation-bar-item key={id} value={id} icon={NAV_ICONS[id]} onClick={() => setTab(id)}>{label}</mdui-navigation-bar-item>)}
    </mdui-navigation-bar>
    <mdui-snackbar ref={snackbarRef} className="app-snackbar" placement="top" closeable auto-close-delay={4000} open={Boolean(notice)}>{notice}</mdui-snackbar>
  </mdui-layout>
}

function navItems(user: User | null): [Tab,string][] {
  const items: [Tab,string][] = [['today','今日'],['goals','目标树'],['dialogue','对话'],['jobs','任务队列'],['review','复盘'],['devices','设备']]
  if (user?.role === 'admin') items.push(['admin','后台'])
  return items
}

function Today({ onNotice, timezone }: { onNotice: (s:string)=>void; timezone?: string }) {
  const [plan, setPlan] = useState<Plan | null>(null)
  const [items, setItems] = useState<PlanItem[]>([])
  const [loading, setLoading] = useState(true)
  const [session, setSession] = useState<WorkSession | null>(null)
  const [elapsed, setElapsed] = useState(0)
  async function load() {
    setLoading(true)
    try { const {data} = await request<{plan:Plan;items:PlanItem[]}>('/daily-plans/current'); setPlan(data.plan); setItems(data.items) }
    catch (e) { if (!(e instanceof ApiError && e.status===404)) onNotice(e instanceof Error?e.message:'加载失败'); setPlan(null); setItems([]) }
    finally { setLoading(false) }
    const sessions = await request<{items:WorkSession[]}>('/work-sessions').catch(()=>null)
    setSession(sessions?.data.items.find(s=>['running','paused'].includes(s.status)) || null)
  }
  useEffect(()=>{load()},[])
  useEffect(()=>{if(!session||session.status!=='running')return;const startedAt=new Date(session.started_at).getTime();const tick=()=>setElapsed(Math.max(0,Math.floor((Date.now()-startedAt)/1000)));tick();const timer=setInterval(tick,1000);return()=>clearInterval(timer)},[session])
	async function generate(replace=false) { if (!timezone) { onNotice('正在加载用户时区，请稍后重试'); return } const localDate=localDateInTimezone(new Date(),timezone); try { const {data}=await request<Job>('/daily-plans/generation-jobs',{method:'POST',headers:{'Idempotency-Key':idem()},body:JSON.stringify({local_date:localDate,timezone,available_minutes:120,replace_existing:replace,base_revision:replace?plan?.revision:undefined})}); onNotice(`计划作业已创建：${data.id}`); const job=await waitJob(data.id);if(job.status!=='succeeded')throw new Error(job.error_message||'计划作业失败');load() } catch(e){onNotice(errorText(e))} }
  async function satisfy(item:PlanItem,type='minimum_action'){try{await request(`/daily-plans/${item.daily_plan_id}/items/${item.id}/completions`,{method:'POST',headers:{'If-Match':etag('dpi',item.id,item.revision),'Idempotency-Key':idem()},body:JSON.stringify({type,summary:type==='minimum_action'?'已完成今天的最小行动':'已获得可验证推进'})});onNotice('已记录推进证据，底层任务仍保持开放');load()}catch(e){onNotice(errorText(e))}}
  async function start(item:PlanItem){if(!item.task_id)return;try{const {data}=await request<WorkSession>('/work-sessions',{method:'POST',headers:{'Idempotency-Key':idem()},body:JSON.stringify({task_id:item.task_id,daily_plan_item_id:item.id,session_type:'pomodoro',target_minutes:25,started_at:new Date().toISOString()})});setSession(data);onNotice('专注计时开始')}catch(e){onNotice(errorText(e))}}
  async function finish(){if(!session)return;try{await request(`/work-sessions/${session.id}/completions`,{method:'POST',headers:{'If-Match':etag('work',session.id,session.revision),'Idempotency-Key':idem()},body:JSON.stringify({ended_at:new Date().toISOString(),outcome:'progressed',note:'从今日工作台完成'})});setSession(null);onNotice('专注记录已保存')}catch(e){onNotice(errorText(e))}}
  const coreItems = items.filter(i=>i.kind==='core')
  const done = coreItems.filter(i=>i.status==='satisfied').length
  const total = coreItems.length
  return <section className="today-page">
    <header className="page-head">
      <div><p className="eyebrow">TODAY / {new Date().toLocaleDateString('zh-CN',{timeZone:timezone,month:'long',day:'numeric',weekday:'long'})}</p><h1>今天只推进<br/><em>真正重要</em>的事。</h1></div>
      <div className="progress-orbit" style={{'--done':`${total ? Math.round((done/total)*100) : 0}%`} as CSSProperties}><strong>{done}<small>/{total}</small></strong><span>核心信号</span></div>
    </header>
    {session && <div className="focus-strip">
      <div className="focus-info"><span className="pulse"/><b>专注进行中</b><span className="focus-timer">{Math.floor(elapsed/60).toString().padStart(2,'0')}:{(elapsed%60).toString().padStart(2,'0')}</span></div>
      <mdui-button variant="filled" onClick={finish}>结束并记录</mdui-button>
    </div>}
    {loading ? <div className="loading-block"><mdui-linear-progress/><p className="empty">正在整理今天的信号…</p></div> : !plan ? <div className="empty-state"><span>∴</span><h2 className="ts-headline-small">今天还没有计划</h2><p>系统会从活跃目标中选择最多三个有明确最小行动的任务。</p><mdui-button variant="filled" icon="auto_awesome" onClick={()=>generate(false)}>生成今日计划</mdui-button></div> : <>
		<div className="plan-toolbar"><mdui-button variant="tonal" icon="refresh" onClick={()=>generate(true)}>基于最新进展重规划</mdui-button></div>
    <div className="today-grid">{coreItems.map((item,index)=><mdui-card key={item.id} variant="outlined" className={item.status==='satisfied'?'signal-card done':'signal-card'}>
      <div className="signal-index">0{index+1}</div>
      <div className="signal-content">
        <p className="meta">{item.status==='satisfied'?'已满足':'核心推进'} · {item.target_minutes} 分钟</p>
        <h2 className="ts-title-large">{item.title}</h2>
        <p>{item.commitment}</p>
        <div className="minimum"><span>最小行动</span>{item.minimum_action}</div>
        <div className="actions">
          <mdui-button variant="outlined" icon="play_arrow" onClick={()=>start(item)} disabled={Boolean(session)||item.status==='satisfied'}>开始专注</mdui-button>
          <mdui-button variant="filled" icon={item.status==='satisfied'?'check_circle':undefined} onClick={()=>satisfy(item)} disabled={item.status==='satisfied'}>{item.status==='satisfied'?'已记录':'完成最小行动'}</mdui-button>
        </div>
      </div>
    </mdui-card>)}</div>
      {items.length===0&&<div className="empty-state"><h2 className="ts-headline-small">合法的空计划</h2><p>当前没有符合条件的任务。请先创建目标、补充最小行动或解除阻碍。</p></div>}
    </>}
  </section>
}

function Goals({ onNotice }: { onNotice:(s:string)=>void }) {
	const [goals,setGoals]=useState<Goal[]>([]);const [tasks,setTasks]=useState<Task[]>([]);const [selected,setSelected]=useState<Goal|null>(null);const [showCreate,setShowCreate]=useState(false);const [tree,setTree]=useState<TaskTree|null>(null);const [treeETag,setTreeETag]=useState('');const [revisionInstruction,setRevisionInstruction]=useState('');const [view,setView]=useState<'list'|'map'>('list');const [focusTask,setFocusTask]=useState('')
  const createDialogRef = useRef<HTMLElement>(null)
  useMduiEvent(createDialogRef, 'close', () => setShowCreate(false))
  const [goalDraft,setGoalDraft]=useState({title:'',criteria:'',target:'',description:''})
  const [taskDraft,setTaskDraft]=useState({title:'',criteria:'',minimum:''})
	async function load(goal=selected){const g=await request<{items:Goal[]}>('/goals');setGoals(g.data.items);const t=await request<{items:Task[]}>('/tasks');setTasks(t.data.items);const active=goal||g.data.items[0]||null;if(!selected&&active)setSelected(active);if(active){const response=await request<TaskTree>(`/goals/${active.id}/task-tree`);setTree(response.data);setTreeETag(response.etag||etag('tree',active.id,response.data.revision))}}
  useEffect(()=>{load().catch(e=>onNotice(errorText(e)))},[])
  async function createGoal(){if(!goalDraft.title.trim()||!goalDraft.criteria.trim())return;try{await request('/goals',{method:'POST',headers:{'Idempotency-Key':idem()},body:JSON.stringify({title:goalDraft.title,description:goalDraft.description,success_criteria:goalDraft.criteria,target_date:goalDraft.target||null})});setShowCreate(false);setGoalDraft({title:'',criteria:'',target:'',description:''});onNotice('目标已创建');load()}catch(e){onNotice(errorText(e))}}
  async function addTask(){if(!selected||!taskDraft.title.trim()||!taskDraft.criteria.trim()||!taskDraft.minimum.trim())return;try{await request('/tasks',{method:'POST',headers:{'Idempotency-Key':idem()},body:JSON.stringify({goal_id:selected.id,type:'task',title:taskDraft.title,description:'',success_criteria:taskDraft.criteria,minimum_action:taskDraft.minimum,estimate_minutes:50,priority:70,position:tasks.length})});setTaskDraft({title:'',criteria:'',minimum:''});onNotice('任务已加入目标树');load()}catch(e){onNotice(errorText(e))}}
	async function askAgent(revision=false){if(!selected)return;try{const path=revision?'revision-jobs':'generation-jobs';const headers:Record<string,string>={'Idempotency-Key':idem()};if(revision)headers['If-Match']=treeETag;const instruction=revision?revisionInstruction:'生成三层以内的可执行任务树，每个任务必须有最小行动';const {data}=await request<Job>(`/goals/${selected.id}/task-tree/${path}`,{method:'POST',headers,body:JSON.stringify({instruction})});onNotice(`作业 ${data.id} 已进入队列`);const job=await waitJob(data.id);if(job.status!=='succeeded')throw new Error(job.error_message||'Agent 作业失败');await load(selected);setRevisionInstruction('')}catch(e){onNotice(errorText(e))}}
	async function decide(proposal:Proposal,apply:boolean){if(!selected)return;try{if(apply){await request(`/goals/${selected.id}/task-tree/proposals/${proposal.id}/application`,{method:'POST',headers:{'If-Match':treeETag,'Idempotency-Key':idem()}});onNotice('提案已确认并应用')}else{await request(`/goals/${selected.id}/task-tree/proposals/${proposal.id}/rejection`,{method:'PUT',headers:{'If-Match':etag('proposal',proposal.id,proposal.revision)}});onNotice('提案已拒绝')}await load(selected)}catch(e){onNotice(errorText(e))}}
  return <section className="goals-page"><header className="page-head compact"><div><p className="eyebrow">GOAL MAP</p><h1>目标不是终点，<br/>它是一张<em>可修正的地图</em>。</h1></div><mdui-button variant="filled" icon={showCreate?'close':'add'} onClick={()=>setShowCreate(v=>!v)}>新建目标</mdui-button></header>
    {showCreate&&<mdui-dialog ref={createDialogRef} className="goal-dialog" open headline="新建目标" description="长期目标定义方向，验收标准决定什么才算完成。">
      <div className="form-stack">
        <mdui-text-field label="目标标题" variant="outlined" required value={goalDraft.title} onChange={e=>setGoalDraft(d=>({...d,title:fieldValue(e)}))}/>
        <mdui-text-field label="什么状态算真正完成？" variant="outlined" required value={goalDraft.criteria} onChange={e=>setGoalDraft(d=>({...d,criteria:fieldValue(e)}))}/>
        <mdui-text-field label="目标日期" type="date" variant="outlined" value={goalDraft.target} onChange={e=>setGoalDraft(d=>({...d,target:fieldValue(e)}))}/>
        <mdui-text-field label="背景与约束" variant="outlined" rows={3} value={goalDraft.description} onChange={e=>setGoalDraft(d=>({...d,description:fieldValue(e)}))}/>
      </div>
      <mdui-button slot="action" onClick={()=>setShowCreate(false)}>取消</mdui-button>
      <mdui-button slot="action" variant="filled" disabled={!goalDraft.title.trim()||!goalDraft.criteria.trim()} onClick={createGoal}>创建</mdui-button>
    </mdui-dialog>}
		<div className="goal-layout">
      <mdui-list className="goal-list">
        {goals.map(goal=><mdui-list-item key={goal.id} active={selected?.id===goal.id} onClick={()=>{setSelected(goal);setView('list');setFocusTask('');load(goal)}}>
          <span slot="icon" className={`goal-status ${goal.status}`}/>
          <span className="goal-item-text"><b>{goal.title}</b><small>{goal.success_criteria}</small></span>
        </mdui-list-item>)}
        {goals.length===0&&<p className="empty">先建立第一个长期目标。</p>}
      </mdui-list>
		<div className="tree-panel">{selected?<>
      <div className="tree-head"><div><p className="meta">{selected.status.toUpperCase()} · TREE REV {tree?.revision??0}</p><h2 className="ts-headline-small">{selected.title}</h2><p>{selected.success_criteria}</p></div><mdui-button variant="tonal" icon="auto_awesome" onClick={()=>askAgent(false)}>让 Agent 拆解</mdui-button></div>
      <mdui-segmented-button-group className="view-switch" selects="single" value={view} onChange={e=>setView(fieldValue(e) as 'list'|'map')}>
        <mdui-segmented-button value="list" icon="list">列表</mdui-segmented-button>
        <mdui-segmented-button value="map" icon="map">地图</mdui-segmented-button>
      </mdui-segmented-button-group>
      {tree?.proposals.map(proposal=><ProposalCard key={proposal.id} proposal={proposal} onApply={()=>decide(proposal,true)} onReject={()=>decide(proposal,false)}/>)}
      <div className="revision-request">
        <mdui-text-field variant="outlined" value={revisionInstruction} onChange={e=>setRevisionInstruction(fieldValue(e))} placeholder="例如：把实验任务拆小，并移到当前里程碑下"/>
        <mdui-button variant="outlined" icon="edit_note" onClick={()=>askAgent(true)} disabled={!revisionInstruction.trim()}>生成修订提案</mdui-button>
      </div>
      {view==='map'?<GoalMapView goal={selected} onNotice={onNotice} onOpenTask={id=>{setFocusTask(id);setView('list')}}/>:<div className="task-stack">{tasks.filter(t=>t.goal_id===selected.id).map(task=><mdui-card key={task.id} variant="outlined" className={focusTask===task.id?'task-card focused':'task-card'}>
        <span className="task-type">{task.type}</span>
        <div><h3 className="ts-title-medium">{task.title}</h3><p>{task.success_criteria}</p><small>下一步 · {task.minimum_action}</small></div>
        <strong>{task.priority}</strong>
      </mdui-card>)}</div>}
      <div className="task-create">
        <mdui-text-field variant="outlined" label="任务" value={taskDraft.title} onChange={e=>setTaskDraft(d=>({...d,title:fieldValue(e)}))} placeholder="手工添加一个真实任务" required/>
        <mdui-text-field variant="outlined" label="完成标准" value={taskDraft.criteria} onChange={e=>setTaskDraft(d=>({...d,criteria:fieldValue(e)}))} placeholder="完成标准" required/>
        <mdui-text-field variant="outlined" label="最小行动" value={taskDraft.minimum} onChange={e=>setTaskDraft(d=>({...d,minimum:fieldValue(e)}))} placeholder="5-15 分钟最小行动" required/>
        <mdui-button variant="tonal" icon="add" disabled={!taskDraft.title.trim()||!taskDraft.criteria.trim()||!taskDraft.minimum.trim()} onClick={addTask}>添加</mdui-button>
      </div>
    </>:<div className="empty">选择一个目标查看任务树。</div>}</div></div>
  </section>
}

function ProposalCard({proposal,onApply,onReject}:{proposal:Proposal;onApply:()=>void;onReject:()=>void}){let operations:Record<string,unknown>[]=[];try{operations=JSON.parse(proposal.patch_json)}catch{return null}return <mdui-card variant="outlined" className="proposal-card"><p className="eyebrow">AGENT PROPOSAL · BASE REV {proposal.base_revision}</p><h3 className="ts-title-large">待确认的任务树变更</h3>{operations.map((operation,index)=><div className="proposal-op" key={index}><b>{String(operation.op||'create')}</b><span>{String(operation.title||operation.target_id||'未命名变更')}</span><small>{String(operation.minimum_action||operation.success_criteria||'')}</small></div>)}<div className="actions"><mdui-button variant="outlined" onClick={onReject}>拒绝</mdui-button><mdui-button variant="filled" onClick={onApply}>确认应用</mdui-button></div></mdui-card>}

type JobFilter = 'active' | 'queued' | 'running' | 'all' | 'failed' | 'succeeded'

export function JobQueue({ onNotice }: { onNotice:(s:string)=>void }) {
  const [jobs,setJobs]=useState<Job[]>([]);const [filter,setFilter]=useState<JobFilter>('active');const [loading,setLoading]=useState(true);const [connected,setConnected]=useState(true);const [updatedAt,setUpdatedAt]=useState<Date|null>(null);const [now,setNow]=useState(Date.now());const [mutating,setMutating]=useState('')
  useEffect(()=>{const timer=setInterval(()=>setNow(Date.now()),1000);return()=>clearInterval(timer)},[])
  useEffect(()=>{let stopped=false;let timer:number|undefined;let controller:AbortController|undefined
    async function poll(){controller?.abort();controller=new AbortController();try{const {data}=await request<{items:Job[]}>('/agent-jobs',{signal:controller.signal});if(stopped)return;setJobs(data.items);setConnected(true);setUpdatedAt(new Date());setLoading(false)}catch(e){if(stopped||e instanceof DOMException&&e.name==='AbortError')return;setConnected(false);setLoading(false)}finally{if(!stopped)timer=window.setTimeout(poll,2000)}}
    poll();return()=>{stopped=true;controller?.abort();if(timer)clearTimeout(timer)}
  },[])
  async function act(job:Job,action:'cancel'|'retry'){setMutating(job.id);try{const path=action==='cancel'?'cancellation':'retries';await request(`/agent-jobs/${job.id}/${path}`,{method:action==='cancel'?'PUT':'POST',headers:{'If-Match':etag('job',job.id,job.revision),'Idempotency-Key':idem()}});onNotice(action==='cancel'?'已提交取消请求':'已创建重试作业');const {data}=await request<{items:Job[]}>('/agent-jobs');setJobs(data.items);setUpdatedAt(new Date())}catch(e){onNotice(errorText(e))}finally{setMutating('')}}
  const activeStatuses=new Set(['queued','running']);const counts={active:jobs.filter(j=>activeStatuses.has(j.status)).length,queued:jobs.filter(j=>j.status==='queued').length,running:jobs.filter(j=>j.status==='running').length,failed:jobs.filter(j=>j.status==='failed').length,succeeded:jobs.filter(j=>j.status==='succeeded').length}
  const visible=jobs.filter(job=>filter==='all'||filter==='active'&&activeStatuses.has(job.status)||job.status===filter).slice(0,50)
  const stats:[JobFilter,string,number][]=[['active','活跃',counts.active],['queued','排队',counts.queued],['running','运行中',counts.running],['failed','失败',counts.failed],['succeeded','成功',counts.succeeded],['all','全部',jobs.length]]
  return <section className="jobs-page"><header className="page-head compact jobs-head"><div><p className="eyebrow">AGENT JOBS / LIVE</p><h1>每一个后台动作，<br/>都应该<em>看得见进度</em>。</h1></div><div className={connected?'live-indicator online':'live-indicator offline'}><span/><div><b>{connected?'实时连接':'连接中断'}</b><small>{updatedAt?`更新于 ${updatedAt.toLocaleTimeString('zh-CN',{hour12:false})}`:'正在连接队列'}</small></div></div></header>
    <div className="job-stats">{stats.map(([key,label,count])=><mdui-chip key={key} selectable selected={filter===key} onClick={()=>setFilter(key)}><span>{label}</span><b>{count}</b></mdui-chip>)}</div>
    {!connected&&<div className="queue-warning"><mdui-icon name="warning_amber"/>暂时无法刷新队列，正在保留最后一次成功读取的数据并自动重连。</div>}
    {loading?<p className="empty">正在连接任务队列…</p>:visible.length===0?<div className="empty-state"><span>✓</span><h2 className="ts-headline-small">{filter==='active'?'当前没有活跃作业':'该分类暂时没有作业'}</h2><p>任务拆解、计划生成、对话回复和语音转写会实时出现在这里。</p></div>:<div className="job-list">{visible.map(job=><JobRow key={job.id} job={job} now={now} busy={mutating===job.id} onAction={act}/>)}</div>}
  </section>
}

function JobRow({job,now,busy,onAction}:{job:Job;now:number;busy:boolean;onAction:(job:Job,action:'cancel'|'retry')=>void}){
  const active=job.status==='queued'||job.status==='running';const start=job.started_at?new Date(job.started_at).getTime():new Date(job.created_at).getTime();const end=job.finished_at?new Date(job.finished_at).getTime():now;const elapsed=Math.max(0,Math.floor((end-start)/1000));const retrying=job.status==='queued'&&job.attempt_count>0
  return <mdui-card variant="outlined" className={`job-row status-${job.status}`}><div className="job-state"><span className={active?'pulse-job':''}/><b>{retrying?'等待重试':jobStatus(job.status)}</b><small>{formatDuration(elapsed)}</small></div><div className="job-main"><div className="job-title"><h3 className="ts-title-medium">{jobType(job.type)}</h3><code>{job.id}</code></div><p>{job.subject_type?`${subjectType(job.subject_type)} · ${shortID(job.subject_id)}`:'系统作业'}{job.base_revision>0?` · 基于版本 ${job.base_revision}`:''}</p><div className="attempt-track"><mdui-linear-progress value={Math.min(1,Math.max(0.08,job.attempt_count/Math.max(1,job.max_attempts)))}/><span>尝试 {job.attempt_count}/{job.max_attempts}</span></div>{job.error_message&&<details className="job-error"><summary>{job.status==='succeeded'?'曾失败后恢复':job.error_code||'最近一次错误'}</summary><p>{job.error_message}</p></details>}<small className="job-time">创建 {new Date(job.created_at).toLocaleString()} · 更新 {new Date(job.updated_at).toLocaleString()}</small></div><div className="job-controls">{active&&<mdui-button variant="outlined" disabled={busy||job.cancel_requested} onClick={()=>onAction(job,'cancel')}>{job.cancel_requested?'取消中':'取消'}</mdui-button>}{['failed','cancelled'].includes(job.status)&&<mdui-button variant="filled" icon="replay" disabled={busy} onClick={()=>onAction(job,'retry')}>{busy?'处理中':'重试'}</mdui-button>}</div></mdui-card>
}

function jobStatus(status:string){return ({queued:'排队中',running:'运行中',succeeded:'已成功',failed:'已失败',cancelled:'已取消'} as Record<string,string>)[status]||status}
function jobType(type:string){return ({task_tree_generation:'生成任务树',task_tree_revision:'修订任务树',daily_plan_generation:'生成每日计划',support_item_generation:'生成辅助任务',conversation:'生成对话回复',voice_transcription:'语音转写'} as Record<string,string>)[type]||type.replaceAll('_',' ')}
function subjectType(type:string){return ({goal:'目标',conversation:'对话',daily_plan:'每日计划',audio:'音频',user:'用户'} as Record<string,string>)[type]||type}
function shortID(id?:string){return id?id.length>18?id.slice(0,10)+'…'+id.slice(-6):id:'—'}
function formatDuration(seconds:number){const minutes=Math.floor(seconds/60);const rest=seconds%60;return minutes?`${minutes}分${rest.toString().padStart(2,'0')}秒`:`${rest}秒`}

function Devices({ onNotice, timezone }: { onNotice:(s:string)=>void; timezone?: string }) {
  const [devices,setDevices]=useState<Device[]>([]);const [shownToken,setShownToken]=useState('');const [deviceName,setDeviceName]=useState('')
  async function load(){const {data}=await request<{items:Device[]}>('/devices');setDevices(data.items)}useEffect(()=>{load().catch(e=>onNotice(errorText(e)))},[])
  async function create(){if(!deviceName.trim())return;if(!timezone){onNotice('正在加载用户时区，请稍后重试');return}try{const {data}=await request<{device:Device;device_token:string}>('/devices',{method:'POST',headers:{'Idempotency-Key':idem()},body:JSON.stringify({name:deviceName,kind:'eink_panel',timezone,capabilities:{width:800,height:480,color_mode:'monochrome'}})});setShownToken(data.device_token);setDeviceName('');load()}catch(e){onNotice(errorText(e))}}
  return <section className="devices-page"><header className="page-head compact"><div><p className="eyebrow">QUIET DISPLAY</p><h1>把注意力留在桌面，<br/>而不是<em>通知中心</em>。</h1></div></header>
    {shownToken&&<div className="token-box"><div><b>设备 Token 仅显示一次</b><code>{shownToken}</code></div><mdui-button variant="tonal" icon="content_copy" onClick={()=>navigator.clipboard.writeText(shownToken)}>复制</mdui-button></div>}
    <div className="device-grid">
      <mdui-card variant="outlined" className="device-add">
        <mdui-icon className="add-icon" name="add_circle"/>
        <h2 className="ts-title-large">连接墨水屏</h2>
        <p>创建一个只读、可撤销的设备身份。</p>
        <mdui-text-field variant="outlined" label="设备名称" value={deviceName} onChange={e=>setDeviceName(fieldValue(e))} placeholder="例如：办公室墨水屏" required/>
        <mdui-button variant="filled" disabled={!deviceName.trim()} onClick={create}>生成设备 Token</mdui-button>
      </mdui-card>
      {devices.map(device=><mdui-card variant="outlined" className="device-card" key={device.id}>
        <div className="screen-preview"><span>3SIGNALS</span><b>01</b><p>今天最重要的任务</p></div>
        <h3 className="ts-title-medium">{device.name}</h3>
        <p><span className={`status ${device.status}`}/>{device.status} · {device.timezone}</p>
        <code>{device.id}</code>
      </mdui-card>)}
    </div>
  </section>
}

async function waitJob(id:string){for(let i=0;i<30;i++){await new Promise(r=>setTimeout(r,250));const {data}=await request<Job>(`/agent-jobs/${id}`);if(['succeeded','failed','cancelled'].includes(data.status))return data}throw new Error('作业仍在运行，请稍后刷新')}
function etag(kind:string,id:string,revision:number){return `"${kind}_${id}_rev_${revision}"`}
function errorText(error:unknown){return error instanceof Error?error.message:'操作失败'}
