import { FormEvent, useEffect, useRef, useState } from 'react'
import { ApiError, idem, login, logout, request, token } from './api'
import type { Conversation, Device, Goal, Job, Message, Plan, PlanItem, Proposal, Task, TaskTree, User, WorkSession } from './types'
import { Admin } from './Admin'
import { GoalMapView, Review } from './Lens'

type Tab = 'today' | 'goals' | 'dialogue' | 'jobs' | 'review' | 'devices' | 'admin'

export function App() {
  const [authenticated, setAuthenticated] = useState(Boolean(token.get()))
  if (!authenticated) return <Login onLogin={() => setAuthenticated(true)} />
	return <Workspace onLogout={async () => { await logout(); setAuthenticated(false) }} />
}

function Login({ onLogin }: { onLogin: () => void }) {
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault(); setBusy(true); setError('')
    const form = new FormData(event.currentTarget)
    try { await login(String(form.get('identifier')), String(form.get('password'))); onLogin() }
    catch (e) { setError(e instanceof Error ? e.message : '登录失败') }
    finally { setBusy(false) }
  }
  return <main className="login-shell">
    <section className="login-story">
      <p className="eyebrow">FAST RESEARCH / 3SIGNALS</p>
      <h1>把漫长的研究，<br/>压缩成今天能动手的三件事。</h1>
      <p>目标不是另一张待办清单。目标应该每天产生清晰、可验证、足够小的行动。</p>
      <div className="signal-row"><span>01 选择</span><span>02 专注</span><span>03 证据</span></div>
    </section>
    <form className="login-card" onSubmit={submit}>
      <div><p className="eyebrow">WELCOME BACK</p><h2>进入工作台</h2></div>
      <label>账号<input name="identifier" autoComplete="username" required /></label>
      <label>密码<input name="password" type="password" autoComplete="current-password" required /></label>
      {error && <p className="error">{error}</p>}
      <button className="primary" disabled={busy}>{busy ? '正在验证…' : '开始今天'}</button>
      <small>生产环境请使用管理员下发账号，登录状态仅在当前浏览器标签会话中保留。</small>
    </form>
  </main>
}

function Workspace({ onLogout }: { onLogout: () => void | Promise<void> }) {
  const [tab, setTab] = useState<Tab>('today')
  const [user, setUser] = useState<User | null>(null)
  const [notice, setNotice] = useState('')
  useEffect(() => { request<User>('/me').then(r => setUser(r.data)).catch(e => { if (e instanceof ApiError && e.status === 401) onLogout() }) }, [onLogout])
  return <div className="app-shell">
    <aside className="rail">
      <div className="brand"><span className="brand-mark">3</span><div><b>FastTask</b><small>research momentum</small></div></div>
      <nav>{navItems(user).map(([id,label],index)=><button key={id} className={tab===id?'active':''} onClick={()=>setTab(id)}><span>0{index+1}</span>{label}</button>)}</nav>
      <div className="profile"><span className="avatar">{user?.display_name?.slice(0,1) || 'F'}</span><div><b>{user?.display_name || '加载中'}</b><small>{user?.timezone}</small></div><button className="text-button" onClick={onLogout}>退出</button></div>
    </aside>
    <main className="canvas">
      {notice && <div className="toast" onClick={()=>setNotice('')}>{notice}</div>}
      {tab==='today' && <Today onNotice={setNotice}/>} 
      {tab==='goals' && <Goals onNotice={setNotice}/>} 
      {tab==='dialogue' && <Dialogue onNotice={setNotice}/>} 
      {tab==='jobs' && <JobQueue onNotice={setNotice}/>}
      {tab==='review' && <Review onNotice={setNotice}/>} 
      {tab==='devices' && <Devices onNotice={setNotice}/>} 
      {tab==='admin' && user?.role==='admin' && <Admin user={user} onNotice={setNotice}/>}
    </main>
    <nav className="mobile-nav">{navItems(user).map(([id,label])=><button key={id} className={tab===id?'active':''} onClick={()=>setTab(id)}>{label}</button>)}</nav>
  </div>
}

function navItems(user: User | null): [Tab,string][] {
  const items: [Tab,string][] = [['today','今日'],['goals','目标树'],['dialogue','对话'],['jobs','任务队列'],['review','复盘'],['devices','设备']]
  if (user?.role === 'admin') items.push(['admin','后台'])
  return items
}

function Today({ onNotice }: { onNotice: (s:string)=>void }) {
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
	async function generate(replace=false) { const now=new Date(); const localDate=now.toLocaleDateString('en-CA',{timeZone:'Asia/Shanghai'}); try { const {data}=await request<Job>('/daily-plans/generation-jobs',{method:'POST',headers:{'Idempotency-Key':idem()},body:JSON.stringify({local_date:localDate,timezone:'Asia/Shanghai',available_minutes:120,replace_existing:replace,base_revision:replace?plan?.revision:undefined})}); onNotice(`计划作业已创建：${data.id}`); const job=await waitJob(data.id);if(job.status!=='succeeded')throw new Error(job.error_message||'计划作业失败');load() } catch(e){onNotice(errorText(e))} }
  async function satisfy(item:PlanItem,type='minimum_action'){try{await request(`/daily-plans/${item.daily_plan_id}/items/${item.id}/completions`,{method:'POST',headers:{'If-Match':etag('dpi',item.id,item.revision),'Idempotency-Key':idem()},body:JSON.stringify({type,summary:type==='minimum_action'?'已完成今天的最小行动':'已获得可验证推进'})});onNotice('已记录推进证据，底层任务仍保持开放');load()}catch(e){onNotice(errorText(e))}}
  async function start(item:PlanItem){if(!item.task_id)return;try{const {data}=await request<WorkSession>('/work-sessions',{method:'POST',headers:{'Idempotency-Key':idem()},body:JSON.stringify({task_id:item.task_id,daily_plan_item_id:item.id,session_type:'pomodoro',target_minutes:25,started_at:new Date().toISOString()})});setSession(data);onNotice('专注计时开始')}catch(e){onNotice(errorText(e))}}
  async function finish(){if(!session)return;try{await request(`/work-sessions/${session.id}/completions`,{method:'POST',headers:{'If-Match':etag('work',session.id,session.revision),'Idempotency-Key':idem()},body:JSON.stringify({ended_at:new Date().toISOString(),outcome:'progressed',note:'从今日工作台完成'})});setSession(null);onNotice('专注记录已保存')}catch(e){onNotice(errorText(e))}}
  const done=items.filter(i=>i.kind==='core'&&i.status==='satisfied').length
  return <section>
    <header className="page-head"><div><p className="eyebrow">TODAY / {new Date().toLocaleDateString('zh-CN',{month:'long',day:'numeric',weekday:'long'})}</p><h1>今天只推进<br/><em>真正重要</em>的事。</h1></div><div className="progress-orbit"><strong>{done}<small>/{items.filter(i=>i.kind==='core').length || 3}</small></strong><span>核心信号</span></div></header>
    {session && <div className="focus-strip"><div><span className="pulse"/><b>专注进行中</b><small>{Math.floor(elapsed/60).toString().padStart(2,'0')}:{(elapsed%60).toString().padStart(2,'0')}</small></div><button onClick={finish}>结束并记录</button></div>}
    {loading ? <p className="empty">正在整理今天的信号…</p> : !plan ? <div className="empty-state"><span>∴</span><h2>今天还没有计划</h2><p>系统会从活跃目标中选择最多三个有明确最小行动的任务。</p><button className="primary" onClick={()=>generate(false)}>生成今日计划</button></div> : <>
		<div className="plan-toolbar"><button onClick={()=>generate(true)}>基于最新进展重规划</button></div><div className="today-grid">{items.filter(i=>i.kind==='core').map((item,index)=><article className={`signal-card ${item.status==='satisfied'?'done':''}`} key={item.id}><div className="signal-index">0{index+1}</div><div className="signal-content"><p className="meta">{item.status==='satisfied'?'已满足':'核心推进'} · {item.target_minutes} 分钟</p><h2>{item.title}</h2><p>{item.commitment}</p><div className="minimum"><span>最小行动</span>{item.minimum_action}</div><div className="actions"><button onClick={()=>start(item)} disabled={Boolean(session)||item.status==='satisfied'}>开始专注</button><button className="primary" onClick={()=>satisfy(item)} disabled={item.status==='satisfied'}>{item.status==='satisfied'?'已记录':'完成最小行动'}</button></div></div></article>)}</div>
      {items.length===0&&<div className="empty-state"><h2>合法的空计划</h2><p>当前没有符合条件的任务。请先创建目标、补充最小行动或解除阻碍。</p></div>}
    </>}
  </section>
}

function Goals({ onNotice }: { onNotice:(s:string)=>void }) {
	const [goals,setGoals]=useState<Goal[]>([]);const [tasks,setTasks]=useState<Task[]>([]);const [selected,setSelected]=useState<Goal|null>(null);const [showCreate,setShowCreate]=useState(false);const [tree,setTree]=useState<TaskTree|null>(null);const [treeETag,setTreeETag]=useState('');const [revisionInstruction,setRevisionInstruction]=useState('');const [view,setView]=useState<'list'|'map'>('list');const [focusTask,setFocusTask]=useState('')
	async function load(goal=selected){const g=await request<{items:Goal[]}>('/goals');setGoals(g.data.items);const t=await request<{items:Task[]}>('/tasks');setTasks(t.data.items);const active=goal||g.data.items[0]||null;if(!selected&&active)setSelected(active);if(active){const response=await request<TaskTree>(`/goals/${active.id}/task-tree`);setTree(response.data);setTreeETag(response.etag||etag('tree',active.id,response.data.revision))}}
  useEffect(()=>{load().catch(e=>onNotice(errorText(e)))},[])
  async function createGoal(event:FormEvent<HTMLFormElement>){event.preventDefault();const form=new FormData(event.currentTarget);try{await request('/goals',{method:'POST',headers:{'Idempotency-Key':idem()},body:JSON.stringify({title:form.get('title'),description:form.get('description'),success_criteria:form.get('criteria'),target_date:form.get('target')||null})});setShowCreate(false);onNotice('目标已创建');load()}catch(e){onNotice(errorText(e))}}
  async function addTask(event:FormEvent<HTMLFormElement>){event.preventDefault();if(!selected)return;const formElement=event.currentTarget;const form=new FormData(formElement);try{await request('/tasks',{method:'POST',headers:{'Idempotency-Key':idem()},body:JSON.stringify({goal_id:selected.id,type:'task',title:form.get('title'),description:'',success_criteria:form.get('criteria'),minimum_action:form.get('minimum'),estimate_minutes:50,priority:70,position:tasks.length})});formElement.reset();onNotice('任务已加入目标树');load()}catch(e){onNotice(errorText(e))}}
	async function askAgent(revision=false){if(!selected)return;try{const path=revision?'revision-jobs':'generation-jobs';const headers:Record<string,string>={'Idempotency-Key':idem()};if(revision)headers['If-Match']=treeETag;const instruction=revision?revisionInstruction:'生成三层以内的可执行任务树，每个任务必须有最小行动';const {data}=await request<Job>(`/goals/${selected.id}/task-tree/${path}`,{method:'POST',headers,body:JSON.stringify({instruction})});onNotice(`作业 ${data.id} 已进入队列`);const job=await waitJob(data.id);if(job.status!=='succeeded')throw new Error(job.error_message||'Agent 作业失败');await load(selected);setRevisionInstruction('')}catch(e){onNotice(errorText(e))}}
	async function decide(proposal:Proposal,apply:boolean){if(!selected)return;try{if(apply){await request(`/goals/${selected.id}/task-tree/proposals/${proposal.id}/application`,{method:'POST',headers:{'If-Match':treeETag,'Idempotency-Key':idem()}});onNotice('提案已确认并应用')}else{await request(`/goals/${selected.id}/task-tree/proposals/${proposal.id}/rejection`,{method:'PUT',headers:{'If-Match':etag('proposal',proposal.id,proposal.revision)}});onNotice('提案已拒绝')}await load(selected)}catch(e){onNotice(errorText(e))}}
  return <section><header className="page-head compact"><div><p className="eyebrow">GOAL MAP</p><h1>目标不是终点，<br/>它是一张<em>可修正的地图</em>。</h1></div><button className="primary" onClick={()=>setShowCreate(v=>!v)}>新建目标</button></header>
    {showCreate&&<form className="inline-form" onSubmit={createGoal}><input name="title" placeholder="目标标题" required/><input name="criteria" placeholder="什么状态算真正完成？" required/><input name="target" type="date"/><textarea name="description" placeholder="背景与约束"/><button className="primary">创建</button></form>}
		<div className="goal-layout"><div className="goal-list">{goals.map(goal=><button key={goal.id} className={selected?.id===goal.id?'selected':''} onClick={()=>{setSelected(goal);setView('list');setFocusTask('');load(goal)}}><span className={`status ${goal.status}`}/><div><b>{goal.title}</b><small>{goal.success_criteria}</small></div></button>)}{goals.length===0&&<p className="empty">先建立第一个长期目标。</p>}</div>
		<div className="tree-panel">{selected?<><div className="tree-head"><div><p className="meta">{selected.status.toUpperCase()} · TREE REV {tree?.revision??0}</p><h2>{selected.title}</h2><p>{selected.success_criteria}</p></div><button onClick={()=>askAgent(false)}>让 Agent 拆解</button></div><div className="view-switch"><button className={view==='list'?'active':''} onClick={()=>setView('list')}>列表</button><button className={view==='map'?'active':''} onClick={()=>setView('map')}>地图</button></div>{tree?.proposals.map(proposal=><ProposalCard key={proposal.id} proposal={proposal} onApply={()=>decide(proposal,true)} onReject={()=>decide(proposal,false)}/>)}<div className="revision-request"><input value={revisionInstruction} onChange={e=>setRevisionInstruction(e.target.value)} placeholder="例如：把实验任务拆小，并移到当前里程碑下"/><button onClick={()=>askAgent(true)} disabled={!revisionInstruction.trim()}>生成修订提案</button></div>{view==='map'?<GoalMapView goal={selected} onNotice={onNotice} onOpenTask={id=>{setFocusTask(id);setView('list')}}/>:<div className="task-stack">{tasks.filter(t=>t.goal_id===selected.id).map(task=><article key={task.id} className={focusTask===task.id?'focused':''}><span className="task-type">{task.type}</span><div><h3>{task.title}</h3><p>{task.success_criteria}</p><small>下一步 · {task.minimum_action}</small></div><strong>{task.priority}</strong></article>)}</div>}<form className="task-create" onSubmit={addTask}><input name="title" placeholder="手工添加一个真实任务" required/><input name="criteria" placeholder="完成标准" required/><input name="minimum" placeholder="5-15 分钟最小行动" required/><button>添加</button></form></>:<div className="empty">选择一个目标查看任务树。</div>}</div></div>
  </section>
}

function ProposalCard({proposal,onApply,onReject}:{proposal:Proposal;onApply:()=>void;onReject:()=>void}){let operations:Record<string,unknown>[]=[];try{operations=JSON.parse(proposal.patch_json)}catch{return null}return <section className="proposal-card"><p className="eyebrow">AGENT PROPOSAL · BASE REV {proposal.base_revision}</p><h3>待确认的任务树变更</h3>{operations.map((operation,index)=><div className="proposal-op" key={index}><b>{String(operation.op||'create')}</b><span>{String(operation.title||operation.target_id||'未命名变更')}</span><small>{String(operation.minimum_action||operation.success_criteria||'')}</small></div>)}<div className="actions"><button onClick={onReject}>拒绝</button><button className="primary" onClick={onApply}>确认应用</button></div></section>}

function Dialogue({ onNotice }: { onNotice:(s:string)=>void }) {
  const [conversations,setConversations]=useState<Conversation[]>([]);const [current,setCurrent]=useState<Conversation|null>(null);const [messages,setMessages]=useState<Message[]>([]);const [text,setText]=useState('');const [recording,setRecording]=useState(false);const recorder=useRef<MediaRecorder|null>(null);const chunks=useRef<Blob[]>([])
  async function load(){const {data}=await request<{items:Conversation[]}>('/conversations');setConversations(data.items);const chosen=current||data.items[0];if(chosen){setCurrent(chosen);const m=await request<{items:Message[]}>(`/conversations/${chosen.id}/messages`);setMessages(m.data.items)}}
  useEffect(()=>{load().catch(e=>onNotice(errorText(e)))},[])
  async function ensureConversation(){if(current)return current;const {data}=await request<Conversation>('/conversations',{method:'POST',headers:{'Idempotency-Key':idem()},body:JSON.stringify({title:'研究推进对话'})});setCurrent(data);return data}
  async function send(){if(!text.trim())return;try{const conv=await ensureConversation();await request(`/conversations/${conv.id}/messages`,{method:'POST',headers:{'Idempotency-Key':idem()},body:JSON.stringify({content:text})});setText('');onNotice('消息已发送，Agent 正在生成结构化建议');setTimeout(load,700)}catch(e){onNotice(errorText(e))}}
  async function toggleRecord(){if(recording){recorder.current?.stop();setRecording(false);return}try{const stream=await navigator.mediaDevices.getUserMedia({audio:true});const media=new MediaRecorder(stream);chunks.current=[];media.ondataavailable=e=>chunks.current.push(e.data);media.onstop=async()=>{stream.getTracks().forEach(t=>t.stop());const form=new FormData();form.append('audio',new Blob(chunks.current,{type:'audio/webm'}),'voice.webm');try{const {data}=await request<Job>('/voice-transcription-jobs',{method:'POST',headers:{'Idempotency-Key':idem()},body:form});const job=await waitJob(data.id);if(job.output_json){const output=JSON.parse(job.output_json);setText(output.transcript||'')}onNotice('转写完成，请编辑确认后发送')}catch(e){onNotice(errorText(e))}};media.start();recorder.current=media;setRecording(true)}catch{onNotice('浏览器无法访问麦克风，请检查权限') }}
  return <section className="dialogue-page"><header className="page-head compact"><div><p className="eyebrow">CONVERSATION</p><h1>把阻碍说清楚，<br/>再决定<em>要不要修改地图</em>。</h1></div></header><div className="chat-shell"><aside>{conversations.map(c=><button key={c.id} className={current?.id===c.id?'selected':''} onClick={()=>{setCurrent(c);request<{items:Message[]}>(`/conversations/${c.id}/messages`).then(r=>setMessages(r.data.items))}}>{c.title}</button>)}</aside><div className="chat"><div className="messages">{messages.map(m=><div key={m.id} className={`message ${m.role}`}><span>{m.role==='user'?'你':'Agent'}</span><p>{m.content}</p></div>)}{messages.length===0&&<div className="empty-state"><h2>从当前阻碍开始</h2><p>例如：这个实验任务太大，请拆成今天能开始的两步。</p></div>}</div><div className="composer"><textarea value={text} onChange={e=>setText(e.target.value)} placeholder="报告进展、阻碍，或请求调整任务树…"/><button className={recording?'recording':''} onClick={toggleRecord}>{recording?'停止':'语音'}</button><button className="primary" onClick={send}>发送</button></div></div></div></section>
}

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
  return <section className="jobs-page"><header className="page-head compact"><div><p className="eyebrow">AGENT JOBS / LIVE</p><h1>每一个后台动作，<br/>都应该<em>看得见进度</em>。</h1></div><div className={`live-indicator ${connected?'online':'offline'}`}><span/><div><b>{connected?'实时连接':'连接中断'}</b><small>{updatedAt?`更新于 ${updatedAt.toLocaleTimeString('zh-CN',{hour12:false})}`:'正在连接队列'}</small></div></div></header>
    <div className="job-stats"><button className={filter==='active'?'active':''} onClick={()=>setFilter('active')}><span>活跃</span><b>{counts.active}</b></button><button className={filter==='queued'?'active':''} onClick={()=>setFilter('queued')}><span>排队</span><b>{counts.queued}</b></button><button className={filter==='running'?'active':''} onClick={()=>setFilter('running')}><span>运行中</span><b>{counts.running}</b></button><button className={filter==='failed'?'active':''} onClick={()=>setFilter('failed')}><span>失败</span><b>{counts.failed}</b></button><button className={filter==='succeeded'?'active':''} onClick={()=>setFilter('succeeded')}><span>成功</span><b>{counts.succeeded}</b></button><button className={filter==='all'?'active':''} onClick={()=>setFilter('all')}><span>全部</span><b>{jobs.length}</b></button></div>
    {!connected&&<div className="queue-warning">暂时无法刷新队列，正在保留最后一次成功读取的数据并自动重连。</div>}
    {loading?<p className="empty">正在连接任务队列…</p>:visible.length===0?<div className="empty-state"><span>✓</span><h2>{filter==='active'?'当前没有活跃作业':'该分类暂时没有作业'}</h2><p>任务拆解、计划生成、对话回复和语音转写会实时出现在这里。</p></div>:<div className="job-list">{visible.map(job=><JobRow key={job.id} job={job} now={now} busy={mutating===job.id} onAction={act}/>)}</div>}
  </section>
}

function JobRow({job,now,busy,onAction}:{job:Job;now:number;busy:boolean;onAction:(job:Job,action:'cancel'|'retry')=>void}){
  const active=job.status==='queued'||job.status==='running';const start=job.started_at?new Date(job.started_at).getTime():new Date(job.created_at).getTime();const end=job.finished_at?new Date(job.finished_at).getTime():now;const elapsed=Math.max(0,Math.floor((end-start)/1000));const retrying=job.status==='queued'&&job.attempt_count>0
  return <article className={`job-row status-${job.status}`}><div className="job-state"><span className={active?'pulse-job':''}/><b>{retrying?'等待重试':jobStatus(job.status)}</b><small>{formatDuration(elapsed)}</small></div><div className="job-main"><div className="job-title"><h3>{jobType(job.type)}</h3><code>{job.id}</code></div><p>{job.subject_type?`${subjectType(job.subject_type)} · ${shortID(job.subject_id)}`:'系统作业'}{job.base_revision>0?` · 基于版本 ${job.base_revision}`:''}</p><div className="attempt-track"><i style={{width:`${Math.min(100,Math.max(8,(job.attempt_count/Math.max(1,job.max_attempts))*100))}%`}}/><span>尝试 {job.attempt_count}/{job.max_attempts}</span></div>{job.error_message&&<details className="job-error"><summary>{job.error_code||'最近一次错误'}</summary><p>{job.error_message}</p></details>}<small className="job-time">创建 {new Date(job.created_at).toLocaleString()} · 更新 {new Date(job.updated_at).toLocaleString()}</small></div><div className="job-controls">{active&&<button disabled={busy||job.cancel_requested} onClick={()=>onAction(job,'cancel')}>{job.cancel_requested?'取消中':'取消'}</button>}{['failed','cancelled'].includes(job.status)&&<button className="primary" disabled={busy} onClick={()=>onAction(job,'retry')}>{busy?'处理中':'重试'}</button>}</div></article>
}

function jobStatus(status:string){return ({queued:'排队中',running:'运行中',succeeded:'已成功',failed:'已失败',cancelled:'已取消'} as Record<string,string>)[status]||status}
function jobType(type:string){return ({task_tree_generation:'生成任务树',task_tree_revision:'修订任务树',daily_plan_generation:'生成每日计划',support_item_generation:'生成辅助任务',conversation:'生成对话回复',voice_transcription:'语音转写'} as Record<string,string>)[type]||type.replaceAll('_',' ')}
function subjectType(type:string){return ({goal:'目标',conversation:'对话',daily_plan:'每日计划',audio:'音频'} as Record<string,string>)[type]||type}
function shortID(id?:string){return id?id.length>18?id.slice(0,10)+'…'+id.slice(-6):id:'—'}
function formatDuration(seconds:number){const minutes=Math.floor(seconds/60);const rest=seconds%60;return minutes?`${minutes}分${rest.toString().padStart(2,'0')}秒`:`${rest}秒`}

function Devices({ onNotice }: { onNotice:(s:string)=>void }) {
  const [devices,setDevices]=useState<Device[]>([]);const [shownToken,setShownToken]=useState('')
  async function load(){const {data}=await request<{items:Device[]}>('/devices');setDevices(data.items)}useEffect(()=>{load().catch(e=>onNotice(errorText(e)))},[])
  async function create(event:FormEvent<HTMLFormElement>){event.preventDefault();const formElement=event.currentTarget;const form=new FormData(formElement);try{const {data}=await request<{device:Device;device_token:string}>('/devices',{method:'POST',headers:{'Idempotency-Key':idem()},body:JSON.stringify({name:form.get('name'),kind:'eink_panel',timezone:'Asia/Shanghai',capabilities:{width:800,height:480,color_mode:'monochrome'}})});setShownToken(data.device_token);formElement.reset();load()}catch(e){onNotice(errorText(e))}}
  return <section><header className="page-head compact"><div><p className="eyebrow">QUIET DISPLAY</p><h1>把注意力留在桌面，<br/>而不是<em>通知中心</em>。</h1></div></header>{shownToken&&<div className="token-box"><b>设备 Token 仅显示一次</b><code>{shownToken}</code><button onClick={()=>navigator.clipboard.writeText(shownToken)}>复制</button></div>}<div className="device-grid"><form className="device-add" onSubmit={create}><span>＋</span><h2>连接墨水屏</h2><p>创建一个只读、可撤销的设备身份。</p><input name="name" placeholder="例如：办公室墨水屏" required/><button className="primary">生成设备 Token</button></form>{devices.map(device=><article className="device-card" key={device.id}><div className="screen-preview"><span>3SIGNALS</span><b>01</b><p>今天最重要的任务</p></div><h3>{device.name}</h3><p><span className={`status ${device.status}`}/>{device.status} · {device.timezone}</p><code>{device.id}</code></article>)}</div></section>
}

async function waitJob(id:string){for(let i=0;i<30;i++){await new Promise(r=>setTimeout(r,250));const {data}=await request<Job>(`/agent-jobs/${id}`);if(['succeeded','failed','cancelled'].includes(data.status))return data}throw new Error('作业仍在运行，请稍后刷新')}
function etag(kind:string,id:string,revision:number){return `"${kind}_${id}_rev_${revision}"`}
function errorText(error:unknown){return error instanceof Error?error.message:'操作失败'}
