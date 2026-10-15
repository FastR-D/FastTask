import { makeAssistantToolUI, useAssistantTransportState } from '@assistant-ui/react'
import { useEffect, useRef, useState } from 'react'
import { fieldValue } from '../../mdui-react'
import { request } from '../../api'
import { TOOL_DAILY_PLAN, TOOL_GOAL_UPDATE, TOOL_GOAL_WITH_TASKS, TOOL_TASK_TREE_PATCH, type ApprovalDecision, type ServerAgentState } from '../state'
import { useProposalDecision } from '../runtime'
import { useComposerDraftUserId } from '../ComposerDraft'

// ProposalCard renders one awaiting-approval proposal and returns the decision via
// an `add-tool-result` command (agent-impl.md §7.1). A live harness stream needs
// a separate receipt request; runtime.ts owns that transport detail. It must NOT use
// respondToApproval/hitl/humanTool: useAssistantTransportRuntime does not wire
// onRespondToToolApproval, so those controls render but do nothing (ADR-0002 §3.1).
// A running tool call can appear before Execute stages its proposal. Readiness
// uses FastTask's own part state; no native SDK approval control is involved.
type CardProps = {
  title: string
  args: Record<string, unknown> | undefined
  result: unknown
  isError?: boolean
  addResult: (result: ApprovalDecision) => void | Promise<void>
  taskTitles?: ReadonlyMap<string,string>
  ready?: boolean
}

function object(value: unknown): Record<string, unknown> {
  return value && typeof value === 'object' && !Array.isArray(value) ? value as Record<string, unknown> : {}
}
function text(value: unknown): string { return typeof value === 'string' ? value : '' }

export function ProposalCard({ title, args, result, isError, addResult, taskTitles, ready = true }: CardProps) {
  const [reason, setReason] = useState('')
  const [sending, setSending] = useState(false)
  const [sendError, setSendError] = useState('')
  const sendingRef = useRef(false)
  const response = object(result)
  const failed = Boolean(isError || response.error || response.decision === 'conflict')
  const applied = response.applied === true && !failed
  const rejected = response.decision === 'reject'
  const submitting = sending || (response.decision === 'approve' && !applied && !failed)
  const state = applied ? '已应用' : rejected ? '已拒绝' : failed ? '未能应用' : submitting ? '等待服务器确认' : ready ? '待确认' : '正在准备'
  const pending = ready && !applied && !rejected && !failed && !submitting

  async function decide(decision: ApprovalDecision) {
    if (!ready || sendingRef.current) return
    sendingRef.current = true
    setSending(true)
    setSendError('')
    try { await addResult(decision) }
    catch (error) { setSendError(error instanceof Error ? error.message : '确认未能送达，请稍后重试。') }
    finally { sendingRef.current = false; setSending(false) }
  }

  return (
    <div className={`agent-proposal${applied || rejected ? ' decided' : ''}`}>
      <header className="agent-proposal-head">
        <mdui-icon name={applied ? 'check_circle' : rejected || failed ? 'cancel' : 'rule'} />
        <b>{title}</b>
        <span className="agent-proposal-state" role="status">{state}</span>
      </header>

      {applied || rejected
        ? <details className="agent-proposal-review"><summary>{text(args?.title) || '查看方案详情'}</summary><ProposalSummary args={args} taskTitles={taskTitles}/></details>
        : <ProposalSummary args={args} taskTitles={taskTitles} />}
      {applied&&<AppliedTodaySummary value={response.today_summary} kind={text(response.kind)}/>}

      {rejected && text(response.reason) && (
        <p className="agent-proposal-reason">理由：{text(response.reason)}</p>
      )}
      {failed && <p className="agent-proposal-error" role="alert">{text(response.detail) || text(response.error) || '提案已过期或无法应用，请让助手根据最新任务重新提出建议。'}</p>}
      {sendError && <p className="agent-proposal-error" role="alert">{sendError}</p>}

      {pending && (
        <div className="agent-proposal-actions">
          <mdui-text-field
            label="调整理由（可选）"
            variant="outlined"
            value={reason}
            onInput={e => setReason(fieldValue(e))}
          />
          <div className="agent-proposal-buttons">
            <mdui-button variant="filled" icon="check" onClick={() => void decide({ decision: 'approve' })}>确认应用</mdui-button>
            {/* Outlined, not tonal: one card gets one accent. The app's other proposal pair (App.tsx
                ProposalCard) is already outlined-reject + filled-apply, and two filled buttons next to
                a 56px field made the decision read as three equally loud controls. */}
            <mdui-button variant="outlined" icon="close" onClick={() => void decide({ decision: 'reject', reason: reason.trim() || undefined })}>暂不应用</mdui-button>
          </div>
        </div>
      )}
    </div>
  )
}

function AppliedTodaySummary({value,kind}:{value:unknown;kind:string}){
  const summary=object(value),message=text(summary.message)
  if(!message)return ['goal_with_tasks','task_tree_patch'].includes(kind)?<p className="meta">任务树已更新；今日是否安排，请查看今日推进。</p>:null
  const items=Array.isArray(summary.items)?summary.items.map(object):[]
  const milestoneCount=summary.milestone_count,workItemCount=summary.work_item_count
  const hasCounts=Number.isInteger(milestoneCount)&&Number.isInteger(workItemCount)&&Number(milestoneCount)>=0&&Number(workItemCount)>=0
  return <div className="agent-proposal-body" aria-label="已应用后的今日安排">
    {hasCounts&&<p className="meta">{Number(milestoneCount)} 个里程碑 · {Number(workItemCount)} 个工作项</p>}
    <p>{message}</p>
    {items.length>0&&<ol>{items.map((item,index)=><li key={index}>
      <b>{text(item.commitment)||text(item.minimum_action)||text(item.title)}</b>
      {text(item.title)&&text(item.title)!==text(item.commitment)&&<p className="meta">{text(item.title)}</p>}
      {text(item.minimum_action)&&<p>最小行动：{text(item.minimum_action)}</p>}
      {Number.isInteger(item.target_minutes)&&Number(item.target_minutes)>0&&<p className="meta">今日专注预算 {Number(item.target_minutes)} 分钟</p>}
    </li>)}</ol>}
  </div>
}

function RuntimeProposalCard({ toolCallId, toolName, ...props }: CardProps & {toolCallId: string; toolName: string}) {
  const decide=useProposalDecision()
  const state=useAssistantTransportState<ServerAgentState>(value=>value as unknown as ServerAgentState)
  // Named/partial tool calls stream before the approval transaction commits.
  // Preserve old requires-action/interrupted/no-status cards with no result;
  // only an explicitly running, unstaged call is still being prepared.
  const message=state.messages?.find(message=>message.role==='assistant'&&message.parts.some(part=>part.type==='tool-call'&&part.toolCallId===toolCallId&&part.toolName===toolName))
  const part=message?.parts.find(part=>part.type==='tool-call'&&part.toolCallId===toolCallId&&part.toolName===toolName)
  const staged=part?.type==='tool-call'&&(part.approval?.status==='pending'||object(part.result).status==='pending')
  const ready=message?.status?.type!=='running'||Boolean(staged)
  const userId=useComposerDraftUserId()
  const owner=useRef(userId)
  owner.current??=userId
  const goalId=text(props.args?.goal_id)
  const patch=Array.isArray(props.args?.patch)?props.args.patch:[]
  const includeDeleted=patch.some(node=>object(node).op==='restore')
  // Daily-plan args intentionally have no goal_id. Read only their bounded,
  // distinct candidate IDs; the current chat goal is not their ownership scope.
  const candidateIds=toolName===TOOL_DAILY_PLAN&&Array.isArray(props.args?.candidates)?[...new Set(props.args.candidates.map(node=>text(object(node).task_id)).filter(Boolean))].slice(0,20):[]
  const candidateKey=JSON.stringify(candidateIds)
  const response=object(props.result)
  const pending=ready&&!props.isError&&!response.error&&response.applied!==true&&!['approve','reject','conflict'].includes(text(response.decision))
  const scope=userId&&owner.current===userId
    ?toolName===TOOL_TASK_TREE_PATCH&&goalId?JSON.stringify([userId,goalId,toolCallId])
      :toolName===TOOL_DAILY_PLAN&&candidateIds.length?JSON.stringify([userId,state.fasttask?.threadId,state.fasttask?.activeGoalId,toolCallId,candidateKey]):''
    :''
  const [names,setNames]=useState<{scope:string;titles:ReadonlyMap<string,string>}|null>(null)
  useEffect(()=>{
    if(!scope||!pending||!userId)return
    const controller=new AbortController()
    let alive=true
    const query=`/tasks?goal_id=${encodeURIComponent(goalId)}`
    const paths=toolName===TOOL_DAILY_PLAN?candidateIds.map(id=>`/tasks/${encodeURIComponent(id)}`):includeDeleted?[query,`${query}&deleted=true`]:[query]
    // Names only aid review. Their availability never changes the staged patch,
    // approval receipt or server-side revision/ownership checks.
    void Promise.allSettled(paths.map(path=>request<unknown>(path,{signal:controller.signal},userId))).then(results=>{
      if(!alive)return
      const titles=new Map<string,string>()
      results.forEach((result,index)=>{
        if(result.status!=='fulfilled')return
        const data=object(result.value.data)
        if(toolName===TOOL_DAILY_PLAN){
          const id=text(data.id),title=text(data.title)
          if(id===candidateIds[index]&&title)titles.set(id,title)
        }else if(Array.isArray(data.items))for(const raw of data.items){
          const task=object(raw),id=text(task.id),title=text(task.title)
          if(id&&title&&task.goal_id===goalId)titles.set(id,title)
        }
      })
      setNames({scope,titles})
    })
    return()=>{alive=false;controller.abort()}
  },[scope,pending,userId,goalId,includeDeleted,candidateKey,toolName])
  return <ProposalCard {...props} ready={ready} taskTitles={names?.scope===scope?names.titles:undefined} addResult={decision=>decide(toolCallId,toolName,decision)}/>
}

// ProposalSummary renders whatever the proposal args carry, defensively: the exact
// arg shape belongs to the server tool, and the card must not break on partial or
// unexpected args (the converter is called mid-stream).
const OP_LABELS: Record<string,string> = {create:'新增',update:'修改',move:'移动',supersede:'废弃',delete:'移入回收站',restore:'恢复'}
const FIELD_LABELS: Record<string,string> = {title:'标题',type:'类型',description:'说明',success_criteria:'验收标准',commitment:'今日推进',minimum_action:'最小行动',status:'状态',blocked_reason:'受阻原因',priority:'优先级',estimate_minutes:'预计总工作量（分钟）',position:'顺序',parent_id:'父任务',parent_ref:'父节点',uncertainty:'研究不确定性',contribution:'研究贡献度',coord_rationale:'坐标依据'}
const TYPE_LABELS:Record<string,string>={milestone:'里程碑',task:'任务',action:'行动'}
const STATUS_LABELS:Record<string,string>={ready:'待开始',in_progress:'进行中',blocked:'受阻',paused:'已暂停',cancelled:'已取消',completed:'已完成',superseded:'已替换',active:'进行中',draft:'待启动',abandoned:'已放弃',archived:'已归档'}

function ProposalDependencies({node,titles}:{node:Record<string,unknown>;titles:Map<string,string>}) {
  const arrays=[node.depends_on_refs,node.depends_on].filter(Array.isArray) as unknown[][]
  if(!arrays.length)return null
  const references=[...new Set(arrays.flat().map(text).filter(Boolean))]
  if(!references.length)return <p>前置成果：清除前置关系</p>
  const unresolved=references.filter(reference=>!titles.has(reference))
  return <>
    <p>前置成果：{references.map(reference=>titles.get(reference)||'关联任务').join('、')}（需最终完成）</p>
    {unresolved.length>0&&<details><summary>查看前置任务引用</summary>{unresolved.map(reference=><p key={reference}><code>{reference}</code></p>)}</details>}
  </>
}

function ProposalField({field,label,value,titles}:{field:string;label:string;value:unknown;titles:Map<string,string>}) {
  const isParent=field==='parent_id'||field==='parent_ref'
  const reference=isParent?text(value):'',title=titles.get(reference)
  const rendered=value===null?'根节点':isParent?title||'关联父任务':field==='type'?TYPE_LABELS[text(value)]||String(value):field==='status'?STATUS_LABELS[text(value)]||String(value):String(value)
  return <>
    <p><span>{label}：</span>{rendered}</p>
    {isParent&&value!==null&&!title&&<details><summary>查看父任务引用</summary><code>{reference}</code></details>}
  </>
}

export function ProposalSummary({ args,taskTitles }: { args: Record<string, unknown> | undefined; taskTitles?:ReadonlyMap<string,string> }) {
  if (!args) return null
  const summary = text(args.instruction) || text(args.summary)
  const list = [args.patch,args.nodes,args.candidates,args.operations].find(Array.isArray) as unknown[] | undefined
  const titles=new Map<string,string>(taskTitles)
  for(const raw of list||[]) {
    const node=object(raw),title=text(node.title)
    if(title)for(const key of ['client_ref','target_id','task_id','id']) {
      const reference=text(node[key])
      if(reference)titles.set(reference,title)
    }
  }
  const isDailyPlan=list===args.candidates
  const changes = object(args.changes)
  return (
    <div className="agent-proposal-body">
      {text(args.title) && <h3>{text(args.title)}</h3>}
      {text(args.description) && <p>{text(args.description)}</p>}
      {text(args.success_criteria) && <p><b>目标验收：</b>{text(args.success_criteria)}</p>}
      {text(args.shortfall_reason) && <p><b>今日不足三项的原因：</b>{text(args.shortfall_reason)}</p>}
      {summary && <p>{summary}</p>}
      {list && <ol className="agent-proposal-diff">{list.map((raw,index) => {
        const node = object(raw)
        const op=text(node.op)||'create'
        const target=text(node.target_id)||text(node.task_id)
        // Existing names come from the caller's already-loaded task catalogue;
        // a proposed rename must not replace the identity the user is reviewing.
        const knownTarget=target?taskTitles?.get(target):undefined
        const title=isDailyPlan?text(node.title)||knownTarget||target||`第 ${index+1} 项`:target?knownTarget||target:text(node.title)||`第 ${index+1} 项`
        return <li key={index} className="agent-proposal-node">
          <b>{OP_LABELS[op] || op} · {title}</b>
          {target&&!knownTarget&&(!isDailyPlan||!text(node.title))&&<p className="meta">任务名称暂不可用，请按上方任务引用核对目标。</p>}
          {Object.entries(FIELD_LABELS).filter(([field]) => node[field] !== undefined&&(field!=='title'||op!=='create')).map(([field,label]) => <ProposalField key={field} field={field} label={label} value={node[field]} titles={titles}/>)}
          <ProposalDependencies node={node} titles={titles}/>
          {op==='delete'&&<p className="meta">所选任务及其未删除的子任务将一起移入回收站；历史推进与专注记录保留。</p>}
          {op==='restore'&&<p className="meta">恢复所选任务及其同一删除批次的子任务；此前独立删除的子项仍留在回收站。</p>}
          {text(node.reason) && <p>{isDailyPlan?'安排理由':'建议理由'}：{text(node.reason)}</p>}
        </li>
      })}</ol>}
      {Object.keys(changes).length > 0 && <dl className="agent-proposal-diff">{Object.entries(changes).map(([key,value]) => <div key={key}><dt>{key==='title'?'标题':FIELD_LABELS[key]||key}</dt><dd>{String(value??'')}</dd></div>)}</dl>}
      {!summary && !list && Object.keys(changes).length===0 && !args.title && <p className="agent-proposal-count">请审阅后决定是否应用。</p>}
    </div>
  )
}

// The two proposal tools the server registers (phases D/E). Rendering these
// components inside the runtime provider registers the cards by tool name
// (frontend.md §4: "提案审批卡片用 makeAssistantToolUI 按工具名注册").
export const TaskTreeProposalUI = makeAssistantToolUI<Record<string, unknown>, ApprovalDecision>({
  toolName: TOOL_TASK_TREE_PATCH,
  display: 'standalone',
  render: ({ args, result, addResult, isError, toolCallId }) => (
    <RuntimeProposalCard title="任务树变更提案" toolCallId={toolCallId} toolName={TOOL_TASK_TREE_PATCH} args={args} result={result} isError={isError} addResult={addResult} />
  ),
})

export const DailyPlanProposalUI = makeAssistantToolUI<Record<string, unknown>, ApprovalDecision>({
  toolName: TOOL_DAILY_PLAN,
  display: 'standalone',
  render: ({ args, result, addResult, isError, toolCallId }) => (
    <RuntimeProposalCard title="今日计划提案" toolCallId={toolCallId} toolName={TOOL_DAILY_PLAN} args={args} result={result} isError={isError} addResult={addResult} />
  ),
})

export const GoalWithTasksProposalUI = makeAssistantToolUI<Record<string,unknown>,ApprovalDecision>({
  toolName:TOOL_GOAL_WITH_TASKS,display:'standalone',
  render:({args,result,addResult,isError,toolCallId}) => <RuntimeProposalCard title="新科研目标与任务" toolCallId={toolCallId} toolName={TOOL_GOAL_WITH_TASKS} args={args} result={result} isError={isError} addResult={addResult}/>,
})
export const GoalUpdateProposalUI = makeAssistantToolUI<Record<string,unknown>,ApprovalDecision>({
  toolName:TOOL_GOAL_UPDATE,display:'standalone',
  render:({args,result,addResult,isError,toolCallId}) => <RuntimeProposalCard title="目标调整提案" toolCallId={toolCallId} toolName={TOOL_GOAL_UPDATE} args={args} result={result} isError={isError} addResult={addResult}/>,
})
