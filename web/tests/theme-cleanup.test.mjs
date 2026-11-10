import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { test } from 'node:test'

test('missing managed browser fails and exits without leaking the theme preview server', () => {
  const browsers = mkdtempSync(join(tmpdir(), 'fasttask-empty-browsers-'))
  try {
    // Execute the unchanged scenario in one Node process, with a genuinely empty browser cache.
    const env = { ...process.env, PLAYWRIGHT_BROWSERS_PATH: browsers }
    delete env.NODE_TEST_CONTEXT // The standalone child must not inherit the parent's binary reporter protocol.
    const result = spawnSync(process.execPath, ['--input-type=module', '--eval', `await import(${JSON.stringify(new URL('./theme.test.mjs', import.meta.url).href)})`], {
      cwd: fileURLToPath(new URL('../', import.meta.url)),
      env,
      encoding: 'utf8',
      timeout: 15000,
    })
    assert.ifError(result.error)
    assert.equal(result.signal, null, 'failure must exit naturally, without the watchdog killing it')
    assert.equal(result.status, 1, 'missing browser must remain a test failure')
    assert.match(result.stdout + result.stderr, /browserType\.launch/)
  } finally {
    rmSync(browsers, { recursive: true, force: true })
  }
})
