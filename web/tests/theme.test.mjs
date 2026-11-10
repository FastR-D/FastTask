import assert from 'node:assert/strict'
import { test } from 'node:test'
import { chromium, expect } from '@playwright/test'
import { preview } from 'vite'
import { mkdir, writeFile } from 'node:fs/promises'

test('production themes preserve the mounted assistant, draft, focus and PWA chrome', { timeout: 120000 }, async () => {
  const artifacts = 'test-results/fastui'
  await mkdir(artifacts, { recursive: true })
  const server = await preview({ configFile: false, preview: { host: '127.0.0.1', port: 0 } })
  const base = `http://127.0.0.1:${server.httpServer.address().port}`
  const browser = await chromium.launch({ headless: true })
  const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, serviceWorkers: 'block', colorScheme: 'light' })
  const errors = [], scenarios = []
  const user = { id: 'theme-user', identifier: 'theme-user', display_name: 'Theme user', timezone: 'UTC', role: 'admin', locale: 'zh-CN', status: 'active', revision: 1, created_at: '2026-11-10T00:00:00Z' }
  const token = `header.${Buffer.from(JSON.stringify({ sub: user.id, exp: 4102444800 })).toString('base64url')}.signature`
  await context.route('**/api/v1/**', async route => {
    const path = new URL(route.request().url()).pathname.replace('/api/v1', '')
    if (path === '/panel/profile') return route.fulfill({ status: 404, json: { detail: 'Profile not configured' } })
    let value = { items: [] }
    if (path === '/auth/refresh') value = { access_token: token, refresh_token: 'fixture-refresh', user }
    else if (path === '/me') value = user
    else if (path === '/agent/availability') value = { configured: true, mode: 'server', model_name: 'fixture-model' }
    else if (path === '/agent/thread-state') value = { state: null }
    else if (path === '/agent/threads') value = { items: [], next_cursor: null }
    else if (path === '/daily-plans/current') value = { plan: { id: 'theme-plan', local_date: new Intl.DateTimeFormat('en-CA', { timeZone: 'UTC', year: 'numeric', month: '2-digit', day: '2-digit' }).format(new Date()), timezone: 'UTC', status: 'draft', revision: 1, current_revision: 1 }, items: [], availability: { status: 'no_tasks', reason: '暂无任务', ready_count: 0, waiting_count: 0, can_replan: true } }
    else if (path === '/planning-settings') value = { daily_budget_minutes: 120, primary_goal_id: null }
    await route.fulfill({ json: value })
  })
  await context.addInitScript(() => {
    if (!localStorage.getItem('fasttask_refresh')) localStorage.setItem('fasttask_refresh', 'fixture-refresh')
  })
  try {
    const page = await context.newPage()
    page.on('pageerror', error => { errors.push(error.message); console.error(error.message) })
    await page.goto(base)
    const composer = page.locator('.agent-input')
    await expect(composer).toBeVisible()
    await composer.fill('保留这段研究草稿')
    const original = await composer.elementHandle()
    const peer = await context.newPage()
    await peer.goto(base)
    for (const visual of ['classic', 'workbench']) for (const mode of ['light', 'dark', 'system']) {
      await composer.focus()
      await peer.evaluate(({ visual, mode }) => {
        localStorage.setItem('fasttask.visual-theme', visual)
        localStorage.setItem('fasttask.theme', mode)
      }, { visual, mode })
      await expect(page.locator('html')).toHaveAttribute('data-ft-theme', visual)
      await expect(page.locator('html')).toHaveAttribute('data-fastui-theme', mode === 'dark' ? 'dark' : 'light')
      await expect(composer).toBeFocused()
      await expect(composer).toHaveValue('保留这段研究草稿')
      assert.equal(await original.evaluate(el => el === document.querySelector('.agent-input')), true)
      assert.equal(await page.evaluate(() => document.querySelector('meta[name="theme-color"]').content === getComputedStyle(document.documentElement).getPropertyValue('--fastui-page').trim()), true)
      await page.screenshot({ path: `${artifacts}/${visual}-${mode}.png` })
    }
    scenarios.push('classic/workbench × light/dark/system preserve assistant DOM, focus, draft and PWA color')
    await page.emulateMedia({ colorScheme: 'dark' })
    await expect(page.locator('html')).toHaveAttribute('data-fastui-theme', 'dark')
    await page.emulateMedia({ colorScheme: 'light' })
    await expect(page.locator('html')).toHaveAttribute('data-fastui-theme', 'light')
    await page.locator('.workbench-sidebar .theme-menu [slot="trigger"]').click()
    await page.locator('.workbench-sidebar .theme-menu mdui-menu-item[value="dark"]').click()
    await expect(page.locator('html')).toHaveAttribute('data-fastui-theme', 'dark')
    assert.equal(await page.evaluate(() => localStorage.getItem('fasttask.theme')), 'dark')
    scenarios.push('system media change and real MDUI theme menu persist existing storage key')
    await page.reload()
    await expect(page.locator('html')).toHaveAttribute('data-fastui-theme', 'dark')
    await expect(composer).toBeVisible()
    for (const width of [1440, 1024, 390]) {
      await page.setViewportSize({ width, height: 1000 })
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true)
      await page.screenshot({ path: `${artifacts}/workbench-${width}.png` })
    }
    // Hold the React entry at the network boundary: the blocking foundation boot must stand alone.
    for (const [stored, system, resolved] of [['light', 'dark', 'light'], ['dark', 'light', 'dark'], ['invalid', 'dark', 'dark'], ['blocked', 'dark', 'dark']]) {
      const cold = await browser.newContext({ colorScheme: system, serviceWorkers: 'block' })
      await cold.addInitScript(stored => {
        if (stored === 'blocked') {
          Object.defineProperty(Storage.prototype, 'getItem', { value() { throw new DOMException('blocked', 'SecurityError') } })
        } else {
          localStorage.setItem('fasttask.theme', stored)
          localStorage.setItem('fasttask.visual-theme', 'classic')
        }
      }, stored)
      await cold.route('**/assets/index-*.js', route => route.abort())
      const boot = await cold.newPage()
      await boot.goto(base)
      await expect(boot.locator('html')).toHaveAttribute('data-fastui-theme', resolved)
      await expect(boot.locator('html')).toHaveAttribute('data-ft-theme', stored === 'blocked' ? 'workbench' : 'classic')
      assert.equal(await boot.locator('#root').evaluate(el => el.childElementCount), 0)
      assert.equal(await boot.evaluate(() => document.querySelector('meta[name="theme-color"]').content === getComputedStyle(document.documentElement).getPropertyValue('--fastui-page').trim()), true)
      await cold.close()
    }
    scenarios.push('blocking pre-React boot honors pin, invalid preference, blocked storage and PWA color')
    assert.deepEqual(errors, [])
    await writeFile(`${artifacts}/results.json`, JSON.stringify({ status: 'passed', scenarios }, null, 2))
  } finally {
    await browser.close()
    await new Promise(resolve => server.httpServer.close(resolve))
  }
})
