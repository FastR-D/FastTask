import { ApiError, idem, request } from './api'
import { SESSION_OWNER_CHANGED } from './session-owner'

export type CreationResponse<T> = { data:T; etag:string|null }

type Attempt<T> = {
  path:string
  body:string
  key:string
  headers:Headers
  uncertain:boolean
  response?:CreationResponse<T>
}

// A definite rejection of the FIRST request permits correcting the draft and
// starting over. Conflict, timeout and rate-limit responses can leave a request
// in progress; ApiError currently has no problem code to distinguish them.
function definiteRejection(error:unknown) {
  if(error instanceof Error&&error.message===SESSION_OWNER_CHANGED)return true
  return error instanceof ApiError && (error.requestSent===false || error.status>=400 && error.status<500 && ![408,409,425,429].includes(error.status))
}

// One instance belongs to one new-resource editor. It performs one POST per
// explicit recover() call, preserving the original request across uncertain
// failures. It never updates an existing resource or chooses editable fields.
// After recovery the UI must bind this returned resource/ETag BEFORE attempting
// a PATCH with the user's current draft, so a PATCH conflict cannot create again.
export function createCreationAttempt<T>(expectedUserId?:string,getCurrentUserId?:()=>string|undefined) {
  let attempt:Attempt<T>|null=null
  let inFlight:Promise<CreationResponse<T>>|null=null
  return {
    recover(path:string,payload:unknown,headers?:HeadersInit):Promise<CreationResponse<T>> {
      if(expectedUserId&&getCurrentUserId&&getCurrentUserId()!==expectedUserId)return Promise.reject(new Error(SESSION_OWNER_CHANGED))
      if(attempt&&attempt.path!==path)return Promise.reject(new Error('请先核对上次新建请求的结果，再开始另一类新建操作。'))
      if(attempt?.response)return Promise.resolve(attempt.response)
      if(inFlight)return inFlight
      if(!attempt){
        const key=idem(),originalHeaders=new Headers(headers)
        originalHeaders.set('Idempotency-Key',key)
        attempt={path,body:JSON.stringify(payload),key,headers:originalHeaders,uncertain:false}
      }
      const current=attempt
      inFlight=request<T>(current.path,{method:'POST',headers:current.headers,body:current.body},expectedUserId).then(response=>{
        current.response=response
        return response
      }).catch((error:unknown)=>{
        if(!current.uncertain&&definiteRejection(error))attempt=null
        else current.uncertain=true
        throw error
      }).finally(()=>{inFlight=null})
      return inFlight
    },
    pending(){return Boolean(attempt&&!attempt.response)},
    // The caller may clear a resolved attempt, or explicitly start a distinct
    // editor. Never automatically discard a pending attempt after a network
    // error; its first POST may already have committed.
    reset(){
      if(inFlight)return false
      attempt=null
      return true
    },
  }
}
