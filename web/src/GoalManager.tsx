import { useEffect, useRef, useState } from 'react'
import { ApiError, idem, request } from './api'
import { createCreationAttempt } from './creation-attempt'
import { SESSION_OWNER_CHANGED } from './session-owner'
import { ProposalSummary } from './agent/tools/ProposalApproval'
import { GoalMapView } from './Lens'
import { fieldValue, useMduiEvent } from './mdui-react'
import { waitJob } from './jobs'
import type { Goal, Job, Proposal, Task, TaskTree, TaskTreeNode } from './types'

const GOAL_STATUS: Record<string,string> = { draft:'待启动', active:'进行中', paused:'已暂停', completed:'已完成', abandoned:'已放弃', archived:'已归档' }
const TASK_STATUS: Record<string,string> = { ready:'待开始', in_progress:'进行中', blocked:'受阻', paused:'已暂停', completed:'已完成', cancelled:'已取消', superseded:'已替换' }
const TASK_TYPES: Record<string,string> = { milestone:'里程碑', task:'任务', action:'行动' }
const emptyGoal = { title:'', success_criteria:'', description:'', target_date:'' }
const emptyTask = { title:'', success_criteria:'', minimum_action:'', description:'', parent_id:'', depends_on:[] as string[], type:'task', estimate_minutes:'40', priority:'70' }
type Editor = ({ kind:'goal'; goal?:Goal } | { kind:'task'; task?:Task; goalId:string; goalTitle:string }) & {ownerId?:string}
type DeleteTarget = ({kind:'goal'; value:Goal} | {kind:'task'; value:Task}) & {ownerId?:string}
type Props = { userId?:string; onNotice:(message:string)=>void; refreshVersion?:number; onBusinessChanged?:()=>void; onOpenChat?:(goal?:Goal)=>void }

function flatten(nodes: TaskTreeNode[]): Task[] { return nodes.flatMap(node=>[node.task,...flatten(node.children)]) }
function canParent(task:Task){return !['completed','cancelled','superseded'].includes(task.status)}
function descendantIds(taskId: string, tasks: Task[]): Set<string> {
  const found = new Set<string>([taskId])
  for (let changed=true; changed;) {
    changed=false
    for (const task of tasks) if(task.parent_id&&found.has(task.parent_id)&&!found.has(task.id)){found.add(task.id);changed=true}
  }
  return found
}
function etag(kind:string, value:{id:string;revision:number}) { return `"${kind}_${value.id}_rev_${value.revision}"` }
function dependencyValues(event:unknown):string[]{
  const value=(event as {target?:{value?:unknown}}).target?.value
  return Array.isArray(value)?value.filter((id):id is string=>typeof id==='string'):typeof value==='string'&&value?[value]:[]
}
function sameDependencies(left:unknown,right:unknown){
  return JSON.stringify(Array.isArray(left)?[...left].sort():[])===JSON.stringify(Array.isArray(right)?[...right].sort():[])
}
function goalFields(goal:Goal){return {title:goal.title,success_criteria:goal.success_criteria,description:goal.description,target_date:goal.target_date||''}}
function taskFields(task:Task){return {title:task.title,success_criteria:task.success_criteria,minimum_action:task.minimum_action,description:task.description,parent_id:task.parent_id||'',depends_on:[...task.depends_on??[]],type:task.type,estimate_minutes:String(task.estimate_minutes),priority:String(task.priority)}}
function canonicalDraftValue(key:string,value:unknown){
  if((key==='priority'||key==='estimate_minutes')&&String(value).trim()!==''&&Number.isInteger(Number(value)))return Number(value)
  return typeof value==='string'?value.trim():value
}
function equalDraftValue(left:unknown,right:unknown,key=''){return Array.isArray(left)||Array.isArray(right)?sameDependencies(left,right):canonicalDraftValue(key,left)===canonicalDraftValue(key,right)}
function mergeDraft<T extends Record<string,unknown>>(latest:T,draft:T,original:T):T {
  const result={...latest}
  for(const key of Object.keys(draft) as (keyof T)[])if(!equalDraftValue(draft[key],original[key],String(key)))result[key]=draft[key]
  return result
}
function errorText(error:unknown) {
  if(error instanceof TypeError)return '连接中断，操作结果暂未确认。草稿已保留，可重试核对原请求。'
  if(error instanceof ApiError&&error.status===412)return '内容已在另一处更新，请查看最新内容后重试。'
  if(error instanceof ApiError&&error.status===409)return '当前状态暂不能执行。若任务正在专注，请先结束专注记录。'
  return error instanceof Error?error.message:'操作失败，请稍后重试'
}

export function GoalManager({ userId,onNotice, refreshVersion=0, onBusinessChanged, onOpenChat }: Props) {
  const [goals,setGoals]=useState<Goal[]>([])
  const [tasks,setTasks]=useState<Task[]>([])
  const [selectedId,setSelectedId]=useState('')
  const [tree,setTree]=useState<TaskTree|null>(null)
  const [treeETag,setTreeETag]=useState('')
  const [view,setView]=useState<'tree'|'map'>('tree')
  const [selectedTask,setSelectedTask]=useState('')
  const [collapsed,setCollapsed]=useState<Set<string>>(new Set())
  const [loading,setLoading]=useState(true)
  const [error,setError]=useState('')
  const [busy,setBusy]=useState('')
  const busyRef=useRef(false)
  const currentUserId=useRef(userId)
  currentUserId.current=userId
  const dataOwner=useRef(userId)
  const creationAttempts=useRef({goal:createCreationAttempt<Goal>(),task:createCreationAttempt<Task>()})
  const loadSequence=useRef(0)
  const selectedRef=useRef('')
  const [editor,setEditor]=useState<Editor|null>(null)
  const [goalDraft,setGoalDraft]=useState(emptyGoal)
  const [taskDraft,setTaskDraft]=useState(emptyTask)
  const [deleteTarget,setDeleteTarget]=useState<DeleteTarget|null>(null)
  const [outcomeDrafts,setOutcomeDrafts]=useState<Record<string,{summary:string;revision:number}>>({})
  const [showTrash,setShowTrash]=useState(false)
  const [trashGoals,setTrashGoals]=useState<Goal[]>([])
  const [trashTasks,setTrashTasks]=useState<Task[]>([])
  const [revisionInstruction,setRevisionInstruction]=useState('')
  const [modelAvailable,setModelAvailable]=useState<boolean|null>(null)
  const editorRef=useRef<HTMLElement>(null)
  const deleteRef=useRef<HTMLElement>(null)
  useMduiEvent(editorRef,'close',event=>{if(busyRef.current)event.preventDefault();else setEditor(null)})
  useMduiEvent(deleteRef,'close',event=>{if(busyRef.current)event.preventDefault();else setDeleteTarget(null)})
  const selected=goals.find(goal=>goal.id===selectedId)??null
  const latestEditGoal=editor?.kind==='goal'&&editor.goal?goals.find(goal=>goal.id===editor.goal!.id):undefined
  const latestEditTask=editor?.kind==='task'&&editor.task?tasks.find(task=>task.id===editor.task!.id):undefined
  const editorChanged=editor?.kind==='goal'?Boolean(editor.goal&&latestEditGoal&&editor.goal.revision!==latestEditGoal.revision):Boolean(editor?.task&&latestEditTask&&editor.task.revision!==latestEditTask.revision)
  const editorOwnerChanged=Boolean(editor?.ownerId&&editor.ownerId!==userId)
  const visibleTree=tree?.goal_id===selectedId?tree:null
  const treeTasks=visibleTree?flatten(visibleTree.nodes):[]
  const editorTasks=editor?.kind==='task'?tasks.filter(task=>task.goal_id===editor.goalId&&!task.deleted_at):treeTasks
  const editorGoalMissing=editor?.kind==='task'&&!loading&&!goals.some(goal=>goal.id===editor.goalId)
  const validEstimate=Number.isInteger(Number(taskDraft.estimate_minutes))&&Number(taskDraft.estimate_minutes)>=1&&Number(taskDraft.estimate_minutes)<=1440
  const validPriority=taskDraft.priority.trim()!==''&&Number.isInteger(Number(taskDraft.priority))&&Number(taskDraft.priority)>=0&&Number(taskDraft.priority)<=100
  const taskNumberError=!validEstimate?'预计总用时须为 1–1440 之间的整数分钟。':!validPriority?'优先级须为 0–100 之间的整数。':''

  function ownedRequest<T>(path:string,init:RequestInit={},ownerId=dataOwner.current){
    if(ownerId&&currentUserId.current!==ownerId)throw new Error(SESSION_OWNER_CHANGED)
    return request<T>(path,init,ownerId)
  }

  async function load(preferredId=selectedRef.current) {
    if(userId&&currentUserId.current!==userId)return
    const sequence=++loadSequence.current
    setLoading(true)
    try {
      const [goalResponse,taskResponse]=await Promise.all([request<{items:Goal[]}>('/goals',{},userId),request<{items:Task[]}>('/tasks',{},userId)])
      if(sequence!==loadSequence.current)return
      dataOwner.current=userId;setGoals(goalResponse.data.items);setTasks(taskResponse.data.items)
      const next=goalResponse.data.items.find(goal=>goal.id===preferredId)??goalResponse.data.items[0]
      selectedRef.current=next?.id??''
      setSelectedId(next?.id??'')
      if(next){
        setTree(current=>current?.goal_id===next.id?current:null)
        const response=await request<TaskTree>(`/goals/${next.id}/task-tree`,{},userId)
        if(sequence!==loadSequence.current)return
        if(response.data.goal_id!==next.id)throw new Error('任务树与当前目标不匹配，请重新加载。')
        setTree(response.data);setTreeETag(response.etag||`"tree_${next.id}_rev_${response.data.revision}"`)
      }else setTree(null)
      setError('')
    }catch(cause){if(sequence===loadSequence.current)setError(errorText(cause))}
    finally{if(sequence===loadSequence.current)setLoading(false)}
  }
  async function loadTrash() {
    try {
      const [goalResponse,taskResponse]=await Promise.all([request<{items:Goal[]}>('/goals?deleted=true',{},userId),request<{items:Task[]}>('/tasks?deleted=true',{},userId)])
      if(userId&&currentUserId.current!==userId)return
      setTrashGoals(goalResponse.data.items);setTrashTasks(taskResponse.data.items)
    }catch(cause){setError(errorText(cause))}
  }
  useEffect(()=>{void load();if(showTrash)void loadTrash()},[refreshVersion,userId])
  useEffect(()=>{if(showTrash)void loadTrash()},[showTrash])
  useEffect(()=>{void request<{configured:boolean}>('/agent/availability',{},userId).then(({data})=>setModelAvailable(data.configured)).catch(()=>setModelAvailable(null))},[refreshVersion,userId])

  async function mutate(key:string, action:()=>Promise<void>, message:string) {
    if(busyRef.current)return
    busyRef.current=true;setBusy(key);setError('')
    try {await action();onNotice(message);onBusinessChanged?.();await load();if(showTrash)await loadTrash()}
    catch(cause){const text=errorText(cause);onNotice(text);if(cause instanceof ApiError&&cause.status===412)await load();setError(text)}
    finally{busyRef.current=false;setBusy('')}
  }
  function openGoal(goal?:Goal){if(busyRef.current)return;const ownerId=goal?dataOwner.current:userId;creationAttempts.current.goal=createCreationAttempt<Goal>(ownerId,()=>currentUserId.current);setGoalDraft(goal?goalFields(goal):{...emptyGoal});setEditor({kind:'goal',goal,ownerId})}
  function openTask(task?:Task,parent?:Task){
    if(busyRef.current||!visibleTree||!selected||parent&&!canParent(parent))return
    const ownerId=dataOwner.current
    creationAttempts.current.task=createCreationAttempt<Task>(ownerId,()=>currentUserId.current)
    setTaskDraft(task?taskFields(task):{...emptyTask,depends_on:[],parent_id:parent?.id||'',type:parent?(parent.type==='milestone'?'task':'action'):'task'})
    setEditor({kind:'task',task,goalId:selected.id,goalTitle:selected.title,ownerId})
  }
  function reviewLatestEditor(keepChanges:boolean){
    if(editorOwnerChanged){onNotice(SESSION_OWNER_CHANGED);return}
    if(editor?.kind==='goal'&&editor.goal&&latestEditGoal){
      const latest=goalFields(latestEditGoal)
      setGoalDraft(keepChanges?mergeDraft(latest,goalDraft,goalFields(editor.goal)):latest)
      setEditor({kind:'goal',goal:latestEditGoal,ownerId:editor.ownerId})
    }else if(editor?.kind==='task'&&editor.task&&latestEditTask){
      const latest=taskFields(latestEditTask)
      setTaskDraft(keepChanges?mergeDraft(latest,taskDraft,taskFields(editor.task)):latest)
      setEditor({...editor,task:latestEditTask})
    }
    setError('')
  }
  async function saveEditor() {
    if(!editor)return
    if(editorOwnerChanged){onNotice(SESSION_OWNER_CHANGED);return}
    if(editor.kind==='goal'){
      if(!goalDraft.title.trim()||!goalDraft.success_criteria.trim())return
      const original=editor.goal
      // The draft belongs to the version the user read when opening it. A
      // background refresh must not silently authorize overwriting newer work.
      const existing=original
      await mutate('editor',async()=>{
        const payload={...goalDraft,title:goalDraft.title.trim(),success_criteria:goalDraft.success_criteria.trim(),target_date:goalDraft.target_date||null}
        const originalFields=original?{title:original.title,success_criteria:original.success_criteria,description:original.description,target_date:original.target_date||null}:null
        const updates=originalFields?Object.fromEntries(Object.entries(payload).filter(([key,value])=>!equalDraftValue(value,originalFields[key as keyof typeof originalFields],key))):payload
        let response:{data:Goal;etag:string|null}
        if(existing)response=await ownedRequest<Goal>(`/goals/${existing.id}`,{method:'PATCH',headers:{'If-Match':etag('goal',existing)},body:JSON.stringify(updates)},editor.ownerId)
        else{
          const recovered=await creationAttempts.current.goal.recover('/goals',payload)
          // Resolve an uncertain creation before updating the current draft.
          // If that PATCH conflicts, the editor now owns this exact resource.
          setEditor({kind:'goal',goal:recovered.data,ownerId:editor.ownerId});creationAttempts.current.goal.reset()
          const createdFields={...goalFields(recovered.data),target_date:recovered.data.target_date||null}
          const changes=Object.fromEntries(Object.entries(payload).filter(([key,value])=>!equalDraftValue(value,createdFields[key as keyof typeof createdFields],key)))
          response=Object.keys(changes).length?await ownedRequest<Goal>(`/goals/${recovered.data.id}`,{method:'PATCH',headers:{'If-Match':recovered.etag||etag('goal',recovered.data)},body:JSON.stringify(changes)},editor.ownerId):recovered
        }
        setSelectedId(response.data.id);setEditor(null)
        // A newly created goal must be the one opened after the shared refresh.
        await load(response.data.id)
      },existing?'目标已更新':'目标已创建')
    }else{
      if(editorGoalMissing||!taskDraft.title.trim()||!taskDraft.success_criteria.trim()||!taskDraft.minimum_action.trim())return
      const estimate=Number(taskDraft.estimate_minutes),priority=Number(taskDraft.priority)
      if(!validEstimate||!validPriority)return
      const original=editor.task
      const existing=original
      await mutate('editor',async()=>{
        const payload={...taskDraft,title:taskDraft.title.trim(),parent_id:taskDraft.parent_id||null,estimate_minutes:estimate,priority,position:existing?.position??editorTasks.filter(task=>(task.parent_id||'')===taskDraft.parent_id).length}
        const originalFields=original?{...original,parent_id:original.parent_id||null,position:original.position??0}:null
        const updates=originalFields?Object.fromEntries(Object.entries(payload).filter(([key,value])=>!equalDraftValue(value,originalFields[key as keyof typeof originalFields],key))):{...payload,goal_id:editor.goalId}
        if(existing)await ownedRequest<Task>(`/tasks/${existing.id}`,{method:'PATCH',headers:{'If-Match':etag('task',existing)},body:JSON.stringify(updates)},editor.ownerId)
        else{
          const recovered=await creationAttempts.current.task.recover('/tasks',{...payload,goal_id:editor.goalId})
          setEditor({...editor,task:recovered.data});creationAttempts.current.task.reset()
          // Position was assigned by the original creation; a refetch's row
          // count is not a user edit and must not move the recovered task.
          const desired={...payload,position:recovered.data.position}
          const createdFields={...recovered.data,parent_id:recovered.data.parent_id||null}
          const changes=Object.fromEntries(Object.entries(desired).filter(([key,value])=>!equalDraftValue(value,createdFields[key as keyof typeof createdFields],key)))
          if(Object.keys(changes).length)await ownedRequest<Task>(`/tasks/${recovered.data.id}`,{method:'PATCH',headers:{'If-Match':recovered.etag||etag('task',recovered.data)},body:JSON.stringify(changes)},editor.ownerId)
        }
        setEditor(null)
      },existing?'任务已更新':'任务已添加')
    }
  }
  async function setGoalStatus(status:string){if(!selected)return;const goal=selected;await mutate('goal-status',async()=>{await ownedRequest(`/goals/${goal.id}`,{method:'PATCH',headers:{'If-Match':etag('goal',goal)},body:JSON.stringify({status})})},'目标状态已更新')}
  async function setTaskStatus(task:Task,status:string){await mutate(task.id,async()=>{await ownedRequest(`/tasks/${task.id}`,{method:'PATCH',headers:{'If-Match':etag('task',task)},body:JSON.stringify({status})})},'任务状态已更新')}
  async function unblockTask(task:Task){await mutate(task.id,async()=>{await ownedRequest(`/tasks/${task.id}`,{method:'PATCH',headers:{'If-Match':etag('task',task)},body:JSON.stringify({status:'ready',blocked_reason:''})})},'已解除受阻；未来安排将按最新前置成果更新')}
  function outcomeKey(task:Task){return `${task.id}:${task.status==='completed'?'reopen':'complete'}`}
  function outcomeOf(task:Task){return outcomeDrafts[outcomeKey(task)]??{summary:'',revision:task.revision}}
  function updateOutcome(task:Task,summary:string){
    const key=outcomeKey(task)
    setOutcomeDrafts(current=>({...current,[key]:{summary,revision:current[key]?.summary.trim()?current[key].revision:task.revision}}))
  }
  async function completeTask(task:Task,reopen=false){
    const key=outcomeKey(task),draft=outcomeOf(task)
    if(!draft.summary.trim()||draft.revision!==task.revision)return
    await mutate(task.id,async()=>{
      await ownedRequest(`/tasks/${task.id}/${reopen?'reopenings':'completions'}`,{method:'POST',headers:{'If-Match':etag('task',{id:task.id,revision:draft.revision}),'Idempotency-Key':idem()},body:JSON.stringify({type:reopen?'step':'result',summary:draft.summary.trim()})})
      setOutcomeDrafts(current=>{const next={...current};delete next[key];return next})
    },reopen?'任务已重新打开':'任务最终成果已记录')
  }
  async function remove(){if(!deleteTarget)return;const target=deleteTarget;await mutate('delete',async()=>{await ownedRequest(`/${target.kind==='goal'?'goals':'tasks'}/${target.value.id}`,{method:'DELETE',headers:{'If-Match':etag(target.kind,target.value),'Idempotency-Key':idem()}},target.ownerId);setDeleteTarget(null);setSelectedTask('')},'已移入回收站，可随时恢复')}
  async function restore(kind:'goal'|'task',value:Goal|Task){await mutate(value.id,async()=>{await ownedRequest(`/${kind==='goal'?'goals':'tasks'}/${value.id}/restorations`,{method:'POST',headers:{'If-Match':etag(kind,value),'Idempotency-Key':idem()}})},'已恢复，未来安排将重新计算')}
  async function askAgent(revision:boolean){if(!selected)return;const goal=selected;await mutate('agent',async()=>{const {data}=await ownedRequest<Job>(`/goals/${goal.id}/task-tree/${revision?'revision-jobs':'generation-jobs'}`,{method:'POST',headers:{'Idempotency-Key':idem(),...(revision?{'If-Match':treeETag}:{})},body:JSON.stringify({instruction:revision?revisionInstruction:'拆解为三层以内的科研任务，每项有具体最小行动和可验证成果'})});const job=await waitJob(data.id);if(job.status!=='succeeded')throw new Error(job.error_message||'任务拆解失败');setRevisionInstruction('')},'拆解建议已准备好，请审阅后应用')}
  async function decide(proposal:Proposal,apply:boolean){if(!selected)return;const goal=selected;await mutate(proposal.id,async()=>{await ownedRequest(`/goals/${goal.id}/task-tree/proposals/${proposal.id}/${apply?'application':'rejection'}`,{method:apply?'POST':'PUT',headers:apply?{'If-Match':treeETag,'Idempotency-Key':idem()}:{'If-Match':etag('proposal',proposal)}})},apply?'任务树已更新':'已拒绝这份建议')}
  function toggle(taskId:string){setCollapsed(current=>{const next=new Set(current);if(next.has(taskId))next.delete(taskId);else next.add(taskId);return next})}
  function selectGoal(id:string){
    if(busyRef.current)return
    selectedRef.current=id;setSelectedId(id);setTree(null);setTreeETag('');setSelectedTask('');setOutcomeDrafts({});setRevisionInstruction('');setView('tree')
    void load(id)
  }

  function renderNodes(nodes:TaskTreeNode[]):React.ReactNode {
    return <ul className="goal-tree">{nodes.map(({task,children})=>{const outcome=outcomeOf(task),outcomeChanged=outcome.revision!==task.revision;return <li key={task.id}>
      <div className={`goal-task-row${selectedTask===task.id?' selected':''}${task.status==='completed'?' done':''}`}>
        {children.length?<button type="button" className="tree-toggle" onClick={()=>toggle(task.id)} aria-label={`${collapsed.has(task.id)?'展开':'收起'}${task.title}`} aria-expanded={!collapsed.has(task.id)}><mdui-icon name={collapsed.has(task.id)?'chevron_right':'expand_more'}/></button>:<span className="tree-leaf"/>}
        <button type="button" className="goal-task-open" onClick={()=>setSelectedTask(selectedTask===task.id?'':task.id)} aria-expanded={selectedTask===task.id}>
          <span><b>{task.title}</b><small>{TASK_TYPES[task.type]} · {TASK_STATUS[task.status]||task.status}</small></span>
          <mdui-icon name={selectedTask===task.id?'expand_less':'more_horiz'}/>
        </button>
      </div>
      {selectedTask===task.id&&<div className="goal-task-detail">
        <p>{task.success_criteria}</p><p className="task-next"><span>下一步</span>{task.minimum_action}</p><p className="meta">预计总工作量：{task.estimate_minutes} 分钟</p>
        {Boolean(task.depends_on?.length)&&<div><b>前置成果</b><ul>{task.depends_on!.map(id=>{const dependency=tasks.find(value=>value.id===id&&value.goal_id===task.goal_id&&!value.deleted_at);return <li key={id}>{dependency?`${dependency.title} · ${TASK_STATUS[dependency.status]||dependency.status}`:'前置任务不可用'}</li>})}</ul>{task.depends_on!.some(id=>tasks.find(value=>value.id===id&&!value.deleted_at)?.status!=='completed')&&<p className="meta">前置任务尚未最终完成；今日的小步推进不代表前置成果已经满足。</p>}</div>}
        {task.status==='blocked'&&task.blocked_reason&&<p className="form-error">阻碍：{task.blocked_reason}</p>}
        <div className="actions">
          <mdui-button variant="text" icon="edit" onClick={()=>openTask(task)} disabled={Boolean(busy)}>编辑与移动</mdui-button>
          {task.status==='blocked'&&<mdui-button variant="outlined" onClick={()=>unblockTask(task)} disabled={Boolean(busy)}>解除受阻</mdui-button>}
          {canParent(task)&&<mdui-button variant="text" icon="add" onClick={()=>openTask(undefined,task)} disabled={Boolean(busy)}>添加子任务</mdui-button>}
          {['cancelled','superseded'].includes(task.status)?<mdui-button variant="text" onClick={()=>setTaskStatus(task,'ready')} disabled={Boolean(busy)}>重新启用任务</mdui-button>:task.status!=='completed'&&<mdui-button variant="text" onClick={()=>setTaskStatus(task,task.status==='paused'?'ready':'paused')} disabled={Boolean(busy)}>{task.status==='paused'?'恢复任务':'暂停任务'}</mdui-button>}
          <mdui-button variant="text" icon="delete_outline" onClick={()=>setDeleteTarget({kind:'task',value:task,ownerId:dataOwner.current})} disabled={Boolean(busy)}>移入回收站</mdui-button>
        </div>
        {!['cancelled','superseded'].includes(task.status)&&<details className="task-result"><summary>{task.status==='completed'?'重新打开任务':'记录最终成果'}</summary>
          <p className="meta">今日推进与最终完成分别记录。这里用于{task.status==='completed'?'说明为什么需要继续做':'确认整个任务已有可验证成果'}。</p>
          {outcomeChanged&&<><p role="status" className="meta">任务已更新，摘要已保留。请核对上方的任务和验收标准，再继续记录。</p><mdui-button variant="text" disabled={Boolean(busy)} onClick={()=>setOutcomeDrafts(current=>({...current,[outcomeKey(task)]:{...outcome,revision:task.revision}}))}>已核对最新任务</mdui-button></>}
          <mdui-text-field disabled={Boolean(busy)} label={task.status==='completed'?'重新打开的原因':'最终成果摘要'} variant="outlined" rows={2} value={outcome.summary} onInput={event=>updateOutcome(task,fieldValue(event))}/>
          <mdui-button variant="outlined" disabled={Boolean(busy)||!outcome.summary.trim()||outcomeChanged} onClick={()=>completeTask(task,task.status==='completed')}>{task.status==='completed'?'重新打开':'确认任务完成'}</mdui-button>
        </details>}
      </div>}
      {children.length>0&&!collapsed.has(task.id)&&renderNodes(children)}
    </li>})}</ul>
  }

  const excludedParents=editor?.kind==='task'&&editor.task?descendantIds(editor.task.id,editorTasks):new Set<string>()
  const affected=deleteTarget?.kind==='goal'?tasks.filter(task=>task.goal_id===deleteTarget.value.id):deleteTarget?.kind==='task'?tasks.filter(task=>descendantIds(deleteTarget.value.id,tasks).has(task.id)):[]
  const latestEditorFields=latestEditGoal?goalFields(latestEditGoal):latestEditTask?taskFields(latestEditTask):null
  const originalEditorFields=editor?.kind==='goal'&&editor.goal?goalFields(editor.goal):editor?.kind==='task'&&editor.task?taskFields(editor.task):null
  const labels:Record<string,string>={title:'标题',success_criteria:'完成标准',minimum_action:'最小行动',description:'说明',parent_id:'上级任务',depends_on:'前置成果',type:'类型',estimate_minutes:'预计总用时（分钟）',priority:'优先级',target_date:'目标日期'}
  function editorFieldText(key:string,value:unknown){
    if(key==='parent_id')return value?tasks.find(task=>task.id===value)?.title||'上级任务不可用':'直接属于目标'
    if(key==='depends_on')return (value as string[]).map(id=>tasks.find(task=>task.id===id)?.title||'前置任务不可用').join('、')||'无'
    if(key==='type')return TASK_TYPES[String(value)]||String(value)
    return String(value||'未填写')
  }
  return <section className="goals-page">
    <header className="page-head"><div className="title-block"><p className="eyebrow">长期方向 · 每日小步</p><h1>目标与任务</h1></div><div className="head-side"><mdui-button variant="text" icon="delete_outline" onClick={()=>setShowTrash(value=>!value)}>{showTrash?'返回目标':'回收站'}</mdui-button><mdui-button variant="outlined" icon="add" onClick={()=>openGoal()} disabled={Boolean(busy)}>新建目标</mdui-button></div></header>
    {error&&<p role="alert" className="form-error">{error}</p>}
    {showTrash?<section className="recycle-bin"><h2>回收站</h2><p className="meta">恢复后保留原有进度；仍在回收站的上级目标或任务需要先恢复。</p>
      {trashGoals.map(goal=><div className="recycle-row" key={goal.id}><div><b>{goal.title}</b><small>目标 · 包含所属任务</small></div><mdui-button variant="outlined" onClick={()=>restore('goal',goal)} disabled={Boolean(busy)}>恢复目标</mdui-button></div>)}
      {trashTasks.filter(task=>!trashGoals.some(goal=>goal.id===task.goal_id)).map(task=><div className="recycle-row" key={task.id}><div><b>{task.title}</b><small>{TASK_TYPES[task.type]}</small></div><mdui-button variant="text" onClick={()=>restore('task',task)} disabled={Boolean(busy)}>恢复任务</mdui-button></div>)}
      {!trashGoals.length&&!trashTasks.length&&<p className="empty">回收站是空的。</p>}
    </section>:<div className="goal-layout">
      <nav className="goal-index" aria-label="长期目标">{goals.map(goal=><button key={goal.id} type="button" className={selectedId===goal.id?'active':''} disabled={Boolean(busy)} onClick={()=>selectGoal(goal.id)}><b>{goal.title}</b><small>{GOAL_STATUS[goal.status]||goal.status}</small></button>)}{!goals.length&&!loading&&<p className="empty">从一个重要目标开始。</p>}</nav>
      <div className="tree-panel">{selected?<>
        <header className="tree-head"><div><p className="meta">{GOAL_STATUS[selected.status]||selected.status}{selected.target_date?` · ${selected.target_date}`:''}</p><h2>{selected.title}</h2><p>{selected.success_criteria}</p></div><mdui-button variant="text" icon="edit" onClick={()=>openGoal(selected)} disabled={Boolean(busy)}>编辑目标</mdui-button></header>
        <div className="goal-toolbar"><div className="actions"><mdui-button variant={view==='tree'?'tonal':'text'} onClick={()=>setView('tree')}>任务树</mdui-button><mdui-button variant={view==='map'?'tonal':'text'} onClick={()=>setView('map')}>目标地图</mdui-button></div><mdui-button variant="outlined" icon="add" onClick={()=>openTask()} disabled={Boolean(busy)||!visibleTree}>添加任务</mdui-button></div>
        {visibleTree?.proposals.map(proposal=><TreeProposal key={proposal.id} proposal={proposal} taskTitles={new Map([...tasks,...trashTasks].map(task=>[task.id,task.title]))} busy={Boolean(busy)} demo={modelAvailable===false} onDecide={apply=>decide(proposal,apply)}/>)}
        {loading&&!visibleTree?<p className="empty">正在读取任务树…</p>:view==='map'?<GoalMapView goal={selected} onNotice={onNotice} onOpenTask={id=>{setSelectedTask(id);setView('tree')}}/>:visibleTree?.nodes.length?renderNodes(visibleTree.nodes):<div className="empty-state"><h3>找到一个能开始的小步骤</h3><p>在对话中描述目标，或手动添加任务。</p>{onOpenChat&&<mdui-button variant="filled" icon="forum" onClick={()=>{if(dataOwner.current&&dataOwner.current!==currentUserId.current){onNotice(SESSION_OWNER_CHANGED);return}onOpenChat(selected)}}>在对话中拆解</mdui-button>}</div>}
        <details className="workspace-disclosure"><summary>更多目标操作</summary><div className="actions">
          <mdui-button variant="text" disabled={Boolean(busy)} onClick={()=>setGoalStatus(selected.status==='active'?'paused':'active')}>{selected.status==='active'?'暂停目标':'恢复目标'}</mdui-button>
          {selected.status==='active'&&<mdui-button variant="text" disabled={Boolean(busy)} onClick={()=>setGoalStatus('completed')}>标记目标完成</mdui-button>}
          {['active','paused','completed','abandoned'].includes(selected.status)&&<mdui-button variant="text" disabled={Boolean(busy)} onClick={()=>setGoalStatus('archived')}>归档目标</mdui-button>}
          <mdui-button variant="text" icon="delete_outline" disabled={Boolean(busy)} onClick={()=>setDeleteTarget({kind:'goal',value:selected,ownerId:dataOwner.current})}>移入回收站</mdui-button>
        </div><div className="form-stack"><p className="meta">也可以直接对助手说怎样调整任务。</p>{modelAvailable===false&&<p className="model-notice">尚未连接真实模型，下面仅提供演示示例，不会理解具体研究内容。</p>}{modelAvailable===null&&<p className="meta">暂时无法确认模型连接，请稍后重试。</p>}<mdui-text-field disabled={Boolean(busy)} label="任务树调整建议" variant="outlined" value={revisionInstruction} onInput={event=>setRevisionInstruction(fieldValue(event))}/><div className="actions"><mdui-button variant="outlined" onClick={()=>askAgent(true)} disabled={Boolean(busy)||!revisionInstruction.trim()||modelAvailable===null}>{modelAvailable===false?'查看演示调整提案':'生成调整提案'}</mdui-button><mdui-button variant="text" onClick={()=>askAgent(false)} disabled={Boolean(busy)||modelAvailable===null}>{modelAvailable===false?'查看演示拆解提案':'生成拆解提案'}</mdui-button></div></div></details>
      </>:!loading&&<div className="empty-state"><h2>让研究有一个清晰的方向</h2><p>先告诉助手最想推进的研究目标，任务树会从这里生长。</p>{onOpenChat&&<mdui-button variant="filled" icon="forum" onClick={()=>onOpenChat()}>和助手聊聊</mdui-button>}</div>}</div>
    </div>}

    {editor&&<mdui-dialog ref={editorRef} open close-on-esc headline={editor.kind==='goal'?(editor.goal?'编辑目标':'新建目标'):(editor.task?'编辑任务':'添加任务')} role="dialog" aria-label={editor.kind==='goal'?'目标编辑':'任务编辑'} className="goal-editor">
      <div className="form-stack">{editorOwnerChanged&&<p role="status" className="meta">{SESSION_OWNER_CHANGED} 草稿仍保留在这里。</p>}{error&&<p role="alert" className="form-error">{error}</p>}{error&&(editor.kind==='goal'?!editor.goal&&creationAttempts.current.goal.pending():!editor.task&&creationAttempts.current.task.pending())&&<p role="status" className="meta">保存结果尚未确认。重试会先核对原来的新建结果，再应用当前草稿，避免重复创建。</p>}{editorChanged&&latestEditorFields&&originalEditorFields&&<section aria-label="核对最新修改">
        <p>内容已在另一处更新，你的草稿仍在。请核对最新修改，再选择保留自己的改动或载入最新内容。</p>
        <dl>{Object.entries(latestEditorFields).filter(([key,value])=>!equalDraftValue(value,originalEditorFields[key as keyof typeof originalEditorFields],key)).map(([key,value])=><div key={key}><dt>{labels[key]}</dt><dd>{editorFieldText(key,value)}</dd></div>)}</dl>
        <p className="meta">{latestEditTask?`任务当前状态：${TASK_STATUS[latestEditTask.status]||latestEditTask.status}`:latestEditGoal?`目标当前状态：${GOAL_STATUS[latestEditGoal.status]||latestEditGoal.status}`:''}。保留自己的改动时，其余内容采用最新版本。</p>
        <div className="actions"><mdui-button variant="outlined" disabled={Boolean(busy)||editorOwnerChanged} onClick={()=>reviewLatestEditor(true)}>保留我的修改</mdui-button><mdui-button variant="text" disabled={Boolean(busy)||editorOwnerChanged} onClick={()=>reviewLatestEditor(false)}>载入最新内容</mdui-button></div>
      </section>}{editor.kind==='goal'?<>
        <mdui-text-field disabled={Boolean(busy)} label="目标标题" variant="outlined" value={goalDraft.title} onInput={event=>setGoalDraft(d=>({...d,title:fieldValue(event)}))}/>
        <mdui-text-field disabled={Boolean(busy)} label="目标完成标准" variant="outlined" rows={2} value={goalDraft.success_criteria} onInput={event=>setGoalDraft(d=>({...d,success_criteria:fieldValue(event)}))}/>
        <mdui-text-field disabled={Boolean(busy)} label="目标日期" type="date" variant="outlined" value={goalDraft.target_date} onInput={event=>setGoalDraft(d=>({...d,target_date:fieldValue(event)}))}/>
        <mdui-text-field disabled={Boolean(busy)} label="背景与约束" variant="outlined" rows={2} value={goalDraft.description} onInput={event=>setGoalDraft(d=>({...d,description:fieldValue(event)}))}/>
      </>:<>
        <p className="meta">所属目标：{editor.goalTitle}</p>
        {editorGoalMissing&&<p role="alert" className="form-error">原目标已移入回收站或暂不可用，草稿已保留；恢复原目标后再保存。</p>}
        <mdui-text-field disabled={Boolean(busy)} label="任务标题" variant="outlined" value={taskDraft.title} onInput={event=>setTaskDraft(d=>({...d,title:fieldValue(event)}))}/>
        <div className="form-row"><mdui-select disabled={Boolean(busy)} label="类型" variant="outlined" value={taskDraft.type} onChange={event=>setTaskDraft(d=>({...d,type:fieldValue(event)}))}>{Object.entries(TASK_TYPES).map(([value,label])=><mdui-menu-item key={value} value={value}>{label}</mdui-menu-item>)}</mdui-select>
        <mdui-select disabled={Boolean(busy)} label="上级任务" variant="outlined" value={taskDraft.parent_id} onChange={event=>setTaskDraft(d=>({...d,parent_id:fieldValue(event)}))}><mdui-menu-item value="">直接属于目标</mdui-menu-item>{editorTasks.filter(task=>!excludedParents.has(task.id)&&(canParent(task)||task.id===editor.task?.parent_id)).map(task=><mdui-menu-item key={task.id} value={task.id} disabled={!canParent(task)}>{task.title}{!canParent(task)?` · ${TASK_STATUS[task.status]}（当前上级）`:''}</mdui-menu-item>)}</mdui-select></div>
        <mdui-select disabled={Boolean(busy)} label="前置任务（可多选）" variant="outlined" multiple value={taskDraft.depends_on} onChange={event=>setTaskDraft(d=>({...d,depends_on:dependencyValues(event)}))}>{editorTasks.filter(task=>task.id!==editor.task?.id&&(!['cancelled','superseded'].includes(task.status)||taskDraft.depends_on.includes(task.id))).map(task=><mdui-menu-item key={task.id} value={task.id}>{task.title} · {TASK_STATUS[task.status]||task.status}</mdui-menu-item>)}{taskDraft.depends_on.filter(id=>!editorTasks.some(task=>task.id===id)).map(id=><mdui-menu-item key={id} value={id}>前置任务不可用（保留原关系）</mdui-menu-item>)}</mdui-select>
        <p className="meta">仅在必须取得前置成果才能开始时设置依赖。通常的工作顺序用任务排列表示，准备工作可以并行。</p>
        <mdui-text-field disabled={Boolean(busy)} label="任务完成标准" variant="outlined" rows={2} value={taskDraft.success_criteria} onInput={event=>setTaskDraft(d=>({...d,success_criteria:fieldValue(event)}))}/>
        <mdui-text-field disabled={Boolean(busy)} label="最小行动" variant="outlined" rows={2} value={taskDraft.minimum_action} onInput={event=>setTaskDraft(d=>({...d,minimum_action:fieldValue(event)}))}/>
        <details><summary>总工作量、优先级与说明</summary><div className="form-stack"><div className="form-row"><mdui-text-field disabled={Boolean(busy)} label="预计总用时（分钟）" type="number" min={1} max={1440} value={taskDraft.estimate_minutes} onInput={event=>setTaskDraft(d=>({...d,estimate_minutes:fieldValue(event)}))}/><mdui-text-field disabled={Boolean(busy)} label="优先级（0–100）" type="number" min={0} max={100} value={taskDraft.priority} onInput={event=>setTaskDraft(d=>({...d,priority:fieldValue(event)}))}/></div><p className="meta">这里记录整个任务的预计总工作量；每日专注预算由今日安排单独记录。</p><mdui-text-field disabled={Boolean(busy)} label="任务说明" rows={2} value={taskDraft.description} onInput={event=>setTaskDraft(d=>({...d,description:fieldValue(event)}))}/></div></details>
      </>}{editor.kind==='task'&&taskNumberError&&<p role="alert" className="form-error">{taskNumberError}</p>}</div>
      <mdui-button slot="action" variant="text" onClick={()=>setEditor(null)} disabled={Boolean(busy)}>取消</mdui-button>
      <mdui-button slot="action" variant="filled" loading={busy==='editor'} disabled={Boolean(busy)||editorOwnerChanged||(editor.kind==='goal'?(!goalDraft.title.trim()||!goalDraft.success_criteria.trim()):(editorGoalMissing||!taskDraft.title.trim()||!taskDraft.success_criteria.trim()||!taskDraft.minimum_action.trim()||!validEstimate||!validPriority))} onClick={saveEditor}>保存</mdui-button>
    </mdui-dialog>}
    {deleteTarget&&<mdui-dialog ref={deleteRef} open close-on-esc headline="移入回收站" role="dialog" aria-label="删除影响预览">
      <div className="form-stack"><p>「{deleteTarget.value.title}」{deleteTarget.kind==='goal'?`及其 ${affected.length} 项任务`:`及其 ${Math.max(0,affected.length-1)} 项子任务`}将移入回收站。</p><p className="meta">未完成的今日安排会移出，历史推进和专注记录会保留。恢复后会重新安排未来任务。</p>{affected.length>1&&<ul className="delete-preview">{affected.slice(0,8).map(task=><li key={task.id}>{task.title}</li>)}{affected.length>8&&<li>还有 {affected.length-8} 项</li>}</ul>}{error&&<p role="alert" className="form-error">{error}</p>}</div>
      <mdui-button slot="action" variant="text" disabled={Boolean(busy)} onClick={()=>setDeleteTarget(null)}>取消</mdui-button><mdui-button slot="action" variant="filled" disabled={Boolean(busy)} loading={busy==='delete'} onClick={remove}>确认移入回收站</mdui-button>
    </mdui-dialog>}
  </section>
}

function TreeProposal({proposal,taskTitles,busy,demo,onDecide}:{proposal:Proposal;taskTitles:ReadonlyMap<string,string>;busy:boolean;demo:boolean;onDecide:(apply:boolean)=>void}) {
  let operations:Record<string,unknown>[]=[]
  try{const raw:unknown=JSON.parse(proposal.patch_json);if(Array.isArray(raw))operations=raw.filter((op):op is Record<string,unknown>=>typeof op==='object'&&op!==null)}catch{return null}
  return <div className="proposal-card"><h3>待确认的调整</h3>{demo&&<p className="model-notice">演示示例 · 未使用真实模型</p>}<ProposalSummary args={{patch:operations,instruction:proposal.instruction}} taskTitles={taskTitles}/><div className="actions"><mdui-button variant="text" disabled={busy} onClick={()=>onDecide(false)}>拒绝</mdui-button><mdui-button variant="outlined" disabled={busy} onClick={()=>onDecide(true)}>确认应用</mdui-button></div></div>
}
