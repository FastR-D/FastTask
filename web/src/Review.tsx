import { useEffect, useRef, useState } from 'react'
import { Review as ReviewOverview, isoWeekOf, shiftWeek } from './Lens'
import { request } from './api'
import { localDateInTimezone } from './date'
import { entryGroup, entryKind, entryState, mergeReviewEntries, reportFromRecords, reviewStamp, taskReviewEntry, validReviewEntry } from './review-data'
import type { ReviewActivity, ReviewEntry } from './review-data'
import './review-workspace.css'

type View = 'overview'|'activity'|'report'
type Props = {userId:string;timezone:string;refreshVersion:number;onNotice:(message:string)=>void}
const message = (error:unknown) => error instanceof Error ? error.message : '暂时无法读取，请重试'
const currentWeek = (timezone:string) => isoWeekOf(new Date(localDateInTimezone(new Date(),timezone)+'T12:00:00Z'))

export function Review(props:Props) {
  const [view,setView] = useState<View>('overview')
  return <div className="review-hub">
    <nav className="review-tabs" aria-label="复盘页面">
      {([['overview','复盘概览'],['activity','成果汇总'],['report','周报']] as const).map(([value,label])=><button key={value} type="button" aria-current={view===value?'page':undefined} onClick={()=>setView(value)}>{label}</button>)}
    </nav>
    {view==='overview'&&<ReviewOverview key={'overview:'+props.userId} onNotice={props.onNotice}/>}
    <ReviewWorkspace key={'workspace:'+props.userId} {...props} view={view}/>
  </div>
}

export function ReviewWorkspace({userId,timezone,refreshVersion,onNotice,view}:Props&{view:View}) {
  const [engaged,setEngaged] = useState(view!=='overview')
  const [week,setWeek] = useState(()=>currentWeek(timezone))
  const [activity,setActivity] = useState<ReviewActivity|null>(null)
  const [localBusy,setLocalBusy] = useState(false)
  const [error,setError] = useState('')
  const [kindFilter,setKindFilter] = useState('all')
  const [query,setQuery] = useState('')
  const [drafts,setDrafts] = useState<Record<string,string>>({})
  const [candidate,setCandidate] = useState<string|null>(null)
  const [previousDraft,setPreviousDraft] = useState<string|null>(null)
  const generation = useRef(0)
  const controller = useRef<AbortController|null>(null)
  const active = useRef(true)
  const latestOwner = useRef(userId); latestOwner.current=userId
  const draft = drafts[week] || ''
  useEffect(()=>{if(view!=='overview')setEngaged(true)},[view])
  useEffect(()=>{active.current=true;return()=>{active.current=false;generation.current++;controller.current?.abort()}},[])

  async function loadLocal(offset=0,epoch=generation.current,signal=controller.current?.signal) {
    setLocalBusy(true);setError('')
    try {
      const {data}=await request<ReviewActivity>('/reviews/activity?week='+encodeURIComponent(week)+'&limit=100&offset='+offset,{signal},userId)
      if(!active.current||epoch!==generation.current||latestOwner.current!==userId)return
      setActivity(previous=>offset&&previous?{...data,items:mergeReviewEntries(previous.items,data.items)}:data)
    } catch(e) {if(active.current&&epoch===generation.current&&!signal?.aborted)setError(message(e))}
    finally {if(active.current&&epoch===generation.current)setLocalBusy(false)}
  }
  function refresh() {
    const epoch=++generation.current
    controller.current?.abort();controller.current=new AbortController()
    setActivity(null)
    void loadLocal(0,epoch,controller.current.signal)
  }
  useEffect(()=>{if(engaged)refresh();return()=>{generation.current++;controller.current?.abort()}},[engaged,userId,week,refreshVersion])

  const allEntries=(activity?.items||[]).filter(taskReviewEntry)
  const visible=allEntries.filter(item=>(kindFilter==='all'||entryGroup(item)===kindFilter)&&`${item.title} ${item.summary} ${item.goal_title||''} ${item.task_title||''}`.toLowerCase().includes(query.toLowerCase().trim()))
  function generateReport() {
    if(!activity||week==='all'||localBusy)return
    const next=reportFromRecords({...activity,items:allEntries})
    if(draft&&next!==draft)setCandidate(next)
    else setDrafts(state=>({...state,[week]:next}))
  }
  async function copyReport() {
    try {await navigator.clipboard.writeText(draft);onNotice('周报已复制')}catch{onNotice('复制未成功，请在编辑框中选择并复制周报')}
  }
  function downloadReport() {
    const url=URL.createObjectURL(new Blob([draft],{type:'text/markdown;charset=utf-8'}))
    const anchor=document.createElement('a');anchor.href=url;anchor.download='科研周报-'+week+'.md';anchor.click();setTimeout(()=>URL.revokeObjectURL(url),1000)
  }
  function selectWeek(value:string) {if(value===week)return;setCandidate(null);setPreviousDraft(null);setWeek(value)}
  return <section className="review-workspace" hidden={view==='overview'} aria-label={view==='report'?'周报':'成果汇总'}>
    <header className="page-head"><div className="title-block"><p className="eyebrow">{week==='all'?'已保存的任务记录':activity?activity.start_date+' → '+activity.end_date:week}</p><h1>{view==='report'?'周报':'成果汇总'}</h1><p className="meta">{view==='report'?'依据 FastTask 任务进展和专注记录整理，生成后可继续编辑。':'汇集 FastTask 任务成果、阶段进展与专注投入。'}</p></div>
      <button type="button" className="review-button" disabled={localBusy} onClick={refresh}><mdui-icon name="refresh"/>{localBusy?'正在读取':'刷新记录'}</button>
    </header>
    <div className="review-period"><div className="review-period-buttons"><button type="button" className="review-button" onClick={()=>selectWeek(shiftWeek(week==='all'?currentWeek(timezone):week,-1))} aria-label="上一周"><mdui-icon name="chevron_left"/></button><input type="week" aria-label="复盘周次" value={week==='all'?'':week} max={currentWeek(timezone)} onChange={event=>{if(/^\d{4}-W\d{2}$/.test(event.target.value))selectWeek(event.target.value)}}/><button type="button" className="review-button" disabled={week==='all'||week>=currentWeek(timezone)} onClick={()=>selectWeek(shiftWeek(week,1))} aria-label="下一周"><mdui-icon name="chevron_right"/></button><button type="button" className="review-button" onClick={()=>selectWeek(currentWeek(timezone))}>本周</button>{view==='activity'&&<button type="button" className="review-button" aria-pressed={week==='all'} onClick={()=>selectWeek('all')}>全部时间</button>}</div></div>
    {!!activity?.warnings.length&&<details className="review-source-details"><summary>记录范围说明</summary>{activity.warnings.map((warning,index)=><p key={index}>{warning}</p>)}</details>}
    {error&&<div className="review-warning" role="alert">FastTask 任务记录读取失败：{error}<button type="button" className="review-button" disabled={localBusy} onClick={()=>void loadLocal()}>重试</button></div>}
    <div hidden={view!=='activity'}>
      <div className="review-counts"><div><b>{allEntries.filter(item=>validReviewEntry(item)&&entryGroup(item)==='result').length}</b><span>任务成果记录</span></div><div><b>{allEntries.filter(item=>validReviewEntry(item)&&entryGroup(item)==='step').length}</b><span>阶段进展</span></div><div><b>{activity?.focus_minutes??'—'}</b><span>有效专注 · 分钟</span></div></div>
      <div className="review-filters"><input type="search" aria-label="搜索任务记录" placeholder="搜索任务、目标或进展" value={query} onChange={event=>setQuery(event.target.value)}/><select aria-label="筛选任务记录" value={kindFilter} onChange={event=>setKindFilter(event.target.value)}><option value="all">所有任务记录</option><option value="result">任务成果</option><option value="step">阶段进展</option><option value="time">专注投入</option></select></div>
      {localBusy&&!activity?<p className="empty" role="status">正在读取 FastTask 任务进展…</p>:!visible.length?<div className="empty-state"><h2>{query||kindFilter!=='all'?'没有符合筛选条件的任务记录':'此范围内暂无任务记录'}</h2><p>在 FastTask 任务中记录完成成果、阶段进展或专注投入。</p></div>:<div className="review-entry-list">{visible.map(item=><OutcomeCard key={item.id} item={item} timezone={activity?.timezone||timezone}/>)}</div>}
    </div>
    <div hidden={view!=='report'} className="review-report">
      {week==='all'&&<p className="review-warning">请选择具体周次生成周报。<button type="button" className="review-button" onClick={()=>selectWeek(currentWeek(timezone))}>查看本周</button></p>}
      <div className="review-report-actions"><button type="button" className="review-button primary" disabled={!activity||localBusy||week==='all'} onClick={generateReport}><mdui-icon name="description"/>{draft?'从最新任务记录重新生成':'生成周报草稿'}</button><button type="button" className="review-button" disabled={!draft} onClick={()=>void copyReport()}>复制</button><button type="button" className="review-button" disabled={!draft} onClick={downloadReport}>导出 Markdown</button></div>
      {candidate!==null&&<div className="review-warning" role="status"><p>已有草稿保持不变。新的任务记录已整理好，可查看后决定是否替换。</p><details><summary>预览新草稿</summary><pre>{candidate}</pre></details><button type="button" className="review-button" onClick={()=>{setPreviousDraft(draft);setDrafts(state=>({...state,[week]:candidate}));setCandidate(null)}}>用新草稿替换</button><button type="button" className="review-button" onClick={()=>setCandidate(null)}>保留当前草稿</button></div>}
      {previousDraft!==null&&<button type="button" className="review-button" onClick={()=>{setDrafts(state=>({...state,[week]:previousDraft}));setPreviousDraft(null)}}>恢复替换前的草稿</button>}
      <textarea aria-label="周报草稿" placeholder="生成周报后，可以在这里补充实际遇到的问题和下周计划。" value={draft} onChange={event=>setDrafts(state=>({...state,[week]:event.target.value}))} spellCheck={false}/><p className="meta">草稿可自由编辑，编辑不会修改任务状态。离开复盘前可复制或导出保存。</p>
    </div>
    {(activity?.has_more||activity?.complete===false)&&<section className="review-more" aria-label="载入更多任务记录"><p className="meta">还有未载入的 FastTask 任务记录。周报会注明当前覆盖范围。</p>{activity?.has_more&&<button type="button" className="review-button" disabled={localBusy} onClick={()=>void loadLocal(activity.next_offset)}>载入更多任务记录</button>}</section>}
  </section>
}

function OutcomeCard({item,timezone}:{item:ReviewEntry;timezone:string}) {
  const title=item.task_title&&item.task_title!==item.title?item.task_title+' · '+item.title:item.title
  return <article className={'review-entry'+(validReviewEntry(item)?'':' is-retracted')}><div className="review-entry-meta"><span>FastTask</span><span>{entryKind(item)}</span>{entryState(item)&&<span className="review-entry-state">{entryState(item)}</span>}<time>{reviewStamp(item.occurred_at,timezone)}</time></div><h2>{title}</h2>{item.summary&&<p className="review-entry-summary">{item.summary}</p>}<div className="review-entry-context">{item.goal_title&&<span>{item.goal_title}</span>}{item.task_id&&<span>FastTask 任务</span>}{item.duration_minutes!==undefined&&<span>{item.duration_minutes} 分钟投入</span>}</div>{item.attribution&&<footer className="review-entry-context">{item.attribution}</footer>}</article>
}
