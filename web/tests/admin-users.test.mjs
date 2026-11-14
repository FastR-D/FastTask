import assert from 'node:assert/strict'
import { test } from 'node:test'
import { chromium, expect } from '@playwright/test'
import { preview } from 'vite'
import { mkdir } from 'node:fs/promises'

test('user list supports filtering, confirmed actions and revision conflicts', { timeout: 90000 }, async () => {
  let server, browser
  try {
    server = await preview({ configFile: false, preview: { host: '127.0.0.1', port: 0 } })
    browser = await chromium.launch({ headless: true })
    const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, serviceWorkers: 'block', colorScheme: 'light' })
    const current = { id: 'admin', identifier: 'admin', display_name: '平台管理员', role: 'admin', status: 'active', timezone: 'Asia/Shanghai', locale: 'zh-CN', revision: 1, created_at: '2026-11-10T00:00:00Z' }
    let users = [current, { ...current, id: 'alice', identifier: 'alice', display_name: '林晓', role: 'member', revision: 3 }, { ...current, id: 'bob', identifier: 'bob', display_name: '陈晨', role: 'member', status: 'disabled' }]
    const token = `header.${Buffer.from(JSON.stringify({ sub: current.id, exp: 4102444800 })).toString('base64url')}.signature`
    const mutations = [], searches = [], errors = []
    let failList = false, conflict = false
    await context.route('**/api/v1/**', async route => {
      const request = route.request(), url = new URL(request.url()), path = url.pathname.replace('/api/v1', ''), method = request.method()
      if (path === '/auth/refresh') return route.fulfill({ json: { access_token: token, refresh_token: 'fixture', user: current } })
      if (path === '/me') return route.fulfill({ json: current })
      if (path === '/admin/users' && method === 'GET') {
        searches.push(url.searchParams.get('q') || '')
        if (failList) return route.fulfill({ status: 503, json: { detail: '用户服务暂时不可用' } })
        const q = (url.searchParams.get('q') || '').toLowerCase(), status = url.searchParams.get('status')
        return route.fulfill({ json: { items: users.filter(user => `${user.identifier} ${user.display_name}`.toLowerCase().includes(q) && (!status || status === user.status)) } })
      }
      if (path.startsWith('/admin/users')) {
        const id = path.split('/')[3], user = users.find(user => user.id === id)
        if (method === 'GET') return route.fulfill({ json: { ...user, revision: 7 }, headers: { ETag: `"user_${id}_rev_7"` } })
        const body = request.postDataJSON()
        mutations.push({ method, path, body, etag: request.headers()['if-match'], key: request.headers()['idempotency-key'] })
        if (conflict && method === 'PATCH') return route.fulfill({ status: 412, json: { detail: 'revision conflict' } })
        if (method === 'DELETE') { users = users.filter(user => user.id !== id); return route.fulfill({ status: 204 }) }
        if (path.endsWith('/session-revocation')) return route.fulfill({ status: 204 })
        const updated = method === 'POST' ? { ...current, ...body, id: 'new-user' } : { ...user, ...body, revision: 8 }
        if (method === 'POST') users.push(updated)
        else users = users.map(user => user.id === id ? updated : user)
        return route.fulfill({ json: updated })
      }
      if (path === '/panel/profile') return route.fulfill({ status: 404, json: { detail: 'not configured' } })
      if (path === '/agent/availability') return route.fulfill({ json: { configured: false } })
      if (path === '/daily-plans/current') return route.fulfill({ json: { plan: { id: 'plan', local_date: '2026-11-13', timezone: 'Asia/Shanghai', status: 'draft', revision: 1 }, items: [], availability: { status: 'no_tasks', reason: '暂无任务', can_replan: true } } })
      if (path === '/planning-settings') return route.fulfill({ json: { daily_budget_minutes: 120, primary_goal_id: null } })
      return route.fulfill({ json: { items: [] } })
    })
    await context.addInitScript(() => {
      localStorage.setItem('fasttask_refresh', 'fixture')
      localStorage.setItem('fasttask.research-assistant.open', 'false')
    })
    const page = await context.newPage()
    page.on('pageerror', error => errors.push(error.message))
    await page.goto(`http://127.0.0.1:${server.httpServer.address().port}`)
    await page.locator('.workbench-sidebar nav').getByRole('button', { name: '用户管理' }).click()
    const table = page.getByRole('table', { name: '用户列表', exact: true })
    await expect(table.getByRole('row')).toHaveCount(4)
    await expect(page.getByRole('dialog')).toHaveCount(0)
    await expect(page.getByRole('button', { name: '删除 平台管理员', exact: true })).toBeDisabled()
    const search = page.getByRole('searchbox', { name: '搜索用户' })
    await search.fill('ALICE')
    await expect(table.getByRole('row')).toHaveCount(2)
    assert.ok(searches.includes('ALICE'), 'search must query the server beyond the initial list limit')
    await expect(table).toContainText('林晓')
    await page.getByRole('button', { name: '清除筛选', exact: true }).click()
    await page.getByLabel('账号状态筛选').selectOption('disabled')
    await expect(table.getByRole('row')).toHaveCount(2)
    await expect(table).toContainText('陈晨')
    await page.getByRole('button', { name: '清除筛选', exact: true }).click()
    await expect(table.getByRole('row')).toHaveCount(4)
    await search.fill('nobody')
    await expect(page.getByRole('heading', { name: '没有匹配的用户' })).toBeVisible()
    await search.fill('')
    await expect(table).toBeVisible()
    await page.getByRole('button', { name: '新增用户', exact: true }).click()
    let dialog = page.getByRole('dialog', { name: '新增用户', exact: true })
    await dialog.getByLabel('登录账号').fill('new-account')
    await dialog.getByLabel('姓名', { exact: true }).fill('新用户')
    await dialog.getByLabel('初始密码').fill('fixture-password-123')
    await dialog.getByLabel('状态', { exact: true }).selectOption('disabled')
    await dialog.getByRole('button', { name: '保存用户' }).click()
    await expect(dialog).toHaveCount(0)
    await expect(table).toContainText('新用户')
    assert.equal(mutations[0].body.status, 'disabled')
    assert.ok(mutations[0].key)
    await page.getByRole('button', { name: '编辑 林晓', exact: true }).click()
    dialog = page.getByRole('dialog', { name: '编辑用户', exact: true })
    await dialog.getByLabel('姓名', { exact: true }).fill('林晓 · 已更新')
    await dialog.getByRole('button', { name: '保存用户' }).click()
    await expect(table).toContainText('林晓 · 已更新')
    assert.equal(mutations[1].etag, '"user_alice_rev_7"', 'use fresh detail ETag')
    assert.deepEqual(mutations[1].body, { display_name: '林晓 · 已更新' }, 'unchanged role must not revoke sessions')
    conflict = true
    await page.getByRole('button', { name: '编辑 林晓 · 已更新', exact: true }).click()
    dialog = page.getByRole('dialog', { name: '编辑用户', exact: true })
    await dialog.getByLabel('姓名', { exact: true }).fill('保留未保存的输入')
    await dialog.getByRole('button', { name: '保存用户' }).click()
    await expect(dialog.getByRole('alert')).toContainText('其他管理员修改')
    await expect(dialog.getByLabel('姓名', { exact: true })).toHaveValue('保留未保存的输入')
    await dialog.getByRole('button', { name: '取消', exact: true }).click()
    conflict = false
    await page.getByRole('button', { name: '编辑 平台管理员', exact: true }).click()
    dialog = page.getByRole('dialog', { name: '编辑用户', exact: true })
    await expect(dialog.getByLabel('角色', { exact: true })).toBeDisabled()
    await expect(dialog.getByLabel('状态', { exact: true })).toBeDisabled()
    await dialog.getByRole('button', { name: '取消', exact: true }).click()
    const before = mutations.length
    await page.getByRole('button', { name: '撤销 陈晨 的会话', exact: true }).click()
    dialog = page.getByRole('dialog', { name: '撤销会话', exact: true })
    assert.equal(mutations.length, before)
    await dialog.getByRole('button', { name: '确认撤销会话' }).click()
    await expect(page.getByRole('status').filter({ hasText: '用户会话已撤销' })).toBeVisible()
    assert.equal(mutations.at(-1).path, '/admin/users/bob/session-revocation')
    await page.getByRole('button', { name: '删除 新用户', exact: true }).click()
    dialog = page.getByRole('dialog', { name: '删除用户', exact: true })
    await dialog.getByRole('button', { name: '取消', exact: true }).click()
    await expect(table).toContainText('新用户')
    await page.getByRole('button', { name: '删除 新用户', exact: true }).click()
    dialog = page.getByRole('dialog', { name: '删除用户', exact: true })
    await dialog.getByRole('button', { name: '确认删除用户' }).click()
    await expect(table).not.toContainText('新用户')
    assert.equal(mutations.at(-1).etag, '"user_new-user_rev_7"')
    failList = true
    await page.getByRole('button', { name: '刷新', exact: true }).click()
    await expect(page.getByRole('alert')).toContainText('用户服务暂时不可用')
    await expect(page.getByRole('heading', { name: '暂时无法加载用户' })).toBeVisible()
    failList = false
    await page.getByRole('button', { name: '重新加载', exact: true }).click()
    await expect(table).toBeVisible()
    await mkdir('test-results/admin-users', { recursive: true })
    for (const mode of ['light', 'dark']) {
      await page.evaluate(mode => window.fastTaskColorMode.setMode(mode), mode)
      for (const width of [1440, 390]) {
        await page.setViewportSize({ width, height: 900 })
        await expect(table).toBeVisible()
        assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, `${mode} ${width} must not overflow`)
        await page.screenshot({ path: `test-results/admin-users/${mode}-${width}.png`, fullPage: true })
      }
    }
    await page.getByRole('button', { name: '新增用户', exact: true }).click()
    dialog = page.getByRole('dialog', { name: '新增用户', exact: true })
    await expect(dialog.getByLabel('登录账号')).toBeFocused()
    await page.keyboard.press('Escape')
    await expect(dialog).toHaveCount(0)
    await expect(page.getByRole('button', { name: '新增用户', exact: true })).toBeFocused()
    assert.deepEqual(errors, [])
  } finally {
    await browser?.close()
    await server?.close()
  }
})
