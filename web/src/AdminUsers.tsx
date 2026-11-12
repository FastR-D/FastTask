import { useEffect, useState } from 'react'
import { request } from './api'
import type { User } from './types'

const blank={id:'',identifier:'',display_name:'',password:'',role:'member',status:'active'}
export function AdminUsers({currentUser}:{currentUser:User}) {
 const [users,setUsers]=useState<User[]>([]),[form,setForm]=useState(blank),[etag,setETag]=useState(''),[error,setError]=useState(''),[busy,setBusy]=useState(false),[query,setQuery]=useState(''),[deleting,setDeleting]=useState(false)
 const load=async()=>setUsers((await request<{items:User[]}>('/admin/users')).data.items)
 useEffect(()=>{void load().catch(e=>setError(e.message))},[])
 async function edit(user:User){try{const result=await request<User>(`/admin/users/${user.id}`);setETag(result.etag||'');setForm({...blank,...result.data,password:''});setDeleting(false)}catch(e){setError(e instanceof Error?e.message:'加载失败')}}
 async function save(remove=false){setBusy(true);setError('');try{
  if(remove) await request(`/admin/users/${form.id}`,{method:'DELETE',headers:{'If-Match':etag}})
  else if(!form.id) await request('/admin/users',{method:'POST',body:JSON.stringify({identifier:form.identifier,display_name:form.display_name,password:form.password,role:form.role,status:form.status})})
  else {
   const old=users.find(u=>u.id===form.id)
   const changes:Record<string,string>={display_name:form.display_name,status:form.status}
   if(old?.identifier!==form.identifier)changes.identifier=form.identifier
   if(old?.role!==form.role)changes.role=form.role
   if(form.password)changes.password=form.password
   await request(`/admin/users/${form.id}`,{method:'PATCH',headers:{'If-Match':etag},body:JSON.stringify(changes)})
  }
  setForm(blank);setDeleting(false);await load()
 }catch(e){setError(e instanceof Error?e.message:'保存失败')}finally{setBusy(false)}}
 async function revoke(user:User){setBusy(true);try{await request(`/admin/users/${user.id}/session-revocation`,{method:'POST'})}catch(e){setError(e instanceof Error?e.message:'操作失败')}finally{setBusy(false)}}
 return <section className="account-page"><header className="page-head"><h1>用户管理</h1><button onClick={()=>{setForm(blank);setDeleting(false)}}>创建用户</button></header>
 {error&&<p role="alert" className="login-error">{error}</p>}
 <label>搜索用户<input value={query} onChange={e=>setQuery(e.target.value)} /></label>
 <div style={{overflowX:'auto'}}><table><thead><tr><th>账号</th><th>姓名</th><th>角色</th><th>状态</th><th>操作</th></tr></thead><tbody>
 {users.filter(u=>`${u.identifier} ${u.display_name}`.toLowerCase().includes(query.toLowerCase())).map(u=><tr key={u.id}><td>{u.identifier}</td><td>{u.display_name}</td><td>{u.role==='admin'?'管理员':'用户'}</td><td>{u.status==='active'?'启用':'停用'}</td><td><button disabled={busy} onClick={()=>void edit(u)}>编辑</button> <button disabled={busy} onClick={()=>void revoke(u)}>撤销会话</button></td></tr>)}
 </tbody></table></div>
 <form className="account-body" onSubmit={e=>{e.preventDefault();void save(deleting)}}>
 <h2>{form.id?'编辑用户':'创建用户'}</h2>
 <label>登录账号<input required minLength={3} maxLength={120} value={form.identifier} onChange={e=>setForm({...form,identifier:e.target.value})}/></label>
 <label>姓名<input required maxLength={120} value={form.display_name} onChange={e=>setForm({...form,display_name:e.target.value})}/></label>
 <label>{form.id?'重置密码（留空保持）':'初始密码'}<input type="password" autoComplete="new-password" required={!form.id} minLength={12} maxLength={200} value={form.password} onChange={e=>setForm({...form,password:e.target.value})}/></label>
 <label>角色<select value={form.role} onChange={e=>setForm({...form,role:e.target.value})}><option value="member">用户</option><option value="admin">管理员</option></select></label>
 <label>状态<select value={form.status} onChange={e=>setForm({...form,status:e.target.value})}><option value="active">启用</option><option value="disabled">停用</option></select></label>
 {deleting&&<p role="alert">确认删除 {form.identifier}？账号、会话和设备访问将被停用。</p>}
 <div className="actions"><button type="submit" disabled={busy}>{deleting?'确认删除':'保存'}</button>{form.id&&form.id!==currentUser.id&&<button type="button" disabled={busy} onClick={()=>setDeleting(!deleting)}>{deleting?'取消删除':'删除用户'}</button>}</div>
 </form></section>
}
