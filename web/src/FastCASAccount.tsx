import {useEffect,useState} from 'react'
import {request} from './api'
import {fieldValue} from './mdui-react'

type Status={enabled:boolean;issuer?:string;links:{id:string;state:string}[]}
export function FastCASAccount(){
 const [status,setStatus]=useState<Status>()
 const [password,setPassword]=useState('')
 const [error,setError]=useState('')
 const [busy,setBusy]=useState(false)
 const load=()=>request<Status>('/auth/fastcas/status').then(r=>setStatus(r.data))
 useEffect(()=>{void load().catch(e=>setError(e.message))},[])
 const current=status?.links.find(link=>link.state!=='revoked')
 async function act(action:'link'|'reconcile'|'revoke'){
  setBusy(true);setError('')
  try{
   if(action==='link'){const result=await request<{url:string}>('/auth/fastcas/link',{method:'POST',body:JSON.stringify({password})});window.location.assign(result.data.url)}
   else{await request(`/auth/fastcas/links/${encodeURIComponent(current!.id)}/${action}`,{method:'POST',body:JSON.stringify({password})});await load();setPassword('')}
  }catch(e){setError(e instanceof Error?e.message:'认证操作失败')}finally{setBusy(false)}
 }
 return <section className="account-page">
 <header className="page-head"><div className="title-block"><p className="eyebrow">统一身份 · 可选</p><h1>账号认证</h1></div></header>
 <div className="account-body">
 <p>FastCAS 是可选登录方式，不改变你的 FastTask 账号、任务或权限。</p>
 {!status?<p className="meta">正在读取认证状态…</p>:!status.enabled?<p className="meta">此部署未启用 FastCAS，本地登录仍可正常使用。</p>:<>
 <p className="account-state">{current?.state==='active'?'已完成 FastCAS 认证':current?'认证尚未完成，可重试确认':'当前账号未认证'}<span className="meta"> · {status.issuer}</span></p>
 <mdui-text-field variant="outlined" label="确认本地 FastTask 密码" type="password" toggle-password autocomplete="current-password" value={password} onInput={e=>setPassword(fieldValue(e))}/>
 <div className="actions">
 {current?.state==='prepared'&&<mdui-button variant="outlined" disabled={busy||!password} onClick={()=>void act('reconcile')}>确认待完成认证</mdui-button>}
 <mdui-button variant={current?'outlined':'filled'} disabled={busy||!password} loading={busy} onClick={()=>void act(current?'revoke':'link')}>{current?'解除 FastCAS 认证':'认证当前账号'}</mdui-button>
 </div>
 <p className="meta">解除认证只终止 FastCAS 来源会话。本地密码登录、设备权限和任务数据保持原样。</p>
 </>}{error&&<p className="login-error" role="alert"><mdui-icon name="error_outline"/>{error}</p>}
 </div></section>
}
