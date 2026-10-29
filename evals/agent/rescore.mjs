import { readFile,writeFile,mkdtemp,rm } from 'node:fs/promises'
import { spawn } from 'node:child_process'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { evaluateTrial } from './verify.mjs'
const folder=process.argv[2];if(!folder)throw new Error('usage: node evals/agent/rescore.mjs /path/to/completed-report')
const suite=JSON.parse(await readFile('docs/agent-harness/eval-cases.json','utf8'))
const report=JSON.parse(await readFile(path.join(folder,'report.json'),'utf8'));const work=await mkdtemp(path.join(tmpdir(),'fasttask-regrade-'))
try{
 for(const entry of report.cases){
  if(!entry.review_rationale?.startsWith('review unavailable:'))continue
  const source=path.join(folder,`${entry.id}-trial-${entry.trial}-evidence.json`),target=path.join(work,`${entry.id}-${entry.trial}.json`)
  const code=await new Promise((resolve,reject)=>{const child=spawn(process.env.FASTTASK_EVAL_BINARY??'/tmp/fasttask-agent-live.test',['-test.run=^TestAgentBehaviorReviewLive$','-test.timeout=100s'],{env:{...process.env,FASTTASK_AGENT_REVIEW_INPUT:source,FASTTASK_AGENT_LIVE_OUTPUT:target},stdio:['ignore','pipe','pipe']});child.stdout.resume();child.stderr.resume();child.once('error',reject);child.once('close',resolve)})
  if(code!==0){console.log(`${entry.id} trial ${entry.trial}: grader still unavailable`);continue}
  const result=JSON.parse(await readFile(target,'utf8'));const scenario=suite.cases.find(c=>c.id===entry.id);const verdict=evaluateTrial(scenario,result)
  entry.previous_unavailable_grade={quality_score:entry.quality_score,errors:entry.errors};Object.assign(entry,verdict);console.log(`${entry.id} trial ${entry.trial}: ${verdict.passed?'PASS':'FAIL'} quality=${verdict.quality_score}`)
 }
 report.failed=report.cases.filter(c=>!c.passed).length;report.grader_recovery_at=new Date().toISOString();await writeFile(path.join(folder,'report.json'),JSON.stringify(report,null,2)+'\n',{mode:0o600})
}finally{await rm(work,{recursive:true,force:true})}
