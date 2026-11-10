// Run after `npm run build` and `go build -o bin/fasttask ./cmd/fasttask`.
// Uses an isolated database and an actual Chromium; never connects to production.
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { mkdtemp, mkdir, writeFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { randomBytes, randomUUID } from 'node:crypto';
import net from 'node:net';
import { checkWorkbench } from './workbench-browser-checks.mjs';

const root = fileURLToPath(new URL('../../', import.meta.url));
const work = await mkdtemp(path.join(tmpdir(), 'fasttask-gantt-browser-'));
const artifacts = process.env.GANTT_ARTIFACTS ?? path.join(tmpdir(), 'fasttask-gantt-browser-artifacts');
await mkdir(artifacts, {recursive:true});
let server, browser, ws, sequence = 0;
const pending = new Map(), errors = [];
const wait = ms => new Promise(resolve => setTimeout(resolve, ms));
async function until(check, message, timeout = 15000) {
  const end = Date.now() + timeout;
  while (Date.now() < end) { if (await check()) return; await wait(100); }
  throw new Error(message);
}
async function stop(child) {
  if (!child || child.exitCode !== null) return;
  child.kill('SIGTERM');
  await Promise.race([new Promise(resolve => child.once('exit', resolve)), wait(2000)]);
  if (child.exitCode === null) child.kill('SIGKILL');
}
try {
  const listener = net.createServer();
  await new Promise(resolve => listener.listen(0, '127.0.0.1', resolve));
  const port = listener.address().port;
  await new Promise(resolve => listener.close(resolve));
  const origin = `http://127.0.0.1:${port}`, password = randomBytes(20).toString('hex');
  const env = Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith('FASTTASK_') && !key.startsWith('OPENAI_')));
  Object.assign(env, {FASTTASK_ENV:'development', FASTTASK_PORT:String(port), FASTTASK_LISTEN:'127.0.0.1', FASTTASK_PUBLIC_URL:origin,
    FASTTASK_DATABASE:path.join(work,'test.db'), FASTTASK_WEB_DIST:path.join(root,'web/dist'), FASTTASK_ADMIN_USER:'gantt-browser',
    FASTTASK_ADMIN_PASSWORD:password, FASTTASK_JWT_SECRET:randomBytes(32).toString('hex'), FASTTASK_NOTIFICATION_ENABLED:'false'});
  let serverLog = '';
  server = spawn(process.env.FASTTASK_TEST_BINARY ?? path.join(root,'bin/fasttask'), ['serve','--with-worker=false','--with-scheduler=false'], {cwd:work,env,stdio:['ignore','ignore','pipe']});
  server.stderr.on('data', bytes => { serverLog = (serverLog + bytes).slice(-10000); });
  server.on('error', error => errors.push(error.message));
  await until(async () => { try { return (await fetch(`${origin}/health/ready`)).ok; } catch { return false; } }, `server did not start: ${serverLog}`);
  let access;
  async function api(method, resource, body, etag) {
    const response = await fetch(`${origin}/api/v1${resource}`, {method,headers:{'Content-Type':'application/json', ...(access?{Authorization:`Bearer ${access}`} : {}), 'Idempotency-Key':randomUUID(), ...(etag?{'If-Match':etag}:{})},body:body?JSON.stringify(body):undefined});
    const value = await response.json();
    assert.ok(response.ok, `${method} ${resource}: ${response.status} ${JSON.stringify(value)}`);
    return {value,etag:response.headers.get('ETag')};
  }
  const login = (await api('POST','/auth/login',{identifier:'gantt-browser',password})).value;
  access = login.access_token;
  const today = new Intl.DateTimeFormat('en-CA',{timeZone:login.user.timezone,year:'numeric',month:'2-digit',day:'2-digit'}).format(new Date());
  const month = today.slice(0,7), day = n => `${month}-${String(n).padStart(2,'0')}`;
  const goal = (await api('POST','/goals',{title:'甘特图浏览器验收',success_criteria:'完成阶段计划',target_date:day(18)})).value;
  async function task(title, extra = {}) {
    return (await api('POST','/tasks',{goal_id:goal.id,title,type:'task',description:'',success_criteria:'可验证成果',minimum_action:'开始一个步骤',estimate_minutes:40,priority:70,position:0,...extra})).value;
  }
  const phase = await task('文献调研阶段',{type:'milestone'});
  const prior = await task('文献检索',{parent_id:phase.id,planned_start:day(2),planned_end:day(10)});
  const next = await task('整理研究现状',{parent_id:phase.id,depends_on:[prior.id],planned_start:day(8),planned_end:day(20)});
  await task('待排期任务',{parent_id:phase.id});
  const second = (await api('POST','/goals',{title:'另一个目标',success_criteria:'单独查看'})).value;
  // Browser refresh rotation owns a separate session from the API test client.
  const browserLogin = (await api('POST','/auth/login',{identifier:'gantt-browser',password})).value;

  browser = spawn(process.env.CHROME_BIN ?? '/usr/bin/chromium', ['--headless','--no-sandbox','--disable-gpu','--disable-dev-shm-usage','--disable-background-networking','--remote-debugging-port=0',`--user-data-dir=${path.join(work,'profile')}`,'about:blank'], {stdio:['ignore','ignore','pipe']});
  const endpoint = await new Promise((resolve,reject) => {
    let log=''; const timer=setTimeout(()=>reject(new Error('Chromium startup timed out')),10000);
    browser.once('error',reject);
    browser.stderr.on('data',bytes=>{log=(log+bytes).slice(-12000); const match=log.match(/DevTools listening on (ws:\/\/\S+)/); if(match){clearTimeout(timer);resolve(match[1]);}});
  });
  const targets = await (await fetch(`${new URL(endpoint).origin.replace('ws:','http:')}/json/list`)).json();
  ws = new WebSocket(targets.find(target=>target.type==='page').webSocketDebuggerUrl);
  await new Promise((resolve,reject)=>{ws.addEventListener('open',resolve,{once:true});ws.addEventListener('error',reject,{once:true});});
  ws.addEventListener('message',event=>{
    const message=JSON.parse(event.data);
    if(message.method==='Runtime.exceptionThrown') errors.push(message.params.exceptionDetails.text+': '+(message.params.exceptionDetails.exception?.description??''));
    const job=pending.get(message.id);if(!job)return;clearTimeout(job.timer);pending.delete(message.id);
    if(message.error)job.reject(new Error(JSON.stringify(message.error)));else job.resolve(message.result);
  });
  const command=(method,params={})=>new Promise((resolve,reject)=>{const id=++sequence;const timer=setTimeout(()=>{pending.delete(id);reject(new Error(`${method} timed out`));},15000);pending.set(id,{resolve,reject,timer});ws.send(JSON.stringify({id,method,params}));});
  const evaluate=async expression=>{const result=await command('Runtime.evaluate',{expression,awaitPromise:true,returnByValue:true});assert.equal(result.exceptionDetails,undefined,JSON.stringify(result.exceptionDetails));return result.result?.value;};
  const click=selector=>evaluate(`(()=>{const el=document.querySelector(${JSON.stringify(selector)});if(!el)throw new Error('Missing click target');el.click();return true})()`);
  const button=text=>evaluate(`(()=>{const el=[...document.querySelectorAll('mdui-button,button')].find(el=>el.textContent.trim()===${JSON.stringify(text)});if(!el)throw new Error('Missing button');el.click();return true})()`);
  const text=()=>evaluate('document.body?.innerText ?? ""');
  const field=(label,value)=>evaluate(`(()=>{const el=document.querySelector('mdui-dialog[open] mdui-text-field[label="'+${JSON.stringify(label)}+'"]');if(!el)throw new Error('Missing field');el.value=${JSON.stringify(value)};el.dispatchEvent(new Event('input',{bubbles:true,composed:true}));return true})()`);
  const dialogClosed=()=>until(()=>evaluate("!document.querySelector('mdui-dialog[open]')"),'editor did not close');
  const openTask=title=>evaluate(`(()=>{const el=[...document.querySelectorAll('.gantt-task-name')].find(el=>el.querySelector('b').textContent===${JSON.stringify(title)});if(!el)throw new Error('Missing task');el.click();return true})()`);
  await command('Runtime.enable');await command('Page.enable');
  await command('Page.addScriptToEvaluateOnNewDocument',{source:`if(location.origin===${JSON.stringify(origin)}&&!localStorage.getItem('fasttask_refresh')){localStorage.setItem('fasttask_refresh',${JSON.stringify(browserLogin.refresh_token)});localStorage.setItem('fasttask_offline_user',${JSON.stringify(JSON.stringify(browserLogin.user))});}`});
  await command('Page.navigate',{url:origin});
  await until(()=>evaluate('Boolean(document.querySelector(".research-workspace"))'),'workspace did not load');
  const shell=await checkWorkbench({ command, evaluate, until, artifacts });
  await until(()=>evaluate('document.querySelectorAll(".goal-index>button").length===2'),'goals did not load');
  await evaluate(`(()=>{[...document.querySelectorAll('.goal-index>button')].find(el=>el.textContent.includes('甘特图浏览器验收')).click();return true})()`);
  await shell.screenshot('workbench-goal-tree');
  await button('目标地图');await shell.screenshot('workbench-goal-map');
  await button('甘特图');
  await until(()=>evaluate('document.querySelectorAll(".gantt-row").length===4'),'gantt rows did not load');
  await evaluate('window.__gantt=document.querySelector(".gantt-view");true');
  await shell.menu('visual-theme','classic');await shell.menu('visual-theme','workbench');
  assert.equal(await evaluate('window.__gantt===document.querySelector(".gantt-view")'),true,'theme switch remounted gantt');
  assert.ok((await text()).includes('自动汇总'));
  assert.ok((await text()).includes('1 项子任务未排期'));
  assert.equal(await evaluate('document.querySelectorAll(".gantt-dependencies>path").length'),1);
  await click('button[aria-label="收起文献调研阶段"]');
  assert.equal(await evaluate('document.querySelectorAll(".gantt-row").length'),1);
  await click('button[aria-label="展开文献调研阶段"]');
  await click('mdui-button-icon[aria-label="下个月"]');
  assert.equal(await evaluate('document.querySelectorAll(".gantt-bar").length'),0);
  await button('回到今天');
  assert.equal(await evaluate('document.querySelectorAll(".gantt-bar").length'),3);
  await openTask('文献调研阶段');
  await until(()=>evaluate('Boolean(document.querySelector("mdui-dialog[open]"))'),'editor did not open');
  await field('任务标题','未提交的阶段标题');
  await evaluate('window.__editor=document.querySelector("mdui-dialog[open]");true');
  await shell.setVisual('classic');await shell.setVisual('workbench');
  assert.equal(await evaluate('window.__editor===document.querySelector("mdui-dialog[open]")'),true,'theme switch remounted editor');
  assert.equal(await evaluate(`document.querySelector('mdui-dialog[open] mdui-text-field[label="任务标题"]').value`),'未提交的阶段标题');
  await field('任务标题','文献调研阶段');
  await shell.closeOther();
  await field('计划开始日期',day(1));
  assert.ok((await text()).includes('请同时填写'));
  assert.equal(await evaluate('[...document.querySelectorAll("mdui-dialog[open] mdui-button")].find(el=>el.textContent.trim()==="保存").disabled'),true);
  await field('计划结束日期',day(12));
  await button('保存');await dialogClosed();
  await until(()=>evaluate(`document.querySelector('.gantt-range')?.textContent.includes(${JSON.stringify(day(12))})`),'manual stage dates were not refreshed');
  let saved=(await api('GET',`/tasks/${phase.id}`)).value;
  assert.equal(saved.planned_end,day(12));
  await click('button[aria-label="查看整理研究现状的依赖与日期提示"]');
  const details=await evaluate('document.querySelector(".gantt-detail").innerText');
  assert.match(details,/超过目标日期/);assert.match(details,/超出「文献调研阶段」/);assert.match(details,/前置「文献检索」/);
  await openTask('文献调研阶段');await button('清空计划日期');await button('保存');await dialogClosed();
  await until(()=>evaluate('document.querySelector(".gantt-range")?.textContent.includes("自动汇总")'),'clearing dates did not restore aggregation');
  saved=(await api('GET',`/tasks/${phase.id}`)).value;
  assert.equal(saved.planned_start,undefined);assert.equal(saved.planned_end,undefined);

  // Another client changes the same date while this editor is open.
  await openTask('文献检索');
  const baseline=await api('GET',`/tasks/${prior.id}`);
  await api('PATCH',`/tasks/${prior.id}`,{planned_end:day(11)},baseline.etag);
  await field('计划结束日期',day(9));await button('保存');
  await until(async()=> (await text()).includes('内容已在另一处更新'),'stale edit did not show conflict');
  assert.equal((await api('GET',`/tasks/${prior.id}`)).value.planned_end,day(11));
  await button('载入最新内容');
  assert.equal(await evaluate(`document.querySelector('mdui-dialog[open] mdui-text-field[label="计划结束日期"]').value`),day(11));
  await button('取消');await dialogClosed();
  await button('添加阶段');
  assert.equal(await evaluate(`document.querySelector('mdui-dialog[open] mdui-select[label="类型"]').value`),'milestone');
  await field('任务标题','实验验证阶段');await field('任务完成标准','保留实验结果');await field('最小行动','准备实验');
  await field('计划开始日期',day(21));await field('计划结束日期',day(25));
  await button('保存');await dialogClosed();
  await until(()=>evaluate('document.querySelectorAll(".gantt-row").length===5'),'new phase did not appear');
  const created=(await api('GET',`/tasks?goal_id=${goal.id}`)).value.items.find(task=>task.title==='实验验证阶段');
  assert.equal(created.type,'milestone');assert.equal(created.planned_start,day(21));

  await command('Emulation.setDeviceMetricsOverride',{width:1440,height:1000,deviceScaleFactor:1,mobile:false});
  await evaluate('document.querySelector(".gantt-view").scrollIntoView({block:"start"})');
  const desktop=await command('Page.captureScreenshot',{format:'png',captureBeyondViewport:false});
  await writeFile(path.join(artifacts,'gantt-desktop.png'),Buffer.from(desktop.data,'base64'));
  await command('Emulation.setDeviceMetricsOverride',{width:390,height:844,deviceScaleFactor:1,mobile:true});
  await evaluate('document.querySelector(".gantt-scroll").scrollIntoView({block:"center"})');
  const before=await evaluate('document.querySelector(".gantt-label-main").getBoundingClientRect().left');
  await evaluate('document.querySelector(".gantt-scroll").scrollLeft=400');
  const after=await evaluate('document.querySelector(".gantt-label-main").getBoundingClientRect().left');
  assert.ok(Math.abs(before-after)<1,'mobile task labels did not stay fixed');
  assert.equal(await evaluate('document.documentElement.scrollWidth<=window.innerWidth'),true,'page overflows mobile viewport');
  const mobile=await command('Page.captureScreenshot',{format:'png',captureBeyondViewport:false});
  await writeFile(path.join(artifacts,'gantt-mobile.png'),Buffer.from(mobile.data,'base64'));
  await command('Emulation.clearDeviceMetricsOverride');
  await evaluate(`(()=>{[...document.querySelectorAll('.goal-index>button')].find(el=>el.textContent.includes('另一个目标')).click();return true})()`);
  await until(()=>evaluate('document.querySelector(".tree-head h2")?.textContent==="另一个目标"'),'goal switch failed');
  await button('甘特图');
  await until(async()=> (await text()).includes('从一个阶段开始规划'),'empty goal state missing');
  assert.equal(await evaluate('document.querySelectorAll(".gantt-row").length'),0);
  await command('Emulation.setDeviceMetricsOverride',{width:1440,height:1000,deviceScaleFactor:1,mobile:false});
  await evaluate('document.querySelector(".workbench-sidebar-bottom>button:last-child").click()');
  await until(()=>evaluate('Boolean(document.querySelector(".login-card"))'),'logout did not show login');
  await shell.screenshot('workbench-login');
  assert.equal(await evaluate('getComputedStyle(document.querySelector(".login-story")).backgroundColor'),'rgb(35, 45, 61)');
  await command('Emulation.setDeviceMetricsOverride',{width:390,height:844,deviceScaleFactor:1,mobile:true});
  assert.equal(await evaluate('document.documentElement.scrollWidth<=innerWidth'),true,'login overflows');
  await shell.screenshot('workbench-mobile-login');
  assert.deepEqual(errors,[]);
  console.log(JSON.stringify({passed:true,checks:['workbench and classic × three color modes','persistent theme and cross-tab sync','assistant collapse and retained draft','responsive today/review/jobs and mobile theme entry','stage creation','manual and derived dates','unscheduled tasks','inclusive bars','dependencies and warnings','collapse','month navigation','date validation','save and clear','concurrent edit protection','mobile sticky labels','goal isolation'],artifacts},null,2));
} finally {
  for(const job of pending.values())clearTimeout(job.timer);
  ws?.close();await stop(browser);await stop(server);
  await rm(work,{recursive:true,force:true,maxRetries:5});
}
