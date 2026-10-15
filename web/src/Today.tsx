import { useEffect, useRef, useState } from 'react'
import { ApiError, idem, request as apiRequest } from './api'
import { createCreationAttempt, type CreationResponse } from './creation-attempt'
import { SESSION_OWNER_CHANGED } from './session-owner'
import { fieldValue } from './mdui-react'
import { localDateInTimezone, timezoneOrDefault } from './date'
import { waitJob } from './jobs'
import type { CompletionType, Goal, Job, Plan, PlanAvailability, PlanItem, PlanningSettings, ScheduledDay, Task, WorkSession } from './types'

const COMPLETION_LABELS: Record<CompletionType, string> = { minimum_action: '最小行动', step: '完成关键步骤', result: '取得结果', time: '累计专注时长' }
type CompletionDraft = { type: CompletionType; summary: string; focusMinutes: string; completeTask?: boolean; taskETag?: string }
const emptyLearningDraft={title:'',commitment:'',minimum_action:'',minutes:'25'}
function learningFields(item:PlanItem){return {title:item.title,commitment:item.commitment,minimum_action:item.minimum_action,minutes:String(item.target_minutes)}}
function sameLearningField(key:keyof typeof emptyLearningDraft,left:string,right:string){return key==='minutes'?Number(left)===Number(right):left.trim()===right.trim()}
function learningChanges(draft:typeof emptyLearningDraft,item:PlanItem){
  const desired={title:draft.title.trim(),commitment:draft.commitment.trim(),minimum_action:draft.minimum_action.trim(),target_minutes:Number(draft.minutes)}
  return Object.fromEntries(Object.entries(desired).filter(([key,value])=>value!==item[key as keyof PlanItem]))
}
function allowedCompletionTypes(item:PlanItem) {
  const allowed=item.allowed_types?.split(',').map(type=>type.trim())
  return (Object.keys(COMPLETION_LABELS) as CompletionType[]).filter(type=>(!allowed||allowed.includes(type))&&(type!=='time'||Boolean(item.task_id)))
}
function defaultCompletionType(item:PlanItem):CompletionType {
  const allowed=allowedCompletionTypes(item)
  return allowed.includes('minimum_action')?'minimum_action':allowed[0]??'minimum_action'
}
function configuredGoal(settings:PlanningSettings){
  return Object.prototype.hasOwnProperty.call(settings,'configured_primary_goal_id')?settings.configured_primary_goal_id??'':settings.primary_goal_id??''
}

function workspaceURL(value?:string){
  if(!value||/[\x00-\x1f\x7f\\]/.test(value))return undefined
  try{const url=new URL(value);return ['http:','https:'].includes(url.protocol)&&url.hostname&&!url.username&&!url.password&&!url.search&&!url.hash?url.href:undefined}catch{return undefined}
}

// Match the server's time-evidence rule: only ended, valid sessions bound to this
// plan item count, including its earlier revisions. Running timers and sessions
// from another day are not evidence.
export function focusEvidence(item: PlanItem, sessions: WorkSession[]) {
  const completed = [...new Map(sessions.filter(s =>
    (s.daily_plan_item_id === item.id || (item.logical_id && s.daily_plan_item_logical_id === item.logical_id)) &&
    ['completed', 'stopped'].includes(s.status),
  ).map(session => [session.id, session])).values()]
  return { seconds: completed.reduce((total, s) => total + Math.max(0, s.duration_seconds), 0), sessionIds: completed.map(s => s.id) }
}

export function activeFocusSeconds(session: WorkSession, now=Date.now()) {
  if (['completed','stopped'].includes(session.status)) return Math.max(0,session.duration_seconds)
  const end=session.status==='paused'&&session.paused_at?Date.parse(session.paused_at):now
  const started=Date.parse(session.started_at)
  if (!Number.isFinite(end)||!Number.isFinite(started)) return 0
  return Math.max(0,Math.floor((end-started)/1000)-Math.max(0,session.paused_seconds??0))
}

type TodayProps = { userId?:string; onNotice: (message: string) => void; timezone?: string; refreshVersion?: number; onBusinessChanged?: () => void; onOpenChat?: () => void; onOpenTools?: (taskID: string) => void }

export function Today({ userId,onNotice, timezone:profileTimezone, refreshVersion = 0, onBusinessChanged, onOpenChat, onOpenTools }: TodayProps) {
  const timezone=profileTimezone===undefined?undefined:timezoneOrDefault(profileTimezone)
  const owner=useRef(userId)
  owner.current??=userId
  const currentUserId=useRef(userId)
  currentUserId.current=userId
  const ownerChanged=Boolean(owner.current&&owner.current!==userId)
  const [plan, setPlan] = useState<Plan | null>(null)
  const [items, setItems] = useState<PlanItem[]>([])
  const [availability, setAvailability] = useState<PlanAvailability | null>(null)
  const [loading, setLoading] = useState(true)
  const [loadError, setLoadError] = useState('')
  const [sessionsAvailable, setSessionsAvailable] = useState(false)
  const [session, setSession] = useState<WorkSession | null>(null)
  const [sessions, setSessions] = useState<WorkSession[]>([])
  const [elapsed, setElapsed] = useState(0)
  const [drafts, setDrafts] = useState<Record<string, CompletionDraft>>({})
  const [sessionNote, setSessionNote] = useState('')
  const [focusTask,setFocusTask]=useState<{taskID:string;title:string}|null>(null)
  const [settings, setSettings] = useState<PlanningSettings | null>(null)
  const [settingsETag, setSettingsETag] = useState('')
  const [primaryGoal, setPrimaryGoal] = useState('')
  const [budget, setBudget] = useState('120')
  const [goals, setGoals] = useState<Goal[]>([])
  const [allGoals,setAllGoals]=useState<Goal[]>([])
  const [settingsConflict,setSettingsConflict]=useState(false)
  const [schedule, setSchedule] = useState<ScheduledDay[]>([])
  const [scheduleError, setScheduleError] = useState('')
  const [learningDraft,setLearningDraft]=useState({...emptyLearningDraft})
  const [learningError,setLearningError]=useState('')
  const learningAttempt=useRef(createCreationAttempt<PlanItem>())
  const learningCreatePath=useRef<string|null>(null)
  const learningSaved=useRef<CreationResponse<PlanItem>|null>(null)
  const generationAttempt=useRef(createCreationAttempt<Job>())
  const generationJob=useRef<Job|null>(null)
  const [learningConflict,setLearningConflict]=useState<CreationResponse<PlanItem>|null>(null)
  const [expanded, setExpanded] = useState<string | null>(null)
  const loadSequence = useRef(0)
  const settingsSequence=useRef(0)
  const settingsDirty=useRef(false)
  const settingsEditETag=useRef('')
  const latestSettings=useRef<{data:PlanningSettings;etag:string}|null>(null)

  async function ownedRequest<T>(path:string,init:RequestInit={}){
    if(owner.current&&currentUserId.current!==owner.current)throw new Error(SESSION_OWNER_CHANGED)
    return apiRequest<T>(path,init,owner.current)
  }

  function itemKey(item: PlanItem) { return item.logical_id || item.id }

  function draftOf(item: PlanItem): CompletionDraft {
    const draft=drafts[itemKey(item)]??{type:defaultCompletionType(item),summary:'',focusMinutes:String(item.target_minutes||30)}
    const compatible=allowedCompletionTypes(item).includes(draft.type)?draft:{...draft,type:defaultCompletionType(item),completeTask:false}
    return compatible.completeTask&&(!item.can_complete_task||!item.task_etag||compatible.taskETag!==item.task_etag)?{...compatible,completeTask:false}:compatible
  }
  function updateDraft(item: PlanItem, update: Partial<CompletionDraft>) {
    const key = itemKey(item)
    setDrafts(current => ({ ...current, [key]: { ...(current[key] ?? draftOf(item)), ...update,...(update.completeTask!==undefined?{taskETag:update.completeTask?item.task_etag:undefined}:{}) } }))
  }
  // The schedule belongs to the same business snapshot as the main plan. A new
  // load invalidates it immediately, even before that load reaches /schedule.
  async function loadSchedule(sequence:number) {
    try {
      const { data } = await ownedRequest<{items: ScheduledDay[]}>('/schedule')
      if(sequence!==loadSequence.current)return
      setSchedule(data.items)
      setScheduleError('')
    } catch (error) {
      if(sequence===loadSequence.current)setScheduleError(errorText(error))
    }
  }
  function applySettings(data:PlanningSettings,etag:string){
    settingsEditETag.current=etag;settingsDirty.current=false
    setSettingsETag(etag);setPrimaryGoal(configuredGoal(data));setBudget(String(data.daily_budget_minutes));setSettingsConflict(false)
  }
  async function loadSettings() {
    const sequence=++settingsSequence.current
    try {
      const [preferences, goalList] = await Promise.all([
        ownedRequest<PlanningSettings>('/planning-settings'), ownedRequest<{items: Goal[]}>('/goals'),
      ])
      if(sequence!==settingsSequence.current)return
      const etag=preferences.etag||''
      latestSettings.current={data:preferences.data,etag}
      setSettings(preferences.data)
      if(!settingsDirty.current)applySettings(preferences.data,etag)
      else setSettingsConflict(Boolean(etag&&settingsEditETag.current&&etag!==settingsEditETag.current))
      setAllGoals(goalList.data.items)
      setGoals(goalList.data.items.filter(goal => goal.status === 'active'))
    } catch (error) { if(sequence===settingsSequence.current)onNotice(`计划设置加载失败：${errorText(error)}`) }
  }
  async function load():Promise<boolean> {
    const sequence = ++loadSequence.current
    setLoading(true)
    try {
      const [planResponse, sessionResponse] = await Promise.allSettled([
        ownedRequest<ScheduledDay>('/daily-plans/current'),
        ownedRequest<{items: WorkSession[]}>('/work-sessions'),
      ])
      if (sequence !== loadSequence.current) return false
      if (sessionResponse.status === 'fulfilled') {
        setSessionsAvailable(true)
        setSessions(sessionResponse.value.data.items)
        setSession(sessionResponse.value.data.items.find(s => ['running', 'paused'].includes(s.status)) || null)
      } else {
        // The PWA can retain a read-only plan while the online-only session
        // endpoint is unavailable. Do not discard that plan or trust stale time.
        setSessions([]); setSession(null)
        setSessionsAvailable(false)
        onNotice('专注记录暂时无法加载，累计时长待联网后确认')
      }
      if (planResponse.status === 'rejected') throw planResponse.reason
      setPlan(planResponse.value.data.plan); setItems(planResponse.value.data.today_items??planResponse.value.data.items)
      setAvailability(planResponse.value.data.availability ?? null)
      setLoadError('')
      await loadSchedule(sequence)
      if(sequence!==loadSequence.current||owner.current&&currentUserId.current!==owner.current)return false
      // A confirmed job can outlive a failed reload. Any successful current-plan
      // read settles it; reload buttons and business refreshes must not POST again.
      if(generationJob.current?.status==='succeeded'){
        generationJob.current=null;generationAttempt.current.reset()
      }
      return true
    } catch (error) {
      if (sequence !== loadSequence.current) return false
      if (error instanceof ApiError && error.status === 404) {setPlan(null);setItems([]);setAvailability(null);setLoadError('')}
      else {setLoadError(errorText(error));onNotice(errorText(error))}
      return false
    } finally { if (sequence === loadSequence.current) setLoading(false) }
  }
  useEffect(() => { void load(); void loadSettings();return()=>{loadSequence.current++;settingsSequence.current++} }, [refreshVersion,userId])
  useEffect(()=>{
    if(!session){setFocusTask(null);return}
    let alive=true
    const taskID=session.task_id
    const fallback=items.find(item=>item.task_id===session.task_id)?.title||''
    setFocusTask({taskID,title:fallback})
    void ownedRequest<Task>(`/tasks/${encodeURIComponent(taskID)}`).then(({data})=>{if(alive)setFocusTask({taskID,title:data.title})}).catch(()=>{if(alive&&!fallback)setFocusTask({taskID,title:'关联任务暂时无法读取'})})
    return()=>{alive=false}
  },[session?.task_id,refreshVersion,userId])
  async function refreshBusiness() { if (onBusinessChanged) onBusinessChanged(); else await load() }
  // One action at a time, named by what it is acting on, so the button that was pressed shows progress
  // and a second press cannot send a duplicate (a fresh idempotency key each time) or a stale If-Match.
  // The ref is the guard (clicks dispatched before React re-renders all see the same state); the state
  // only drives what the buttons show.
  const [busy,setBusy]=useState('')
  const busyRef=useRef('')
  async function guarded(key:string,action:()=>Promise<void>){if(busyRef.current)return;busyRef.current=key;setBusy(key);try{await action()}finally{busyRef.current='';setBusy('')}}
  useEffect(()=>{if(!session)return;const tick=()=>setElapsed(activeFocusSeconds(session));tick();if(session.status!=='running')return;const timer=setInterval(tick,1000);return()=>clearInterval(timer)},[session])
  async function generate(replace=false) {
    if (!timezone) { onNotice('正在加载用户时区，请稍后重试'); return }
    const localDate = localDateInTimezone(new Date(), timezone)
    await guarded('plan', async () => {
      try {
        if(!generationJob.current){
          if(!generationAttempt.current.pending())generationAttempt.current=createCreationAttempt<Job>(owner.current,()=>currentUserId.current)
          const {data}=await generationAttempt.current.recover('/daily-plans/generation-jobs',{local_date:localDate,timezone,available_minutes:settings?.daily_budget_minutes??120,replace_existing:replace,base_revision:replace?plan?.revision:undefined})
          generationJob.current=data
        }
        let job=generationJob.current
        if(!['succeeded','failed','cancelled'].includes(job.status)){
          onNotice('正在按最新进展安排任务')
          job=await waitJob(job.id,{readJob:async id=>(await ownedRequest<Job>(`/agent-jobs/${id}`)).data})
          generationJob.current=job
        }
        if(job.status!=='succeeded'){
          generationJob.current=null;generationAttempt.current.reset()
          // A terminal failure can be an obsolete base revision. Read the
          // current plan before a later explicit operation chooses its baseline.
          await load()
          throw new Error(job.error_message||(job.status==='cancelled'?'计划作业已取消':'计划作业失败'))
        }
        if(await load())onBusinessChanged?.()
      } catch (error) { onNotice(errorText(error)) }
    })
  }
  async function saveSettings() {
    const minutes = Number(budget)
    if (!settingsETag || settingsConflict || !Number.isInteger(minutes) || minutes < 30 || minutes > 480) return
    await guarded('settings', async () => {
      try {
        const response = await ownedRequest<PlanningSettings>('/planning-settings', {method:'PUT', headers:{'If-Match':settingsETag}, body:JSON.stringify({primary_goal_id:primaryGoal || null, daily_budget_minutes:minutes})})
        settingsSequence.current++;setSettings(response.data);latestSettings.current={data:response.data,etag:response.etag||''};applySettings(response.data,response.etag||'')
        onNotice('计划设置已保存；今日保留，之后的安排已更新')
        await refreshBusiness()
      } catch (error) {
        onNotice(errorText(error))
        if (error instanceof ApiError && error.status === 412) await loadSettings()
      }
    })
  }
  async function satisfy(item: PlanItem) {
    const draft = draftOf(item)
    const evidence = focusEvidence(item, sessions)
    if (!planEditable || item.kind!=='core'&&!supportUnlocked || loadError || !allowedCompletionTypes(item).includes(draft.type) || !draft.summary.trim() || (draft.type === 'time' && evidence.seconds < item.target_minutes * 60) || draft.completeTask&&(!item.can_complete_task||!item.task_etag||draft.type!=='result')) return
    await guarded('satisfy:'+item.id, async () => {
      try {
        await ownedRequest(`/daily-plans/${item.daily_plan_id}/items/${item.id}/completions`, {method:'POST', headers:{'If-Match':item.item_etag??etag('dpi',item.id,item.revision), 'Idempotency-Key':idem()}, body:JSON.stringify({type:draft.type, summary:draft.summary.trim(), ...(draft.type === 'time' ? {work_session_ids:evidence.sessionIds} : {}),...(draft.completeTask?{complete_task:true,task_if_match:draft.taskETag}:{})})})
        onNotice(draft.completeTask?'今日步骤和这项任务的全部产出已一起确认':'今天的推进已记录')
        updateDraft(item,{completeTask:false})
        setExpanded(null)
        await refreshBusiness()
      } catch (error) { onNotice(errorText(error)); if (error instanceof ApiError && error.status === 412) await load() }
    })
  }
  async function reopen(item: PlanItem) {
    if(loadError||!planEditable)return
    await guarded('reopen:'+item.id, async () => {
      try {
        await ownedRequest(`/daily-plans/${item.daily_plan_id}/items/${item.id}/reopenings`, {method:'POST', headers:{'If-Match':item.item_etag??etag('dpi',item.id,item.revision), 'Idempotency-Key':idem()}, body:JSON.stringify({summary:'撤销误标记'})})
        updateDraft(item, {summary:'',completeTask:false})
        onNotice('已撤销今日推进；单独确认的整项任务成果仍保留')
        await refreshBusiness()
      } catch (error) { onNotice(errorText(error)); if (error instanceof ApiError && error.status === 412) await load() }
    })
  }
  async function start(item: PlanItem) {
    const minutes = Number(draftOf(item).focusMinutes)
    if (!planEditable || item.kind!=='core'&&!supportUnlocked || !item.task_id || !sessionsAvailable || loadError || !Number.isInteger(minutes) || minutes < 5 || minutes > 180) return
    await guarded('start:'+item.id, async () => {
      try {
        const {data} = await ownedRequest<WorkSession>('/work-sessions', {method:'POST', headers:{'Idempotency-Key':idem()}, body:JSON.stringify({task_id:item.task_id, daily_plan_item_id:item.id, session_type:'pomodoro', target_minutes:minutes, started_at:new Date().toISOString()})})
        setSession(data); setSessionNote(''); onNotice('专注计时开始')
      } catch (error) { onNotice(errorText(error));if(error instanceof ApiError&&[409,412].includes(error.status))await load() }
    })
  }
  async function pauseOrResume() {
    if(!session)return
    const current=session,action=current.status==='paused'?'resume':'pause'
    await guarded(action,async()=>{
      try{
        const {data}=await ownedRequest<WorkSession>(`/work-sessions/${current.id}/${action==='pause'?'pauses':'resumptions'}`,{method:'POST',headers:{'If-Match':etag('work',current.id,current.revision),'Idempotency-Key':idem()},body:JSON.stringify({at:new Date().toISOString()})})
        setSession(data);onNotice(action==='pause'?'计时已暂停，暂停时间不会计入专注':'继续专注计时')
      }catch(error){onNotice(errorText(error));if(error instanceof ApiError&&[409,412].includes(error.status))await load()}
    })
  }
  async function finish() {
    if (!session || !sessionNote.trim()) return
    const activeSession = session
    await guarded('finish', async () => {
      try {
        const {data:completed}=await ownedRequest<WorkSession>(`/work-sessions/${activeSession.id}/completions`, {method:'POST', headers:{'If-Match':etag('work',activeSession.id,activeSession.revision), 'Idempotency-Key':idem()}, body:JSON.stringify({ended_at:new Date().toISOString(), outcome:'progressed', note:sessionNote.trim()})})
        const activeItem = items.find(item => item.id === activeSession.daily_plan_item_id || item.logical_id&&item.logical_id===activeSession.daily_plan_item_logical_id)
        if (activeItem) {
          const evidence=focusEvidence(activeItem,sessions)
          const seconds=evidence.seconds+(evidence.sessionIds.includes(completed.id)?0:Math.max(0,completed.duration_seconds))
          const useTime=allowedCompletionTypes(activeItem).includes('time')&&seconds>=activeItem.target_minutes*60
          updateDraft(activeItem, {summary:sessionNote.trim(),type:useTime?'time':defaultCompletionType(activeItem),completeTask:false});setExpanded(itemKey(activeItem))
        }
        setSession(null); setSessionNote('')
        await refreshBusiness()
        onNotice('专注记录已保存，可按实际完成的小步骤记录今日推进')
      } catch (error) { onNotice(errorText(error));if(error instanceof ApiError&&[409,412].includes(error.status))await load() }
    })
  }
  async function finishLearningSave(){
    learningAttempt.current.reset();learningCreatePath.current=null;learningSaved.current=null;setLearningConflict(null)
    setLearningDraft({...emptyLearningDraft});setLearningError('');onNotice('学习任务已保存，不计入核心三项');await refreshBusiness()
  }
  function reviewLearning(keepChanges:boolean){
    const saved=learningSaved.current,latest=learningConflict
    if(!saved||!latest)return
    const baseline=learningFields(saved.data),draft=learningDraft,merged=learningFields(latest.data)
    if(keepChanges)for(const key of Object.keys(merged) as (keyof typeof merged)[])if(!sameLearningField(key,draft[key],baseline[key]))merged[key]=draft[key]
    learningSaved.current=latest;setLearningDraft(merged);setLearningConflict(null);setLearningError('')
  }
  async function addLearningItem(){
    const minutes=Number(learningDraft.minutes)
    if(!plan||!supportUnlocked||loadError||learningConflict||!learningDraft.title.trim()||!learningDraft.commitment.trim()||!learningDraft.minimum_action.trim()||!Number.isInteger(minutes)||minutes<5||minutes>180)return
    const activePlan=plan
    const position=Math.max(0,...items.filter(item=>['input','support'].includes(item.kind)).map(item=>item.position??0))+1
    await guarded('add-learning',async()=>{
      setLearningError('')
      try{
        if(!learningSaved.current){
          if(!learningCreatePath.current&&!learningAttempt.current.pending())learningAttempt.current=createCreationAttempt<PlanItem>(owner.current,()=>currentUserId.current)
          learningCreatePath.current??=`/daily-plans/${activePlan.id}/items`
          const recovered=await learningAttempt.current.recover(learningCreatePath.current,{kind:'input',title:learningDraft.title.trim(),commitment:learningDraft.commitment.trim(),minimum_action:learningDraft.minimum_action.trim(),target_minutes:minutes,position},{'If-Match':etag('plan',activePlan.id,activePlan.revision)})
          // Bind the confirmed original row before editing. Even a PATCH whose
          // response is lost must never send another creation request.
          learningSaved.current=recovered;learningAttempt.current.reset();learningCreatePath.current=null
        }
        const saved=learningSaved.current,changes=learningChanges(learningDraft,saved.data)
        if(Object.keys(changes).length)await ownedRequest<PlanItem>(`/daily-plans/${saved.data.daily_plan_id}/items/${saved.data.id}`,{method:'PATCH',headers:{'If-Match':saved.etag||etag('dpi',saved.data.id,saved.data.revision)},body:JSON.stringify(changes)})
        await finishLearningSave()
      }catch(error){
        const saved=learningSaved.current
        if(!saved&&!learningAttempt.current.pending())learningCreatePath.current=null
        if(saved&&error instanceof ApiError&&error.status===412){
          try{
            const latest=await ownedRequest<PlanItem>(`/daily-plans/${saved.data.daily_plan_id}/items/${saved.data.id}`)
            if(!Object.keys(learningChanges(learningDraft,latest.data)).length){await finishLearningSave();return}
            setLearningConflict(latest)
          }catch{/* Keep the original identity and draft until it can be read. */}
        }
        const message=error instanceof ApiError&&error.status===412?(saved?'已找回的学习任务在另一处更新，草稿已保留；请核对后保存到原条目。':'今日安排已在另一处更新，已重新读取；请核对后再次添加。'):error instanceof ApiError&&error.status===409?'当前安排暂时不能保存学习任务，已重新读取；请检查计划状态。':errorText(error)
        if(error instanceof ApiError&&[409,412].includes(error.status))await load()
        setLearningError(message);onNotice(message)
      }
    })
  }
  const coreItems = items.filter(i=>i.kind==='core'&&!['skipped','superseded','not_completed'].includes(i.status))
  const supportItems=items.filter(item=>['input','support'].includes(item.kind)&&!['skipped','superseded','not_completed'].includes(item.status))
  const planEditable=Boolean(plan&&timezone&&!ownerChanged&&plan.status==='active'&&plan.local_date<=localDateInTimezone(new Date(),timezone))
  const supportUnlocked=planEditable
  const pendingGeneration=Boolean(generationJob.current||generationAttempt.current.pending())
  const retainedLearning=ownerChanged&&Boolean(learningDraft.title||learningDraft.commitment||learningDraft.minimum_action||learningAttempt.current.pending()||learningSaved.current)
  const validLearningMinutes=Number.isInteger(Number(learningDraft.minutes))&&Number(learningDraft.minutes)>=5&&Number(learningDraft.minutes)<=180
  const removedItems = items.filter(i=>i.kind==='core'&&i.status==='skipped')
  const unfinishedItems = items.filter(item=>item.status==='not_completed')
  const done = coreItems.filter(i=>i.status==='satisfied').length
  const total = items.filter(item=>item.kind==='core'&&!['skipped','superseded'].includes(item.status)).length
  const canReplan=Boolean(planEditable&&availability?.can_replan)
  const availabilityStatus=availability?.status??(plan?.status!=='active'?'closed':removedItems.length?'withdrawn':'no_tasks')
  const showAvailability=Boolean(plan&&(availabilityStatus==='closed'||coreItems.length<3&&availability||coreItems.length===0&&supportItems.length===0))
  const availabilityTitle:Record<string,string>={ready:'今日安排不足三项',no_tasks:availability?'当前目标还没有工作项':'还没有可安排的任务',waiting_dependencies:'正在等待前置成果',no_ready_tasks:'暂时没有可执行的任务',withdrawn:'今日旧安排已移出',frozen:'今日安排已保留',closed:plan?.status==='cancelled'?'今日安排已取消':'今日安排已关闭'}
  const availabilityReason=availability?.reason||(availabilityStatus==='closed'?'这份安排仅保留历史记录，请查看接下来的排期。':availabilityStatus==='withdrawn'?'旧安排已移出，历史记录仍保留；可查看接下来的排期，或明确重新安排今天。':'可和助手核对当前目标的下一步，不必为了凑满三项增加任务。')
  const currentGoal = goals.find(goal=>goal.id===settings?.primary_goal_id)
  const fixedGoal=allGoals.find(goal=>goal.id===primaryGoal)
  const configuredUnavailable=Boolean(primaryGoal&&!goals.some(goal=>goal.id===primaryGoal))
  function renderItems(list:PlanItem[],optional=false){
    return list.map((item,index)=>{
      const draft=draftOf(item),evidence=focusEvidence(item,sessions),enoughTime=evidence.seconds>=item.target_minutes*60,focusMinutes=Number(draft.focusMinutes),open=expanded===itemKey(item),methods=allowedCompletionTypes(item)
      const canProgress=planEditable&&(!optional||supportUnlocked)
      const workspace=item.workspace,url=workspaceURL(workspace?.url)
      const workspaceAction=workspace&&(url?<mdui-button variant="text" icon="open_in_new" href={url} target="_blank" rel="noreferrer">打开 {workspace.label}</mdui-button>:<><span className="meta">{workspace.label} 网页地址待配置</span>{item.task_id&&onOpenTools&&<mdui-button variant="text" icon="settings" onClick={()=>onOpenTools(item.task_id!)}>设置工具地址</mdui-button>}</>)
      return <article key={itemKey(item)} className={`today-item${item.status==='satisfied'?' done':''}`} aria-label={item.title}>
        <div className="today-item-head">
          <span className="today-item-number" aria-hidden="true">{item.status==='satisfied'?<mdui-icon name="check"/>:optional?<mdui-icon name="menu_book"/>:String(index+1).padStart(2,'0')}</span>
          <div className="today-item-copy"><h2>{item.commitment||item.minimum_action||item.title}</h2>{item.title!==item.commitment&&<small>{item.title}</small>}<p>{item.minimum_action}</p><small>{item.task_id?`今日专注预算 ${item.target_minutes} 分钟 · 已记录 ${Math.floor(evidence.seconds/60)} 分钟`:`建议学习 ${item.target_minutes} 分钟 · 按实际收获记录`}</small></div>
        </div>
        {item.status==='satisfied'?<>
          {item.completion_summary&&<p className="today-evidence">{item.completion_summary}</p>}
          <div className="today-item-actions">{workspaceAction}<span className="meta">{optional?'已记录学习推进':'今天已推进'}</span><mdui-button variant="text" icon="undo" onClick={()=>reopen(item)} disabled={Boolean(busy)||Boolean(loadError)||!planEditable} loading={busy==='reopen:'+item.id}>撤销今日推进</mdui-button></div>
        </>:<>
          <div className="today-item-actions">
            {workspaceAction}
            {item.task_id&&<mdui-button variant="outlined" icon="play_arrow" onClick={()=>start(item)} disabled={!canProgress||!sessionsAvailable||Boolean(loadError)||Boolean(session)||Boolean(busy)||!Number.isInteger(focusMinutes)||focusMinutes<5||focusMinutes>180} loading={busy==='start:'+item.id}>开始专注</mdui-button>}
            <mdui-button variant="text" onClick={()=>setExpanded(open?null:itemKey(item))} aria-expanded={open} aria-controls={`completion-${itemKey(item)}`}>{open?'收起记录':'记录今日推进'}</mdui-button>
          </div>
          <div id={`completion-${itemKey(item)}`} className="today-completion" hidden={!open}>
            <p className="meta">今日的小步推进单独记录，不代表整个任务已经最终完成。</p>
            {drafts[itemKey(item)]?.completeTask&&drafts[itemKey(item)].taskETag!==item.task_etag&&<p role="status" className="meta">任务内容已更新，推进摘要已保留；整项完成需要重新确认。</p>}
            {item.task_id&&onOpenTools&&<mdui-button variant="text" icon="library_books" onClick={()=>onOpenTools(item.task_id!)}>查找工具材料</mdui-button>}
            {methods.length?<mdui-select label="推进方式" aria-label={`${item.title}的推进方式`} variant="outlined" value={draft.type} onChange={event=>{const type=fieldValue(event) as CompletionType;updateDraft(item,{type,...(type!=='result'?{completeTask:false}:{})})}}>{methods.map(type=><mdui-menu-item key={type} value={type}>{COMPLETION_LABELS[type]}</mdui-menu-item>)}</mdui-select>:<p className="meta">这项学习仅允许计时完成，但未关联可计时的任务。</p>}
            {item.can_complete_task&&item.task_etag&&methods.includes('result')&&<mdui-select label="完成范围" aria-label={`${item.title}的完成范围`} variant="outlined" value={draft.completeTask?'task':'today'} onChange={event=>updateDraft(item,{completeTask:fieldValue(event)==='task',...(fieldValue(event)==='task'?{type:'result'}:{})})}>
              <mdui-menu-item value="today">只记录今天的小步骤</mdui-menu-item><mdui-menu-item value="task">这项任务的全部产出也已完成</mdui-menu-item>
            </mdui-select>}
            {draft.completeTask&&<p className="meta">{item.task_success_criteria&&<>整项验收：{item.task_success_criteria}。</>}保存时会一起确认整项任务成果，解锁依赖它的工作。之后撤销今日记录仍保留整项成果；误确认可在目标任务中撤销任务完成。</p>}
            <mdui-text-field label="实际推进摘要" aria-label={`${item.title}的实际推进摘要`} placeholder="例如：完成引言第一段，或排查出一个实验阻碍" variant="outlined" rows={2} value={draft.summary} onInput={event=>updateDraft(item,{summary:fieldValue(event)})}/>
            {draft.type==='time'&&!enoughTime&&<small>先保存专注记录，还需 {Math.ceil((item.target_minutes*60-evidence.seconds)/60)} 分钟。</small>}
            <mdui-button variant="filled" onClick={()=>satisfy(item)} disabled={!canProgress||!methods.includes(draft.type)||Boolean(busy)||Boolean(loadError)||!draft.summary.trim()||(draft.type==='time'&&!enoughTime)} loading={busy==='satisfy:'+item.id}>保存推进记录</mdui-button>
          </div>
          {item.task_id&&<details className="today-focus-options"><summary>本轮专注 {draft.focusMinutes} 分钟 · 调整</summary><mdui-text-field label="本轮专注（分钟）" aria-label={`${item.title}的本轮专注分钟`} type="number" min={5} max={180} variant="outlined" value={draft.focusMinutes} onInput={event=>updateDraft(item,{focusMinutes:fieldValue(event)})}/></details>}
        </>}
      </article>
    })
  }
  return <section className="today-page" aria-label="今日推进">
    <header className="page-head">
      <div className="title-block"><p className="eyebrow">{new Date().toLocaleDateString('zh-CN',{timeZone:timezone,month:'long',day:'numeric',weekday:'long'})}</p><h1>今日推进</h1>{currentGoal&&<p className="today-direction"><span>当前方向</span>{currentGoal.title}</p>}</div>
      <div className="head-side">
        {total>0&&<p className="head-stat"><strong>{done}<small>/{total}</small></strong><span>今日已推进</span></p>}
      </div>
    </header>
    {ownerChanged&&<p role="status" className="meta">{SESSION_OWNER_CHANGED} 原账号的草稿仍保留。</p>}
    <p className="today-intro">通常每天三项具体小步骤，超出的顺延。暂时不足时，会说明还缺什么。</p>
    {pendingGeneration&&<div className="empty-state"><p role="status">{generationJob.current?.status==='succeeded'?'安排作业已完成，当前任务还需读取确认。':'上次安排的结果尚未确认，将沿用原请求或作业继续核对。'}</p><mdui-button variant="outlined" disabled={Boolean(busy)||ownerChanged} loading={busy==='plan'} onClick={()=>generate()}>核对上次安排</mdui-button></div>}
    {loadError&&<div role="alert" className="form-error"><p>今日任务暂时无法更新：{loadError}</p>{plan&&<p className="meta">下方保留上次成功读取的安排，请重新加载后再操作。</p>}{!pendingGeneration&&<mdui-button variant="text" disabled={loading||Boolean(busy)} onClick={()=>void load()}>重新加载今日任务</mdui-button>}</div>}
    {!sessionsAvailable&&plan&&!loading&&<p className="meta">专注记录暂时无法加载，重新加载今日任务后可继续计时。</p>}
    {session && <div className="focus-strip">
      <p className="focus-task-title"><span>正在推进</span><b>{focusTask?.taskID===session.task_id&&focusTask.title||'正在读取关联任务…'}</b></p>
      <div className="focus-info"><span className="pulse"/><b>{session.status==='paused'?'专注已暂停':'专注进行中'}</b><span className="focus-timer" aria-label="本轮已专注时间">{Math.floor(elapsed/60).toString().padStart(2,'0')}:{(elapsed%60).toString().padStart(2,'0')}</span><span>本轮目标 {session.target_minutes} 分钟</span></div>
      <mdui-text-field label="这轮实际推进了什么？" variant="outlined" value={sessionNote} disabled={busy==='finish'} onInput={event => setSessionNote(fieldValue(event))}/>
      <mdui-button variant="outlined" icon={session.status==='paused'?'play_arrow':'pause'} disabled={Boolean(busy)||ownerChanged} loading={busy==='pause'||busy==='resume'} onClick={pauseOrResume}>{session.status==='paused'?'继续专注':'暂停计时'}</mdui-button>
      <mdui-button variant="filled" disabled={Boolean(busy)||ownerChanged||!sessionNote.trim()} loading={busy==='finish'} onClick={finish}>结束并记录</mdui-button>
    </div>}
    {loading&&!plan ? <div className="loading-block"><mdui-linear-progress/><p className="empty">正在整理今日任务…</p></div> : !plan ? !loadError&&<div className="empty-state"><h2>还没有今日安排</h2><p>可先核对已有目标和下一步，再明确生成今日安排。</p>{onOpenChat&&<mdui-button variant="filled" icon="forum" onClick={onOpenChat}>和助手聊聊</mdui-button>}{!pendingGeneration&&<mdui-button variant="text" disabled={busy==='plan'} loading={busy==='plan'} onClick={()=>generate(false)}>重新获取今日安排</mdui-button>}</div> : <>
      {showAvailability&&<div className="empty-state"><h2>{availabilityTitle[availabilityStatus]||'今日安排待确认'}</h2><p>{availabilityReason}</p>{onOpenChat&&<mdui-button variant="text" icon="forum" onClick={onOpenChat}>和助手聊聊</mdui-button>}{canReplan&&!pendingGeneration&&<mdui-button variant="filled" icon="refresh" disabled={Boolean(busy)||Boolean(loadError)} loading={busy==='plan'} onClick={()=>generate(true)}>重新安排今天</mdui-button>}</div>}
    <div className="today-list">{renderItems(coreItems)}</div>
      {(supportItems.length>0||supportUnlocked||retainedLearning)&&<section className="today-support" aria-label="可选阅读与补充">
        <header><h2>可选阅读与补充</h2><p className="meta">可选补充阅读或学习，不计入核心三项。</p></header>
        {!planEditable&&<p className="meta">这份安排已关闭或尚未到执行日期，仅可查看记录。</p>}
        {supportItems.length>0&&<div className="today-list">{renderItems(supportItems,true)}</div>}
        {(supportUnlocked||retainedLearning)&&<details className="today-support-add"><summary>添加学习任务</summary><div className="form-stack">
          <mdui-text-field label="学习任务标题" aria-label="学习任务标题" variant="outlined" value={learningDraft.title} disabled={Boolean(busy)||ownerChanged} onInput={event=>setLearningDraft(current=>({...current,title:fieldValue(event)}))}/>
          <mdui-text-field label="预期收获" aria-label="学习任务的预期收获" variant="outlined" rows={2} value={learningDraft.commitment} disabled={Boolean(busy)||ownerChanged} onInput={event=>setLearningDraft(current=>({...current,commitment:fieldValue(event)}))}/>
          <mdui-text-field label="最小行动" aria-label="学习任务的最小行动" variant="outlined" rows={2} value={learningDraft.minimum_action} disabled={Boolean(busy)||ownerChanged} onInput={event=>setLearningDraft(current=>({...current,minimum_action:fieldValue(event)}))}/>
          <mdui-text-field label="建议用时（分钟）" aria-label="学习任务的建议用时" type="number" min={5} max={180} variant="outlined" value={learningDraft.minutes} disabled={Boolean(busy)||ownerChanged} onInput={event=>setLearningDraft(current=>({...current,minutes:fieldValue(event)}))}/>
          {!validLearningMinutes&&<p role="alert" className="form-error">建议用时须为 5–180 之间的整数分钟。</p>}
          {learningError&&<p role="alert" className="form-error">{learningError}</p>}
          {learningError&&learningAttempt.current.pending()&&<p role="status" className="meta">保存结果尚未确认。再次保存会先核对原来的学习任务，再处理当前草稿，避免重复添加。</p>}
          {learningConflict&&<section aria-label="核对已创建的学习任务"><p>原学习任务已经创建。请核对最新内容，再选择保留自己的改动或使用最新内容。</p><dl><dt>学习任务标题</dt><dd>{learningConflict.data.title}</dd><dt>预期收获</dt><dd>{learningConflict.data.commitment}</dd><dt>最小行动</dt><dd>{learningConflict.data.minimum_action}</dd><dt>建议用时</dt><dd>{learningConflict.data.target_minutes} 分钟</dd></dl><div className="actions"><mdui-button variant="outlined" disabled={Boolean(busy)} onClick={()=>reviewLearning(true)}>保留我的修改</mdui-button><mdui-button variant="text" disabled={Boolean(busy)} onClick={()=>reviewLearning(false)}>使用最新内容</mdui-button></div></section>}
          <mdui-button variant="outlined" disabled={Boolean(busy)||ownerChanged||Boolean(loadError)||Boolean(learningConflict)||!validLearningMinutes||!learningDraft.title.trim()||!learningDraft.commitment.trim()||!learningDraft.minimum_action.trim()} loading={busy==='add-learning'} onClick={addLearningItem}>保存学习任务</mdui-button>
        </div></details>}
      </section>}
      {removedItems.length>0&&<details className="today-removed"><summary>已移出今日安排 · {removedItems.length} 项</summary>{removedItems.map(item=><p key={item.id}>{item.title}</p>)}<p>当前不符合执行条件，历史记录仍保留。</p></details>}
      {unfinishedItems.length>0&&<details className="today-removed"><summary>今日未完成的安排 · {unfinishedItems.length} 项</summary>{unfinishedItems.map(item=><div key={item.id}><b>{item.title}</b><p>{item.commitment||item.minimum_action}</p></div>)}<p>保留当天记录，不作为当前可开始的任务。</p></details>}
    </>}
    <details className="workspace-disclosure" aria-label="接下来七天的排期">
      <summary>接下来几天<span>查看未来安排</span></summary>
      <p className="meta">剩余任务按顺序排入未来日期，通常每天三项；今日已有内容保留，新任务可以补入空位。</p>
      {scheduleError?<p role="alert">未来安排暂时无法加载：{scheduleError}</p>:schedule.filter(day=>day.plan.local_date!==plan?.local_date).map(day=><details key={day.plan.id} className="schedule-day">
        <summary>{day.plan.local_date} · {day.plan.status==='active'?`${day.items.filter(item=>item.kind==='core').length} 项`:day.plan.status==='cancelled'?'已取消':'已关闭'}</summary>
        {day.plan.status==='active'&&day.items.filter(item=>item.kind==='core').map(item=><div key={item.id}><b>{item.title}</b><p>{item.minimum_action} · {item.target_minutes} 分钟</p></div>)}
        {day.plan.status==='active'&&day.items.every(item=>item.kind!=='core')&&<p className="empty">暂无待推进任务</p>}
        {day.plan.status!=='active'&&<p className="meta">这天不安排任务，开放任务会在其他日期重新安排。</p>}
      </details>)}
    </details>
    {settings && <details className="workspace-disclosure">
      <summary>计划设置 · 每日 {settings.daily_budget_minutes} 分钟{settings.primary_goal_id ? ` · ${goals.find(goal => goal.id === settings.primary_goal_id)?.title || '主目标'}` : ''}</summary>
      <div className="form-stack">
        <mdui-select label="主要科研目标" variant="outlined" value={primaryGoal} disabled={Boolean(busy)} onChange={event => {settingsDirty.current=true;setPrimaryGoal(fieldValue(event))}}>
          <mdui-menu-item value="">自动选择最早的活跃目标</mdui-menu-item>
          {configuredUnavailable&&<mdui-menu-item value={primaryGoal} disabled>{fixedGoal?.title||'原固定目标'} · 暂不可用（保留选择）</mdui-menu-item>}
          {goals.map(goal => <mdui-menu-item key={goal.id} value={goal.id}>{goal.title}</mdui-menu-item>)}
        </mdui-select>
        <mdui-text-field label="每日专注预算（分钟）" type="number" min={30} max={480} step={30} variant="outlined" value={budget} disabled={Boolean(busy)} onInput={event => {settingsDirty.current=true;setBudget(fieldValue(event))}}/>
        {settings?.primary_goal_mode==='automatic'&&currentGoal&&<p className="meta">自动选择当前方向：{currentGoal.title}</p>}
        {settings?.primary_goal_mode==='fixed'&&configuredGoal(settings)!==settings.primary_goal_id&&<p className="meta">原固定目标暂不可用，当前按{currentGoal?`「${currentGoal.title}」`:'其他活跃目标'}安排；固定选择仍保留。</p>}
        {settingsConflict&&<div className="settings-conflict" role="alert"><p>计划设置已在另一处更新。你的草稿已保留，请核对后选择继续使用草稿或采用最新设置。</p><div className="actions"><mdui-button variant="outlined" disabled={Boolean(busy)} onClick={()=>{const latest=latestSettings.current;if(latest){settingsEditETag.current=latest.etag;setSettingsETag(latest.etag);setSettingsConflict(false)}}}>继续使用草稿</mdui-button><mdui-button variant="text" disabled={Boolean(busy)} onClick={()=>{const latest=latestSettings.current;if(latest)applySettings(latest.data,latest.etag)}}>使用最新设置</mdui-button></div></div>}
        <p className="meta">每天最多三项，专注预算与最小行动分别记录。已有推进的今日承诺保留，未来随进展更新。</p>
        <div className="actions"><mdui-button variant="outlined" onClick={saveSettings} loading={busy==='settings'} disabled={Boolean(busy) || ownerChanged || settingsConflict || !settingsETag || !Number.isInteger(Number(budget)) || Number(budget)<30 || Number(budget)>480}>保存设置</mdui-button>{canReplan&&!showAvailability&&!pendingGeneration&&<mdui-button variant="text" icon="refresh" disabled={Boolean(busy)||Boolean(loadError)} loading={busy==='plan'} onClick={()=>generate(true)}>重新安排今天</mdui-button>}</div>
      </div>
    </details>}
  </section>
}

function etag(kind: string, id: string, revision: number) { return `"${kind}_${id}_rev_${revision}"` }
function errorText(error: unknown) { return error instanceof Error ? error.message : '操作失败，请稍后重试' }
