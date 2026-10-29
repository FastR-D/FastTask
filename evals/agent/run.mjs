#!/usr/bin/env node
import { readFile, mkdir, writeFile } from 'node:fs/promises'
import { spawn } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import path from 'node:path'
import { evaluateTrial } from './verify.mjs'

const root=fileURLToPath(new URL('../../',import.meta.url))
const suite=JSON.parse(await readFile(path.join(root,'docs/agent-harness/eval-cases.json'),'utf8'))
const selected=process.env.FASTTASK_EVAL_CASES?.split(',').filter(Boolean)
if(selected){for(const id of selected)if(!suite.cases.some(c=>c.id===id))throw new Error(`unknown case ${id}`);suite.cases=suite.cases.filter(c=>selected.includes(c.id))}
const args=process.argv.slice(2)
const mode=args.shift()??'regression'
const execute=(command,commandArgs,input,timeout=180000)=>new Promise(resolve=>{
 const child=spawn(command,commandArgs,{cwd:root,stdio:['pipe','pipe','pipe']})
 let output='',error='',exceeded=false
 const timer=setTimeout(()=>{exceeded=true;child.kill('SIGKILL')},timeout)
 child.stdout.on('data',b=>{output+=b;if(output.length>8*1024*1024){exceeded=true;child.kill('SIGKILL')}})
 child.stderr.on('data',b=>{error=(error+b).slice(-4000)})
 child.on('error',err=>{clearTimeout(timer);resolve({code:1,error:String(err),output})})
 child.on('close',code=>{clearTimeout(timer);resolve({code:exceeded?124:code,output,error})})
 child.stdin.end(input??'')
})
const out=path.resolve(process.env.FASTTASK_EVAL_OUTPUT??'/tmp/fasttask-agent-eval')
await mkdir(out,{recursive:true,mode:0o700})
let failed=0
let partialWrite=Promise.resolve()
const report={schema_version:1,mode,started_at:new Date().toISOString(),cases:[],quality:'not_evaluated'}
if(mode==='regression'){
 const mapping=JSON.parse(await readFile(new URL('./regression-map.json',import.meta.url),'utf8'))
 const unique=[...new Set(Object.values(mapping).flat())]
 const result=await execute('go',['test','-json','./internal/application','./internal/persistence','-run',`^(${unique.join('|')})$`])
 const events=result.output.split('\n').flatMap(line=>{try{return[JSON.parse(line)]}catch{return[]}})
 for(const scenario of suite.cases){
  const tests=mapping[scenario.id]??[]
  const missing=tests.filter(test=>!events.some(e=>e.Action==='pass'&&e.Test===test))
  const passed=tests.length>0&&missing.length===0
  if(!passed)failed++
  report.cases.push({id:scenario.id,title:scenario.title,status:passed?'regression_passed':'failed',tests,missing,behavior_quality:'not_evaluated'})
 }
 if(result.code!==0)failed++
 await writeFile(path.join(out,'go-events.jsonl'),result.output,{mode:0o600})
}else if(mode==='live'){
 if(args[0]==='--')args.shift()
 const [adapter,...adapterArgs]=args
 if(!adapter)throw new Error('live requires an adapter executable: node evals/agent/run.mjs live -- /path/to/adapter [args]. See evals/agent/README.md.')
 report.quality='evaluated_by_adapter_review'
 const jobs=suite.cases.flatMap(scenario=>Array.from({length:suite.trial_policy.real_model_trials_per_case},(_,index)=>({scenario,trial:index+1})))
 const databases=new Set();let cursor=0
 const worker=async()=>{while(cursor<jobs.length){
   const {scenario,trial}=jobs[cursor++]
   const input={schema_version:1,case:scenario,seed_recipe:suite.seed_recipes[scenario.seed],trial,require_isolated_database:true}
   const result=await execute(adapter,adapterArgs,JSON.stringify(input),300000)
   let verdict
   try{if(result.code!==0){await writeFile(path.join(out,`${scenario.id}-trial-${trial}-adapter-error.log`),result.error,{mode:0o600});throw new Error(`adapter exit ${result.code}`)};verdict=evaluateTrial(scenario,JSON.parse(result.output));if(databases.has(verdict.database_id)){verdict.passed=false;verdict.errors.push('database reused between trials')}if(verdict.database_id)databases.add(verdict.database_id)}catch(error){verdict={passed:false,errors:[String(error)]}}
   if(!verdict.passed)failed++
   report.cases.push({id:scenario.id,trial,...verdict})
   console.log(`${scenario.id} trial ${trial}: ${verdict.passed?'PASS':'FAIL'} quality=${verdict.quality_score??'unknown'}`)
   partialWrite=partialWrite.then(()=>writeFile(path.join(out,'report.partial.json'),JSON.stringify(report,null,2)+'\n',{mode:0o600}));await partialWrite
 }}
 await Promise.all(Array.from({length:3},worker))
 report.cases.sort((a,b)=>a.id.localeCompare(b.id)||a.trial-b.trial)
}else throw new Error('mode must be regression or live')
report.finished_at=new Date().toISOString();report.failed=failed
await writeFile(path.join(out,'report.json'),JSON.stringify(report,null,2)+'\n',{mode:0o600})
console.log(`${mode}: ${report.cases.length} results, ${failed} failures. ${path.join(out,'report.json')}`)
process.exitCode=failed?1:0
