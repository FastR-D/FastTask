import assert from 'node:assert/strict';
import { writeFile } from 'node:fs/promises';
import path from 'node:path';

// Shared with the isolated Chromium fixture in gantt-browser.mjs.
export async function checkWorkbench({ command, evaluate, until, artifacts }) {
  const visible = selector => evaluate(`Boolean([...document.querySelectorAll(${JSON.stringify(selector)})].find(el=>el.getBoundingClientRect().width&&el.getBoundingClientRect().height))`);
  const settle = () => evaluate('new Promise(resolve=>setTimeout(resolve,250))');
  const resize = async width => { await command('Emulation.setDeviceMetricsOverride',{width,height:1000,deviceScaleFactor:1,mobile:width<600}); await settle(); };
  const screenshot = async name => { await command('Page.bringToFront'); const shot=await command('Page.captureScreenshot',{format:'png',captureBeyondViewport:false}); await writeFile(path.join(artifacts,`${name}.png`),Buffer.from(shot.data,'base64')); };
  // Use actual hit testing, including the custom elements' shadow buttons.
  const tap = async (selector,position = 'center') => {
    const point=await evaluate(`(()=>{const el=[...document.querySelectorAll(${JSON.stringify(selector)})].find(el=>{const r=el.getBoundingClientRect();return r.width&&r.height});if(!el)throw new Error('Missing visible target: '+${JSON.stringify(selector)});const r=el.getBoundingClientRect();return {x:r.x+(${JSON.stringify(position)}==='right'?r.width-8:r.width/2),y:r.y+r.height/2}})()`);
    await command('Input.dispatchMouseEvent',{type:'mousePressed',button:'left',clickCount:1,...point});
    await command('Input.dispatchMouseEvent',{type:'mouseReleased',button:'left',clickCount:1,...point});
    await settle();
  };
  const menu = async (kind,value) => {
    await tap(`.${kind}-menu > [slot="trigger"]`);
    await tap(`.${kind}-menu mdui-menu-item[value="${value}"]`);
  };
  await resize(1440);
  await until(()=>visible('.workbench-sidebar nav'),'sidebar missing');
  await until(()=>visible('.agent-input'),'assistant composer missing');
  for(const kind of ['visual-theme','theme']) {
    // Click the text and the empty right edge: both belong to the same full-row trigger.
    for(const target of [`.workbench-sidebar .${kind}-menu [slot="trigger"]>span`,`.workbench-sidebar .${kind}-menu [slot="trigger"]`]) {
      await tap(target,target.endsWith('>span')?'center':'right');
      assert.equal(await evaluate(`document.querySelector('.workbench-sidebar .${kind}-menu').open`),true,'appearance row did not open its menu');
      await command('Input.dispatchKeyEvent',{type:'keyDown',key:'Escape',code:'Escape',windowsVirtualKeyCode:27});
      await command('Input.dispatchKeyEvent',{type:'keyUp',key:'Escape',code:'Escape',windowsVirtualKeyCode:27});
      await settle();
    }
    await evaluate(`document.querySelector('.workbench-sidebar .${kind}-menu [slot="trigger"]').focus();true`);
    await command('Input.dispatchKeyEvent',{type:'keyDown',key:'Enter',code:'Enter',windowsVirtualKeyCode:13,text:'\r',unmodifiedText:'\r'});
    await command('Input.dispatchKeyEvent',{type:'keyUp',key:'Enter',code:'Enter',windowsVirtualKeyCode:13});
    await settle();
    assert.equal(await evaluate(`document.querySelector('.workbench-sidebar .${kind}-menu').open`),true,'keyboard did not open appearance menu');
    await command('Input.dispatchKeyEvent',{type:'keyDown',key:'Escape',code:'Escape',windowsVirtualKeyCode:27});
    await command('Input.dispatchKeyEvent',{type:'keyUp',key:'Escape',code:'Escape',windowsVirtualKeyCode:27});
    await settle();
  }

  assert.equal(await evaluate('document.documentElement.dataset.ftTheme'),'workbench');
  assert.equal(await evaluate('document.querySelector(".research-workspace").dataset.assistantOpen'),'true');
  await evaluate(`window.__composer=document.querySelector('.agent-input');window.__composer.value='保留这段研究草稿';window.__composer.dispatchEvent(new Event('input',{bubbles:true}));`);
  await tap('.workbench-assistant-toggle');
  assert.equal(await visible('.research-chat'),false);
  await tap('.workbench-assistant-toggle');
  assert.equal(await visible('.research-chat'),true);
  for (const visual of ['classic','workbench']) {
    await menu('visual-theme',visual);
    for (const color of ['light','dark','system']) {
      await menu('theme',color);
      assert.equal(await evaluate('document.documentElement.dataset.ftTheme'),visual);
      assert.equal(await evaluate('localStorage.getItem("fasttask.visual-theme")'),visual);
      assert.equal(await evaluate('document.documentElement.classList.contains('+JSON.stringify(`mdui-theme-${color==='system'?'auto':color}`)+')'),true);
      assert.equal(await evaluate('window.__composer===document.querySelector(".agent-input")&&window.__composer.value==="保留这段研究草稿"'),true,'theme switch reset composer');
      assert.equal(await evaluate('document.documentElement.scrollWidth<=innerWidth'),true);
      const inset=await evaluate('parseFloat(getComputedStyle(document.querySelector(".app-main")).paddingLeft)');
      assert.ok(visual==='workbench'?inset>=232:inset>0&&inset<232,`wrong layout inset ${visual}: ${inset}`);
      await screenshot(`${visual}-${color}-today`);
    }
  }
  await menu('theme','light');
  await command('Page.reload');
  await until(()=>visible('.workbench-sidebar nav'),'persisted workbench did not load');
  assert.equal(await evaluate('document.documentElement.dataset.ftTheme'),'workbench');
  // A real second page sends browser storage events, rather than synthetic events.
  const {targetId}=await command('Target.createTarget',{url:'about:blank',background:true});
  const {sessionId}=await command('Target.attachToTarget',{targetId,flatten:false});
  let nested=0;
  const other=async expression=>command('Target.sendMessageToTarget',{sessionId,message:JSON.stringify({id:++nested,method:'Runtime.evaluate',params:{expression}})});
  await other('location.href='+JSON.stringify(await evaluate('location.origin')));
  await settle();await settle();
  await other('localStorage.setItem("fasttask.visual-theme","classic")');
  await until(()=>evaluate('document.documentElement.dataset.ftTheme==="classic"'),'cross-tab visual theme sync failed');
  await other('localStorage.setItem("fasttask.theme","dark")');
  await until(()=>evaluate('document.documentElement.classList.contains("mdui-theme-dark")'),'cross-tab color sync failed');
  await other('localStorage.removeItem("fasttask.visual-theme");localStorage.removeItem("fasttask.theme")');
  await until(()=>evaluate('document.documentElement.dataset.ftTheme==="workbench"&&document.documentElement.classList.contains("mdui-theme-auto")'),'cross-tab default reset failed');
  await menu('theme','light');
  for (const width of [1440,1024,390]) {
    await resize(width);
    assert.equal(await evaluate('document.documentElement.scrollWidth<=innerWidth'),true,`today overflows at ${width}`);
    await screenshot(`workbench-${width}-today`);
    if(width<1080){
      await tap('.research-pane-switch button:nth-child(2)');
      assert.equal(await visible('.agent-input'),true);
      await screenshot(`workbench-${width}-assistant`);
      await tap('.research-pane-switch button:first-child');
    }
  }
  await tap('.app-bar mdui-button-icon[aria-label="打开导航"]');
  await until(()=>visible('.app-drawer .visual-theme-select'),'mobile theme selector missing');
  const order=await evaluate('document.querySelector(".visual-theme-select").getBoundingClientRect().top < document.querySelector(".theme-select").getBoundingClientRect().top');
  assert.equal(order,true,'visual selector must be above color selector');
  // Close using Escape, as a keyboard user would.
  await command('Input.dispatchKeyEvent',{type:'keyDown',key:'Escape',code:'Escape',windowsVirtualKeyCode:27});
  await command('Input.dispatchKeyEvent',{type:'keyUp',key:'Escape',code:'Escape',windowsVirtualKeyCode:27});
  await resize(1440);
  for(const [index,selector,name] of [[3,'.review-page','review'],[4,'.jobs-page','jobs']]) {
    await tap(`.workbench-sidebar nav button:nth-child(${index})`);
    await until(()=>visible(selector),`${name} missing`);
    await screenshot(`workbench-${name}`);
    await resize(390);
    assert.equal(await evaluate('document.documentElement.scrollWidth<=innerWidth'),true,`${name} overflows`);
    await screenshot(`workbench-mobile-${name}`);
    await resize(1440);
  }
  await tap('.workbench-sidebar-bottom>button:first-child');
  await screenshot('workbench-panel');
  await tap('.workbench-sidebar nav button:nth-child(2)');
  await until(()=>visible('.goal-index'),'goals missing');
  return { menu, screenshot, setVisual:async value=>{
    await other('localStorage.setItem("fasttask.visual-theme",'+JSON.stringify(value)+')');
    await until(()=>evaluate('document.documentElement.dataset.ftTheme==='+JSON.stringify(value)),'editor theme sync failed');
    await settle();
  }, closeOther:()=>command('Target.closeTarget',{targetId}) };
}
