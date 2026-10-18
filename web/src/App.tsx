import { FormEvent, KeyboardEvent, useCallback, useEffect, useRef, useState } from 'react'
import { ApiError, completeFastCAS, completeFastResearch, ensureFreshAccessToken, hasRefreshToken, idem, login, logout, offlineSessionAvailable, onSessionEnded, readOfflineUser, rememberUser, request, token } from './api'
import { FastCASAccount } from './FastCASAccount'
import type { Device, Goal, Job, User } from './types'
import type { GoalChatRequest } from './agent'
import { fieldValue, useMduiEvent } from './mdui-react'
import { Admin } from './Admin'
import { Review } from './Lens'
import { PwaUpdate } from './PwaUpdate'
import { ResearchWorkspace } from './ResearchWorkspace'
import { GoalManager } from './GoalManager'
import { Integrations } from './Integrations'
import { timezoneOrDefault } from './date'
import { assertAccessOwner } from './session-owner'
export { Today, focusEvidence } from './Today'

type Tab = 'today' | 'goals' | 'jobs' | 'review' | 'devices' | 'admin' | 'account' | 'integrations'

const NAV_ICONS: Record<Tab, string> = {
  today: 'today', goals: 'account_tree', jobs: 'pending',
  review: 'fact_check', devices: 'devices', admin: 'admin_panel_settings', account: 'verified_user',
  integrations: 'hub',
}
const BAR_TABS: Tab[] = ['today', 'goals', 'review', 'jobs']

// The API speaks English enums; the interface speaks Chinese. One map each, so a status is
// never shown raw (`ACTIVE · TREE REV 10`) and never translated in three different places.
const DEVICE_STATUS: Record<string,string> = { active: '在用', disabled: '已停用', revoked: '已撤销' }

export function App() {
  const [authenticated, setAuthenticated] = useState(Boolean(token.get()))
  const [callbackPending] = useState(() => ['fastcas', 'fastresearch'].some(key => new URLSearchParams(window.location.search).get(key) === 'callback'))
  const [fastResearchCallback] = useState(() => new URLSearchParams(window.location.search).get('fastresearch') === 'callback')
  const [authError, setAuthError] = useState('')
  // Cold start of an installed PWA has an empty in-memory access token but a
  // persisted refresh token (pwa.md §4). Restore the session silently rather
  // than flashing the login screen; if the refresh token is gone or rejected the
  // user lands on Login as before.
  const [restoring, setRestoring] = useState(() => callbackPending || (!token.get() && hasRefreshToken()))
  useEffect(() => {
    if (!restoring) return
    let alive = true
    const restore = callbackPending ? (fastResearchCallback ? completeFastResearch() : completeFastCAS()).then(() => token.get()) : ensureFreshAccessToken()
    restore.then(restored => {
      if (!alive) return
      setAuthenticated(Boolean(restored) || offlineSessionAvailable())
      setRestoring(false)
    }).catch(error => {
      if (alive) {
        setAuthError(error instanceof Error ? error.message : '外部账号登录失败')
        setAuthenticated(Boolean(token.get()))
        setRestoring(false)
      }
    }).finally(() => { if (callbackPending) window.history.replaceState(null, '', window.location.pathname) })
    return () => { alive = false }
  }, [restoring, callbackPending])
  const handleLogout = useCallback(async () => { await logout(); setAuthenticated(false) }, [])
  // A refresh rejected mid-session, or a logout in another tab, ends this session too.
  useEffect(() => onSessionEnded(() => setAuthenticated(false)), [])
  if (restoring) return null
  return (
    <>
      {authenticated
        ? <Workspace onLogout={handleLogout} requireFreshProfile={callbackPending} />
        : <Login onLogin={() => setAuthenticated(true)} initialError={authError} />}
      <PwaUpdate />
    </>
  )
}

// The server's problem details are English and deliberately vague; the sign-in form says what the
// status means for the person typing instead.
export function loginErrorText(error: unknown): string {
  if (error instanceof ApiError) {
    if (error.status === 401) return '账号或密码不正确'
    if (error.status === 429) return '失败次数过多，请几分钟后再试'
    if (error.status === 422) return '账号或密码格式不正确'
    if (error.status >= 500) return '服务暂时不可用，请稍后再试'
    return error.message || '登录失败'
  }
  if (error instanceof TypeError) return '无法连接服务器，请检查网络'
  return '登录失败'
}

function Login({ onLogin, initialError = '' }: { onLogin: () => void; initialError?: string }) {
  const [identifier, setIdentifier] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState(initialError)
  const [fastcas, setFastcas] = useState(false)
  const [fastResearch, setFastResearch] = useState(false)
  useEffect(() => {
    void request<{enabled:boolean}>('/auth/fastcas/available').then(r => setFastcas(r.data.enabled)).catch(() => {})
    void request<{enabled:boolean}>('/auth/fastresearch/available').then(r => setFastResearch(r.data.enabled)).catch(() => {})
  }, [])
  const [busy, setBusy] = useState(false)
  async function submit() {
    if (busy || !identifier.trim() || !password) return
    setBusy(true); setError('')
    try { await login(identifier.trim(), password); onLogin() }
    catch (e) { setError(loginErrorText(e)) }
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
    </section>
    <div className="login-panel">
      <form className="login-card" onSubmit={onSubmit} onKeyDown={onKeyDown}>
        <div><p className="eyebrow">WELCOME BACK</p><h2>进入工作台</h2></div>
        <mdui-text-field label="账号" variant="outlined" required autocomplete="username" value={identifier} onInput={e => setIdentifier(fieldValue(e))}/>
        <mdui-text-field label="密码" type="password" toggle-password variant="outlined" required autocomplete="current-password" value={password} onInput={e => setPassword(fieldValue(e))}/>
        {error && <p className="login-error" role="alert"><mdui-icon name="error_outline"/>{error}</p>}
        <mdui-button variant="filled" full-width disabled={busy} loading={busy} onClick={submit}>{busy ? '正在验证…' : '开始今天'}</mdui-button>
        {fastResearch && <mdui-button type="button" variant="filled" full-width onClick={() => window.location.assign('/api/v1/auth/fastresearch/login')}>使用 FastResearch Key 登录</mdui-button>}
        {fastcas && !fastResearch && <mdui-button type="button" variant="outlined" full-width onClick={() => window.location.assign('/api/v1/auth/fastcas/login')}>使用 FastCAS 登录</mdui-button>}
        <small>{fastResearch ? '普通用户使用个人 FastResearch Key。管理员仍使用上方 FastTask 账号和密码。' : '生产环境请使用管理员下发账号，登录状态会安全地保留在此设备上，退出登录时一并清除。'}</small>
      </form>
    </div>
  </main>
}

function Workspace({ onLogout,requireFreshProfile=false }: { onLogout: () => void | Promise<void>;requireFreshProfile?:boolean }) {
  const [tab, setTab] = useState<Tab>('today')
  const [user, setUser] = useState<User | null>(()=>requireFreshProfile?null:readOfflineUser())
  const [profileError,setProfileError]=useState('')
  const [profileAttempt,setProfileAttempt]=useState(0)
  const timezone=user?timezoneOrDefault(user.timezone??''):undefined
  const [notice, setNotice] = useState('')
  const [drawerOpen, setDrawerOpen] = useState(false)
  const [businessRevision, setBusinessRevision] = useState(0)
  const [chatRequest, setChatRequest] = useState(0)
  const chatSequence=useRef(0)
  const [goalChatRequest,setGoalChatRequest]=useState<GoalChatRequest>()
  const [toolTaskID, setToolTaskID] = useState('')
  const businessChanged = useCallback(() => setBusinessRevision(value => value + 1), [])
  const openChat = useCallback((goal?:Goal) => {
    const request=++chatSequence.current
    setGoalChatRequest(goal?{goalId:goal.id,title:goal.title,ownerId:user?.id,request}:undefined)
    setTab('today');setChatRequest(request)
  }, [user?.id])
  const openTools = useCallback((taskID: string) => { setToolTaskID(taskID); setTab('integrations') }, [])
  const drawerRef = useRef<HTMLElement>(null)
  const snackbarRef = useRef<HTMLElement>(null)
  useEffect(() => {
    let alive=true
    setProfileError('')
    request<User>('/me').then(({data})=>{
      if(!data?.id)throw new Error('未能确认当前账号，请重试。')
      // The response may have started under a session that another tab has
      // replaced. Never overwrite the new account's retained profile with it.
      assertAccessOwner(token.get(),data.id)
      if(alive){setUser(data);rememberUser(data)}
    }).catch(error=>{
      if(!alive)return
      setProfileError(error instanceof Error?error.message:'暂时无法确认当前账号')
      if(error instanceof ApiError&&error.status===401&&!offlineSessionAvailable())void onLogout()
    })
    return()=>{alive=false}
  },[onLogout,profileAttempt])
  useMduiEvent(drawerRef, 'close', () => setDrawerOpen(false))
  useMduiEvent(snackbarRef, 'close', () => setNotice(''))
  const items = navItems(user)
  const currentLabel = items.find(([id]) => id === tab)?.[1] ?? ''
  if(!user)return <main className="page"><h1>正在打开工作台</h1>
    <p role={profileError?'alert':'status'}>{profileError?`暂时无法确认当前账号：${profileError}`:'正在确认当前账号…'}</p>
    <div className="actions">{profileError&&<mdui-button variant="outlined" onClick={()=>setProfileAttempt(value=>value+1)}>重试确认账号</mdui-button>}<mdui-button variant="text" onClick={onLogout}>退出登录</mdui-button></div>
  </main>
  return <mdui-layout className="app-layout">
    <mdui-navigation-drawer ref={drawerRef} className="app-drawer" modal close-on-esc close-on-overlay-click open={drawerOpen}>
      <div className="drawer-inner">
        <div className="drawer-head"><span className="brand-mark">3</span><div><b>FastTask</b><small>每天推进重要的事</small></div></div>
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
        <mdui-button-icon icon="logout" aria-label="退出登录" onClick={onLogout}/>
      </div>
    </mdui-navigation-rail>
    <mdui-top-app-bar className="app-bar">
      <mdui-button-icon icon="menu" aria-label="打开导航" onClick={() => setDrawerOpen(true)}/>
      <mdui-top-app-bar-title>FastTask · {currentLabel}</mdui-top-app-bar-title>
      <mdui-button-icon icon="logout" aria-label="退出登录" onClick={onLogout}/>
    </mdui-top-app-bar>
    <mdui-layout-main className="app-main">
      <ResearchWorkspace active={tab==='today'} timezone={timezone} userId={user?.id} onNotice={setNotice} refreshVersion={businessRevision} onBusinessChanged={businessChanged} chatRequest={chatRequest} goalChatRequest={goalChatRequest} onOpenTools={openTools}/>
      <div className="page" hidden={tab==='today'}>
        {tab==='goals' && <GoalManager userId={user?.id} onNotice={setNotice} refreshVersion={businessRevision} onBusinessChanged={businessChanged} onOpenChat={openChat}/>}
        {tab==='jobs' && <JobQueue onNotice={setNotice}/>}
        {tab==='review' && <Review onNotice={setNotice}/>}
        {tab==='devices' && <Devices onNotice={setNotice} timezone={timezone}/>}
        {tab==='integrations' && <Integrations user={user} onNotice={setNotice} onBusinessChanged={businessChanged} initialTaskID={toolTaskID}/>}
        {tab==='account' && <FastCASAccount/>}
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
  const items: [Tab,string][] = [['today','工作台'],['goals','目标任务'],['integrations','工具内容'],['review','复盘'],['jobs','后台作业'],['devices','设备'],['account','账号认证']]
  if (user?.role === 'admin') items.push(['admin','后台'])
  return items
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
  const stats:[JobFilter,string,number][]=[['active','活跃',counts.active],['queued','排队',counts.queued],['running','运行中',counts.running],['failed','失败',counts.failed],['succeeded','成功',counts.succeeded],['all','全部',jobs.length]]
  return <section className="jobs-page"><header className="page-head"><div className="title-block"><p className="eyebrow">{counts.failed?`${counts.failed} 个作业失败`:counts.active?`${counts.active} 个作业正在进行`:'队列空闲'}</p><h1>任务队列</h1></div><div className="head-side"><div className={connected?'live-indicator online':'live-indicator offline'}><span/><div><b>{connected?'实时连接':'连接中断'}</b><small>{updatedAt?`更新于 ${updatedAt.toLocaleTimeString('zh-CN',{hour12:false})}`:'正在连接队列'}</small></div></div></div></header>
    <div className="job-stats">{stats.map(([key,label,count])=><mdui-chip key={key} selectable selected={filter===key} onClick={()=>setFilter(key)}><span>{label}</span><b>{count}</b></mdui-chip>)}</div>
    {!connected&&<div className="queue-warning"><mdui-icon name="warning_amber"/>暂时无法刷新队列，正在保留最后一次成功读取的数据并自动重连。</div>}
    {loading?<p className="empty">正在连接任务队列…</p>:visible.length===0?<div className="empty-state"><h2>{filter==='active'?'当前没有活跃作业':'该分类暂时没有作业'}</h2><p>任务拆解、计划生成、对话回复和语音转写会实时出现在这里。</p></div>:<div className="job-list">{visible.map(job=><JobRow key={job.id} job={job} now={now} busy={mutating===job.id} onAction={act}/>)}</div>}
  </section>
}

function JobRow({job,now,busy,onAction}:{job:Job;now:number;busy:boolean;onAction:(job:Job,action:'cancel'|'retry')=>void}){
  const active=job.status==='queued'||job.status==='running';const start=job.started_at?new Date(job.started_at).getTime():new Date(job.created_at).getTime();const end=job.finished_at?new Date(job.finished_at).getTime():now;const elapsed=Math.max(0,Math.floor((end-start)/1000));const retrying=job.status==='queued'&&job.attempt_count>0
  return <mdui-card variant="outlined" className={`job-row status-${job.status}`}><div className="job-state"><span className={active?'pulse-job':''}/><b>{retrying?'等待重试':jobStatus(job.status)}</b><small>{formatDuration(elapsed)}</small></div><div className="job-main"><div className="job-title"><h3>{jobType(job.type)}</h3><code>{shortID(job.id)}</code></div><p>{job.subject_type?`${subjectType(job.subject_type)} · ${shortID(job.subject_id)}`:'系统作业'}{job.base_revision>0?` · 基于版本 ${job.base_revision}`:''}{job.attempt_count>1?` · 第 ${job.attempt_count}/${job.max_attempts} 次尝试`:''}</p>{job.error_message&&<details className="job-error"><summary>{job.status==='succeeded'?'曾失败后恢复':job.error_code||'最近一次错误'}</summary><p>{job.error_message}</p></details>}<small className="job-time">创建 {stamp(job.created_at)} · 更新 {stamp(job.updated_at)}</small></div><div className="job-controls">{active&&<mdui-button variant="outlined" disabled={busy||job.cancel_requested} onClick={()=>onAction(job,'cancel')}>{job.cancel_requested?'取消中':'取消'}</mdui-button>}{['failed','cancelled'].includes(job.status)&&<mdui-button variant="filled" icon="replay" disabled={busy} onClick={()=>onAction(job,'retry')}>{busy?'处理中':'重试'}</mdui-button>}</div></mdui-card>
}

function jobStatus(status:string){return ({queued:'排队中',running:'运行中',succeeded:'已成功',failed:'已失败',cancelled:'已取消'} as Record<string,string>)[status]||status}
function jobType(type:string){return ({task_tree_generation:'生成任务树',task_tree_revision:'修订任务树',daily_plan_generation:'生成每日计划',support_item_generation:'生成辅助任务',conversation:'生成对话回复',voice_transcription:'语音转写'} as Record<string,string>)[type]||type.replaceAll('_',' ')}
function subjectType(type:string){return ({goal:'目标',conversation:'对话',daily_plan:'每日计划',audio:'音频',user:'用户'} as Record<string,string>)[type]||type}
// One abbreviation rule for every id in a row: a 36-character hex string on the title line outweighs
// the title it belongs to (measured 238px of id against an 84px title), and the subject id below was
// already shortened, so the same row was showing one id in full and the next one cut.
function shortID(id?:string){return id?id.length>18?id.slice(0,10)+'…'+id.slice(-6):id:'—'}
// Two full locale timestamps per row read as noise; month-day hour-minute is enough to
// tell a fresh job from yesterday's, and it matches the zh-CN used everywhere else.
function stamp(value:string){return new Date(value).toLocaleString('zh-CN',{month:'2-digit',day:'2-digit',hour:'2-digit',minute:'2-digit',hour12:false})}
function formatDuration(seconds:number){const minutes=Math.floor(seconds/60);const rest=seconds%60;return minutes?`${minutes}分${rest.toString().padStart(2,'0')}秒`:`${rest}秒`}

function Devices({ onNotice, timezone }: { onNotice:(s:string)=>void; timezone?: string }) {
  const [devices,setDevices]=useState<Device[]>([]);const [shownToken,setShownToken]=useState('');const [deviceName,setDeviceName]=useState('')
  async function load(){const {data}=await request<{items:Device[]}>('/devices');setDevices(data.items)}useEffect(()=>{load().catch(e=>onNotice(errorText(e)))},[])
  async function create(){if(!deviceName.trim())return;if(!timezone){onNotice('正在加载用户时区，请稍后重试');return}try{const {data}=await request<{device:Device;device_token:string}>('/devices',{method:'POST',headers:{'Idempotency-Key':idem()},body:JSON.stringify({name:deviceName,kind:'eink_panel',timezone,capabilities:{width:800,height:480,color_mode:'monochrome'}})});setShownToken(data.device_token);setDeviceName('');load()}catch(e){onNotice(errorText(e))}}
  return <section className="devices-page"><header className="page-head"><div className="title-block"><p className="eyebrow">{devices.length?`${devices.length} 台设备已登记`:'尚未登记设备'}</p><h1>设备</h1></div></header>
    {shownToken&&<div className="token-box"><div><b>设备 Token 仅显示一次</b><code>{shownToken}</code></div><mdui-button variant="tonal" icon="content_copy" onClick={()=>navigator.clipboard.writeText(shownToken)}>复制</mdui-button></div>}
    <div className="device-grid">
      <mdui-card variant="outlined" className="device-add">
        <h2>连接墨水屏</h2>
        <p>创建一个只读、可撤销的设备身份。</p>
        <mdui-text-field variant="outlined" label="设备名称" value={deviceName} onInput={e=>setDeviceName(fieldValue(e))} placeholder="例如：办公室墨水屏" required/>
        <mdui-button variant="filled" disabled={!deviceName.trim()} onClick={create}>生成设备 Token</mdui-button>
      </mdui-card>
      {devices.map(device=><mdui-card variant="outlined" className="device-card" key={device.id}>
        <h3>{device.name}</h3>
        <p><span className={`status ${device.status}`}/>{DEVICE_STATUS[device.status]??device.status} · {device.timezone}</p>
        <p className="meta">{device.kind==='eink_panel'?'墨水屏面板 · 只读':device.kind}</p>
        <code>{device.id}</code>
      </mdui-card>)}
    </div>
  </section>
}

function etag(kind:string,id:string,revision:number){return `"${kind}_${id}_rev_${revision}"`}
function errorText(error:unknown){return error instanceof Error?error.message:'操作失败'}
