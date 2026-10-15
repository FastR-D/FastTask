import { useEffect, useRef, useState } from 'react'
import { ApiError, idem, request } from './api'
import { fieldValue } from './mdui-react'
import type { Task, User } from './types'
import './integrations.css'

const SYSTEMS = ['fastwrite','fastread','fastnews','fastppt','fastlab'] as const
type System = typeof SYSTEMS[number]
type Profile = 'auto' | 'fastwrite' | 'fastread-web' | 'fastnews-panel' | 'fastnews-local' | 'fastppt-studio' | 'fastlabs'
type Connection = {
  system: System
  api_url: string
  web_url: string
  profile: Profile
  auth_kind: 'bearer' | 'cookie'
  auth_configured: boolean
  personal_instance: boolean
  revision: number
  can_edit_address: boolean
}
type Diagnostic = { code: string; message: string; reason: string; action: string; http_status?: number; retryable: boolean }
type Resource = { id: string; title: string; kind: string; url?: string; summary?: string; path?: string }
type Content = { ok: boolean; resource: Resource; text: string; children: Resource[]; version: string; truncated: boolean; diagnostic?: Diagnostic; warning?: Diagnostic }
type SavedMaterial = { id:string; system:System; title:string; url?:string; status:string; revision:number; text:string; truncated:boolean; warning?:Diagnostic }
type Draft = { api_url: string; web_url: string; profile: Profile; auth_kind: 'bearer' | 'cookie'; auth_token: string; clear_auth: boolean; personal_instance: boolean }
type Props = { user: User | null; onNotice: (message: string) => void; onBusinessChanged?: () => void; initialTaskID?: string }
const LABELS: Record<System,string> = { fastwrite:'FastWrite', fastread:'FastRead', fastnews:'FastNews', fastppt:'FastPPT', fastlab:'FastLab' }
const MODULE_PURPOSES: Record<System,string> = { fastwrite:'撰写、编辑和保存论文正文', fastread:'检索文献、阅读全文与整理笔记', fastnews:'查看研究资讯与关注动态', fastppt:'制作、修改和导出演示文稿', fastlab:'安排实验与查看执行进展' }
const PROFILES: Record<Profile,string> = { auto:'按工具选择协议', fastwrite:'FastWrite 写作服务', 'fastread-web':'FastRead 网页阅读服务', 'fastnews-panel':'FastResearch 资讯收件箱', 'fastnews-local':'FastNews 独立服务', 'fastppt-studio':'FastPPT 本机工作台', fastlabs:'FastLab 本机实验服务' }
const SYSTEM_PROFILES: Record<System,Profile[]> = { fastwrite:['auto','fastwrite'], fastread:['auto','fastread-web'], fastnews:['auto','fastnews-local','fastnews-panel'], fastppt:['auto','fastppt-studio'], fastlab:['auto','fastlabs'] }
const RESOURCE_KINDS: Record<string,string> = { project:'项目', paper:'论文', file:'文件', item:'资讯', material:'材料' }
const emptyDraft: Draft = { api_url:'', web_url:'', profile:'auto', auth_kind:'bearer', auth_token:'', clear_auth:false, personal_instance:false }

function localError(cause: unknown, secret = '') {
  let message = cause instanceof ApiError && cause.status===412 ? '连接设置已更新，请查看最新设置后重试。' : cause instanceof Error ? cause.message : '请求失败，请稍后重试。'
  if(secret)message=message.replaceAll(secret,'[凭据已隐藏]')
  return message
}
function safeLink(value?: string) {
  if(!value||!/^https?:\/\/[^/]/i.test(value.trim())||/[\x00-\x1f\x7f\\]/.test(value))return undefined
  try {
    const url=new URL(value.trim())
    return (url.protocol==='http:'||url.protocol==='https:')&&url.hostname&&!url.username&&!url.password?url.href:undefined
  }catch{return undefined}
}

export function Integrations({ user, onNotice, onBusinessChanged, initialTaskID = '' }: Props) {
  const [system,setSystem]=useState<System>('fastwrite')
  const [connections,setConnections]=useState<Connection[]>([])
  const [tasks,setTasks]=useState<Task[]>([])
  const [tasksError,setTasksError]=useState('')
  const [draft,setDraft]=useState<Draft>(emptyDraft)
  const [configurationOpen,setConfigurationOpen]=useState(false)
  const [advancedOpen,setAdvancedOpen]=useState(Boolean(initialTaskID))
  const [loading,setLoading]=useState(true)
  const [error,setError]=useState('')
  const [diagnostic,setDiagnostic]=useState<Diagnostic|null>(null)
  const [diagnosticOK,setDiagnosticOK]=useState(false)
  const [resources,setResources]=useState<Resource[]>([])
  const [query,setQuery]=useState('')
  const [appliedQuery,setAppliedQuery]=useState('')
  const [offset,setOffset]=useState(0)
  const [hasMore,setHasMore]=useState(false)
  const [listBusy,setListBusy]=useState(false)
  const [listLoaded,setListLoaded]=useState(false)
  const [content,setContent]=useState<Content|null>(null)
  const [contentBusy,setContentBusy]=useState(false)
  const [selectedTask,setSelectedTask]=useState(initialTaskID)
  const [busy,setBusy]=useState<'save'|'diagnose'|'material'|'remove'|''>('')
  const [saved,setSaved]=useState(false)
  const [savedMaterialID,setSavedMaterialID]=useState('')
  const [materials,setMaterials]=useState<SavedMaterial[]>([])
  const [materialsOffset,setMaterialsOffset]=useState(0)
  const [materialsHasMore,setMaterialsHasMore]=useState(false)
  const [materialsBusy,setMaterialsBusy]=useState(false)
  const [materialsError,setMaterialsError]=useState('')
  const [removeTarget,setRemoveTarget]=useState('')
  const busyRef=useRef(false)
  const currentSystem=useRef<System>(system)
  const listSequence=useRef(0)
  const contentSequence=useRef(0)
  const materialsSequence=useRef(0)
  const currentTask=useRef(initialTaskID)
  const connection=connections.find(item=>item.system===system)
  const canEditAddress=user?.role==='admin'&&connection?.can_edit_address===true

  function selectTask(taskID:string){currentTask.current=taskID;materialsSequence.current++;setSelectedTask(taskID);setSaved(false);setRemoveTarget('');setMaterials([]);setMaterialsError('');setMaterialsBusy(false);setMaterialsOffset(0);setMaterialsHasMore(false)}
  useEffect(()=>{selectTask(initialTaskID);if(initialTaskID)setAdvancedOpen(true)},[initialTaskID])
  useEffect(()=>{if(selectedTask)void loadMaterials(selectedTask,0)},[selectedTask])

  async function loadConnections() {
    const response=await request<{items:Connection[]}>('/integrations/connections')
    setConnections(response.data.items)
    return response.data.items
  }
  useEffect(()=>{
    let alive=true
    void Promise.allSettled([request<{items:Connection[]}>('/integrations/connections'),request<{items:Task[]}>('/tasks?limit=100')]).then(([connectionResponse,taskResponse])=>{
      if(!alive)return
      if(connectionResponse.status==='fulfilled')setConnections(connectionResponse.value.data.items)
      else setError(localError(connectionResponse.reason))
      if(taskResponse.status==='fulfilled')setTasks(taskResponse.value.data.items.filter(task=>!task.deleted_at))
      else setTasksError('任务列表暂时无法加载，请重试后选择关联任务。')
      setLoading(false)
    })
    return()=>{alive=false;listSequence.current++;contentSequence.current++;materialsSequence.current++}
  },[])

  useEffect(()=>{
    if(!connection){setDraft({...emptyDraft});return}
    setDraft({api_url:connection.api_url,web_url:connection.web_url,profile:connection.profile,auth_kind:connection.auth_kind,auth_token:'',clear_auth:false,personal_instance:connection.personal_instance})
    setConfigurationOpen(false)
  },[system,connection])

  useEffect(()=>{
    if(advancedOpen&&(connection?.api_url||connection?.web_url))void loadResources('',0)
  },[advancedOpen,system,connection?.revision])

  function selectSystem(next:System) {
    if(busyRef.current)return
    currentSystem.current=next;listSequence.current++;contentSequence.current++
    setSystem(next);setResources([]);setContent(null);setDiagnostic(null);setError('');setQuery('');setAppliedQuery('');setOffset(0);setHasMore(false);setListLoaded(false);setListBusy(false);setContentBusy(false);setSaved(false)
    setDraft({...emptyDraft})
  }
  async function loadResources(search=query,pageOffset=offset) {
    if(!connection?.api_url&&!connection?.web_url)return
    const selected=system,sequence=++listSequence.current
    setListBusy(true);setError('');setDiagnostic(null);setContent(null);setContentBusy(false);setSaved(false);contentSequence.current++
    try {
      const params=new URLSearchParams({q:search,limit:'20',offset:String(pageOffset)})
      const {data}=await request<{ok:boolean;items:Resource[];limit:number;offset:number;has_more:boolean;diagnostic?:Diagnostic;warning?:Diagnostic}>(`/integrations/${selected}/resources?${params}`)
      if(sequence!==listSequence.current||currentSystem.current!==selected)return
      setResources(data.ok?data.items:[]);setAppliedQuery(search);setOffset(data.offset??pageOffset);setHasMore(data.ok&&data.has_more);setListLoaded(true)
      if(data.diagnostic){setDiagnostic(data.diagnostic);setDiagnosticOK(data.ok)}
      else if(data.warning){setDiagnostic(data.warning);setDiagnosticOK(false)}
    }catch(cause){if(sequence===listSequence.current&&currentSystem.current===selected){setError(localError(cause));setResources([]);setHasMore(false);setListLoaded(false)}}
    finally{if(sequence===listSequence.current)setListBusy(false)}
  }
  async function readContent(resource:Resource) {
    const selected=system,sequence=++contentSequence.current
    setContentBusy(true);setError('');setDiagnostic(null);setSaved(false)
    try {
      const params=new URLSearchParams({resource_id:resource.id})
      if(resource.path)params.set('path',resource.path)
      const {data}=await request<Content>(`/integrations/${selected}/content?${params}`)
      if(sequence!==contentSequence.current||currentSystem.current!==selected)return
      setContent(data.ok?data:null)
      if(data.diagnostic){setDiagnostic(data.diagnostic);setDiagnosticOK(data.ok)}
    }catch(cause){if(sequence===contentSequence.current&&currentSystem.current===selected){setError(localError(cause));setContent(null)}}
    finally{if(sequence===contentSequence.current)setContentBusy(false)}
  }
  async function loadTasks(){
    try{const {data}=await request<{items:Task[]}>('/tasks?limit=100');setTasks(data.items.filter(task=>!task.deleted_at));setTasksError('')}
    catch{setTasksError('任务列表暂时无法加载，请重试后选择关联任务。')}
  }
  async function loadMaterials(taskID=selectedTask,pageOffset=materialsOffset){
    if(!taskID)return
    const sequence=++materialsSequence.current
    setMaterialsBusy(true);setMaterialsError('')
    try{
      const {data}=await request<{items:SavedMaterial[];offset:number;has_more:boolean}>(`/tasks/${encodeURIComponent(taskID)}/materials?limit=20&offset=${pageOffset}`)
      if(sequence!==materialsSequence.current||currentTask.current!==taskID)return
      setMaterials(data.items);setMaterialsOffset(data.offset??pageOffset);setMaterialsHasMore(data.has_more)
    }catch(cause){if(sequence===materialsSequence.current&&currentTask.current===taskID)setMaterialsError(`材料暂时无法加载：${localError(cause)}`)}
    finally{if(sequence===materialsSequence.current)setMaterialsBusy(false)}
  }
  async function removeMaterial(material:SavedMaterial){
    const taskID=selectedTask
    await guarded('remove',async()=>{
      setMaterialsError('')
      try{
        await request(`/tasks/${encodeURIComponent(taskID)}/materials/${encodeURIComponent(material.id)}`,{method:'DELETE',headers:{'If-Match':`"import_${material.id}_rev_${material.revision}"`,'Idempotency-Key':idem()}})
        setRemoveTarget('');if(material.id===savedMaterialID){setSaved(false);setSavedMaterialID('')};onNotice('已移除这份任务材料，来源中的原文仍保留。');onBusinessChanged?.()
        await loadMaterials(taskID,materialsOffset)
      }catch(cause){const message=cause instanceof ApiError&&cause.status===412?'材料已在另一处更新，请查看最新内容后重试。':localError(cause);if(cause instanceof ApiError&&cause.status===412){setRemoveTarget('');await loadMaterials(taskID,materialsOffset)}setMaterialsError(`未能移除材料：${message}`)}
    })
  }
  async function guarded(kind:'save'|'diagnose'|'material'|'remove',action:()=>Promise<void>) {
    if(busyRef.current)return
    busyRef.current=true;setBusy(kind)
    try{await action()}finally{busyRef.current=false;setBusy('')}
  }
  async function saveConnection() {
    if(!connection)return
    const active=connection,token=draft.auth_token.trim()
    await guarded('save',async()=>{
      setError('')
      try {
        const body={api_url:canEditAddress?draft.api_url.trim():active.api_url,web_url:canEditAddress?draft.web_url.trim():active.web_url,profile:draft.profile,auth_kind:draft.auth_kind,personal_instance:canEditAddress?draft.personal_instance:active.personal_instance,...(token?{auth_token:token}:{}),...(draft.clear_auth?{clear_auth:true}:{})}
        const {data}=await request<Connection>(`/integrations/${active.system}/connection`,{method:'PUT',headers:{'If-Match':`"integration_${active.system}_rev_${active.revision}"`,'Idempotency-Key':idem()},body:JSON.stringify(body)})
        setConnections(current=>current.map(item=>item.system===active.system?data:item))
        setDraft(current=>({...current,auth_token:'',clear_auth:false}))
        setDiagnostic(null);setSaved(false)
        onNotice('本人连接已保存，凭据不会回显。')
      }catch(cause){setError(localError(cause,token));if(cause instanceof ApiError&&cause.status===412)await loadConnections().catch(()=>undefined)}
      finally{setDraft(current=>({...current,auth_token:''}))}
    })
  }
  async function diagnose() {
    const selected=system
    await guarded('diagnose',async()=>{
      setError('')
      try{const {data}=await request<{ok:boolean;diagnostic:Diagnostic}>(`/integrations/${selected}/diagnostics`,{method:'POST'});if(currentSystem.current!==selected)return;setDiagnostic(data.diagnostic);setDiagnosticOK(data.ok)}
      catch(cause){setError(localError(cause))}
    })
  }
  async function saveMaterial() {
    if(!content?.ok||!content.text.trim()||!selectedTask||contentBusy)return
    const selected=system,resource=content.resource
    await guarded('material',async()=>{
      setError('');setDiagnostic(null)
      try {
        const {data}=await request<{ok:boolean;import_id?:string;diagnostic?:Diagnostic}>(`/integrations/${selected}/materials`,{method:'POST',headers:{'Idempotency-Key':idem()},body:JSON.stringify({resource_id:resource.id,...(resource.path?{path:resource.path}:{}),task_id:selectedTask})})
        if(currentSystem.current!==selected)return
        if(data.ok){setSaved(true);setSavedMaterialID(data.import_id||'');onNotice('已保存到关联任务材料，可在对话中引用。');onBusinessChanged?.();await loadMaterials(selectedTask,0)}
        else if(data.diagnostic){setDiagnostic(data.diagnostic);setDiagnosticOK(false)}
      }catch(cause){setError(localError(cause))}
    })
  }

  const addressChanged=connection&&(draft.api_url.trim()!==connection.api_url||draft.auth_kind!==connection.auth_kind)
  return <section className="integrations-page">
    <header className="page-head"><div className="title-block"><p className="eyebrow">研究工作 · 打开工具</p><h1>研究工具</h1></div></header>
    <p className="integrations-intro">打开对应模块的网页，继续检索、阅读、写作或实验。需要登录时，在该模块网页完成登录。</p>
    <nav className="integrations-launchers" aria-label="研究工具网页">
      <ul>{SYSTEMS.map(value=>{
        const entry=connections.find(item=>item.system===value),href=safeLink(entry?.web_url)
        return <li key={value}><div><b>{LABELS[value]}</b><p>{MODULE_PURPOSES[value]}</p></div>{href?<a className="integrations-open-link" href={href} target="_blank" rel="noopener noreferrer">打开 {LABELS[value]}<mdui-icon name="open_in_new"/></a>:<span className="meta">{loading?'正在读取网页入口…':entry?.web_url?'网页地址无效，请检查连接设置':'尚未配置网页地址'}</span>}</li>
      })}</ul>
    </nav>
    {error&&<p role="alert" className="integrations-error">{error}</p>}
    <details className="integrations-advanced" open={advancedOpen} onToggle={event=>setAdvancedOpen((event.currentTarget as HTMLDetailsElement).open)}>
      <summary onClick={event=>{event.preventDefault();setAdvancedOpen(current=>!current)}}>已有材料与高级连接</summary>
      {advancedOpen&&<>
    <p className="meta">按需读取已授权的工具材料，或管理内容接口连接；打开上方网页不需要在此填写凭据。</p>
    <div className="integrations-toolbar">
      <mdui-select label="工具" aria-label="选择工具" variant="outlined" value={system} disabled={Boolean(busy)} onChange={event=>selectSystem(fieldValue(event) as System)}>{SYSTEMS.map(value=><mdui-menu-item key={value} value={value}>{LABELS[value]}</mdui-menu-item>)}</mdui-select>
      <span className="integrations-connection-state">{loading?'正在读取连接…':connection?.api_url?`${connection.auth_configured?'已设置本人凭据':connection.personal_instance?'个人实例 · 无鉴权':'尚未设置凭据'}`:'尚未配置连接'}</span>
      <mdui-button variant="outlined" icon="network_check" disabled={Boolean(busy)||loading} loading={busy==='diagnose'} onClick={diagnose}>诊断连接</mdui-button>
    </div>
    {diagnostic&&<DiagnosticView value={diagnostic} ok={diagnosticOK}/>}
    {connection&&<details className="integrations-config" open={configurationOpen} onToggle={event=>setConfigurationOpen((event.currentTarget as HTMLDetailsElement).open)}>
      <summary>本人连接设置<span>仅用于当前账号</span></summary>
      <div className="integrations-config-body">
        {canEditAddress?<div className="integrations-form-row"><mdui-text-field label="API 地址" variant="outlined" value={draft.api_url} onInput={event=>setDraft(current=>({...current,api_url:fieldValue(event)}))}/><mdui-text-field label="网页地址" variant="outlined" value={draft.web_url} onInput={event=>setDraft(current=>({...current,web_url:fieldValue(event)}))}/></div>:<div className="integrations-addresses"><p>API 地址：{connection.api_url||'尚未配置'}</p><p>网页地址：{connection.web_url||'尚未配置'}</p><p className="meta">连接地址与个人实例模式由管理员为本人账号配置。</p></div>}
        <div className="integrations-form-row"><mdui-select label="接口协议" variant="outlined" value={draft.profile} onChange={event=>setDraft(current=>({...current,profile:fieldValue(event) as Profile}))}>{SYSTEM_PROFILES[system].map(value=><mdui-menu-item key={value} value={value}>{PROFILES[value]}</mdui-menu-item>)}</mdui-select><mdui-select label="鉴权方式" variant="outlined" value={draft.auth_kind} onChange={event=>setDraft(current=>({...current,auth_kind:fieldValue(event) as Draft['auth_kind']}))}><mdui-menu-item value="bearer">访问令牌</mdui-menu-item><mdui-menu-item value="cookie">本人会话</mdui-menu-item></mdui-select></div>
        <div className="integrations-form-row"><mdui-text-field label={draft.auth_kind==='cookie'?'本人会话值':'访问令牌'} aria-label="新连接凭据" type="password" autocomplete="off" variant="outlined" placeholder={connection.auth_configured?'已设置，留空保留':'只填写本人的凭据'} value={draft.auth_token} onInput={event=>setDraft(current=>({...current,auth_token:fieldValue(event),clear_auth:false}))}/><mdui-select label="已有凭据" variant="outlined" value={draft.clear_auth?'clear':'keep'} onChange={event=>setDraft(current=>({...current,clear_auth:fieldValue(event)==='clear',auth_token:''}))}><mdui-menu-item value="keep">留空保留已有凭据</mdui-menu-item><mdui-menu-item value="clear">清除已有凭据</mdui-menu-item></mdui-select></div>
        {draft.auth_kind==='cookie'&&<p className="meta">只填写本人的会话值。FastRead 使用 fastread_session；FastNews 独立服务使用 fastnews_session；FastResearch 收件箱使用 fr_session。</p>}
        {addressChanged&&!draft.auth_token.trim()&&<p className="integrations-warning">修改 API 地址或鉴权方式后，旧凭据会被清除；请重新填写本人的凭据。</p>}
        {canEditAddress&&<mdui-select label="实例访问方式" variant="outlined" value={draft.personal_instance?'personal':'authenticated'} onChange={event=>setDraft(current=>({...current,personal_instance:fieldValue(event)==='personal'}))}><mdui-menu-item value="authenticated">需要本人凭据</mdui-menu-item><mdui-menu-item value="personal">本人专用实例（无鉴权）</mdui-menu-item></mdui-select>}
        {draft.personal_instance&&<p className="integrations-warning">已选择本人专用实例：仅接受同机回环地址，不能用于多人共用服务。FastPPT 和 FastLab 的当前协议仅支持这种方式。</p>}
        <mdui-button variant="outlined" disabled={Boolean(busy)||!connection.api_url&&!canEditAddress} loading={busy==='save'} onClick={saveConnection}>保存本人连接</mdui-button>
      </div>
    </details>}
    <div className="integrations-task-context">
      <mdui-select label="关联研究任务" aria-label="选择关联任务" variant="outlined" value={selectedTask} disabled={Boolean(busy)} onChange={event=>selectTask(fieldValue(event))}><mdui-menu-item value="">选择已有任务</mdui-menu-item>{tasks.map(task=><mdui-menu-item key={task.id} value={task.id}>{task.title}</mdui-menu-item>)}</mdui-select>
      <p className="meta">选择任务后，可保存工具原文，并查看已带回的材料。</p>
      {tasksError&&<p role="alert" className="integrations-error">{tasksError}<mdui-button variant="text" disabled={Boolean(busy)} onClick={()=>void loadTasks()}>重新加载任务</mdui-button></p>}
    </div>
    {!connection?.api_url&&!connection?.web_url&&!loading?<div className="empty-state"><h2>{LABELS[system]} 尚未配置地址</h2><p>{canEditAddress?'可在本人连接设置中登记网页地址；内容接口按需单独设置。':'请管理员登记网页地址，即可从上方打开对应模块。'}</p></div>:<div className="integrations-browser">
      <section className="integrations-resources" aria-label="工具资源列表"><form className="integrations-search" onSubmit={event=>{event.preventDefault();void loadResources(query,0)}}><mdui-text-field label="搜索内容" variant="outlined" value={query} onInput={event=>setQuery(fieldValue(event))}/><mdui-button variant="text" icon="search" disabled={listBusy||Boolean(busy)} onClick={()=>void loadResources(query,0)}>搜索</mdui-button></form>
        {listBusy?<p className="empty">正在读取资源…</p>:<ul>{resources.map(resource=><li key={`${resource.id}:${resource.path||''}`}><button type="button" aria-label={resource.title} onClick={()=>void readContent(resource)} className={content?.resource.id===resource.id&&content?.resource.path===resource.path?'selected':''} disabled={Boolean(busy)}><b>{resource.title}</b>{resource.summary&&<small>{resource.summary}</small>}<span>{RESOURCE_KINDS[resource.kind]||'内容'}</span></button></li>)}</ul>}
        {!listBusy&&listLoaded&&resources.length===0&&!diagnostic&&<p className="empty">没有找到内容，可以换个关键词或先在工具中创建内容。</p>}
        <div className="integrations-pagination"><mdui-button variant="text" disabled={offset===0||listBusy||Boolean(busy)} onClick={()=>void loadResources(appliedQuery,Math.max(0,offset-20))}>上一页</mdui-button><span>第 {Math.floor(offset/20)+1} 页</span><mdui-button variant="text" disabled={!hasMore||listBusy||Boolean(busy)} onClick={()=>void loadResources(appliedQuery,offset+20)}>下一页</mdui-button></div>
      </section>
      <section className="integrations-content" aria-label="工具内容详情">{contentBusy?<p className="empty">正在读取内容…</p>:content?<>
        <header><div><p className="meta">{LABELS[system]}{content.resource.path?` · ${content.resource.path}`:''}</p><h2>{content.resource.title}</h2></div>{safeLink(content.resource.url)&&<a href={safeLink(content.resource.url)} target="_blank" rel="noopener noreferrer">查看来源<mdui-icon name="open_in_new"/></a>}</header>
        {content.resource.summary&&<p className="integrations-summary">{content.resource.summary}</p>}
        {content.warning&&<div><p className="meta">以下为已成功读取的部分内容，另有内容尚未取得：</p><DiagnosticView value={content.warning} ok={false}/></div>}
        {content.children?.length>0&&<nav className="integrations-files" aria-label="项目文件">{content.children.map(resource=><button type="button" key={`${resource.id}:${resource.path||''}`} onClick={()=>void readContent(resource)} disabled={Boolean(busy)}><mdui-icon name="description"/>{resource.path||resource.title}</button>)}</nav>}
        {content.text?<div className="integrations-text">{content.text}</div>:!content.children?.length&&<p className="empty">这个资源暂时没有可读取的文本。</p>}
        {content.truncated&&<p className="integrations-warning">内容较长，此处显示节选。保存材料时会由服务端重新读取内容。</p>}
        <div className="integrations-material"><h3>带回研究任务</h3><p className="meta">保存后可在对话中引用，不会自动完成任务或占用今日名额。</p><div className="integrations-material-actions"><mdui-button variant="filled" icon={saved?'check':undefined} disabled={Boolean(busy)||!content.text.trim()||!selectedTask||saved} loading={busy==='material'} onClick={saveMaterial}>{saved?'已保存到任务材料':'保存到任务材料'}</mdui-button>{!selectedTask&&<span className="meta">先在上方选择关联任务。</span>}</div>{!content.text.trim()&&<p className="meta">{content.children?.length?'先选择一份有正文的文件再保存。':'这份内容没有可保存的正文，请选择其他内容。'}</p>}{!tasks.length&&!tasksError&&<p className="meta">先在工作台对话中建立研究任务，再保存材料。</p>}</div>
      </>:<div className="integrations-content-empty"><mdui-icon name="description"/><p>选择一项内容，查看原文与材料。</p>{system==='fastnews'&&<p className="meta">独立 FastNews 和 FastResearch 收件箱使用不同会话，请选择实际部署的接口协议。</p>}</div>}</section>
    </div>}
    {selectedTask&&<section className="integrations-saved-materials" aria-label="已保存的任务材料">
      <header><div><h2>任务材料</h2><p className="meta">{tasks.find(task=>task.id===selectedTask)?.title||'当前关联任务'} · 助手可在对话中读取这些材料</p></div><mdui-button variant="text" icon="refresh" disabled={materialsBusy||Boolean(busy)} onClick={()=>void loadMaterials(selectedTask,materialsOffset)}>刷新材料</mdui-button></header>
      {materialsError&&<p role="alert" className="integrations-error">{materialsError}</p>}
      {materialsBusy?<p className="empty" role="status">正在读取任务材料…</p>:materials.length?<ul>{materials.map(material=><li key={material.id}>
        <details><summary><b>{material.title}</b><span>{LABELS[material.system]||material.system}</span></summary><div className="integrations-material-detail">{material.text?<div className="integrations-text">{material.text}</div>:<p className="meta">这份材料没有可显示的文本。</p>}{material.truncated&&<p className="meta">当前显示节选，完整内容请在来源工具查看。</p>}{material.warning&&<DiagnosticView value={material.warning} ok={false}/>}<div className="actions">{safeLink(material.url)&&<a href={safeLink(material.url)} target="_blank" rel="noopener noreferrer">查看来源</a>}{['candidate','converted'].includes(material.status)&&<mdui-button variant="text" icon="delete_outline" disabled={Boolean(busy)} onClick={()=>setRemoveTarget(material.id)}>移除材料</mdui-button>}</div>{removeTarget===material.id&&<div className="integrations-remove-confirm"><p>从此任务移除「{material.title}」？来源中的原文仍保留。</p><mdui-button variant="text" disabled={Boolean(busy)} onClick={()=>setRemoveTarget('')}>取消</mdui-button><mdui-button variant="outlined" disabled={Boolean(busy)} loading={busy==='remove'} onClick={()=>void removeMaterial(material)}>确认移除</mdui-button></div>}</div></details>
      </li>)}</ul>:!materialsError&&<p className="empty">还没有任务材料。选择工具内容后，点击“保存到任务材料”。</p>}
      {(materialsOffset>0||materialsHasMore)&&<div className="integrations-pagination"><mdui-button variant="text" disabled={materialsOffset===0||materialsBusy||Boolean(busy)} onClick={()=>void loadMaterials(selectedTask,Math.max(0,materialsOffset-20))}>上一页材料</mdui-button><span>第 {Math.floor(materialsOffset/20)+1} 页</span><mdui-button variant="text" disabled={!materialsHasMore||materialsBusy||Boolean(busy)} onClick={()=>void loadMaterials(selectedTask,materialsOffset+20)}>下一页材料</mdui-button></div>}
    </section>}
      </>}
    </details>
  </section>
}

function DiagnosticView({value,ok}:{value:Diagnostic;ok:boolean}) {
  return <div className={`integrations-diagnostic${ok?' ok':''}`} role={ok?'status':'alert'}><div><mdui-icon name={ok?'check_circle':'info_outline'}/><b>{value.message||'连接诊断'}</b><code>{value.code}</code>{value.http_status!==undefined&&value.http_status>0&&<span>HTTP {value.http_status}</span>}</div>{value.reason&&<p><span>原因</span>{value.reason}</p>}{value.action&&<p><span>建议</span>{value.action}</p>}{!ok&&value.retryable&&<small>处理后可重试。</small>}</div>
}
