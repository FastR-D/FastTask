import { readFile,mkdtemp,writeFile,rm } from 'node:fs/promises'
import { spawn } from 'node:child_process'
import { tmpdir } from 'node:os'
import path from 'node:path'
let input='';for await(const chunk of process.stdin)input+=chunk
const work=await mkdtemp(path.join(tmpdir(),'fasttask-live-'))
try{
 const source=path.join(work,'input.json'),target=path.join(work,'result.json')
 await writeFile(source,input,{mode:0o600})
 const binary=process.env.FASTTASK_EVAL_BINARY??'/tmp/fasttask-agent-live.test'
 const code=await new Promise((resolve,reject)=>{const child=spawn(binary,['-test.run=^TestAgentBehaviorLive$','-test.timeout=290s'],{cwd:process.cwd(),env:{...process.env,FASTTASK_AGENT_LIVE_INPUT:source,FASTTASK_AGENT_LIVE_OUTPUT:target},stdio:['ignore','pipe','pipe']});let output='';child.stdout.on('data',b=>{output=(output+b).slice(-4000)});child.stderr.on('data',b=>{output=(output+b).slice(-4000)});child.on('error',reject);child.on('close',code=>{if(code!==0)process.stderr.write(output);resolve(code)})})
 if(code!==0)process.exitCode=code??1
 else process.stdout.write(await readFile(target,'utf8'))
}finally{await rm(work,{recursive:true,force:true})}
