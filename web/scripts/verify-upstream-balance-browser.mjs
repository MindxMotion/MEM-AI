import assert from 'node:assert/strict'
import { mkdir, readFile, rm } from 'node:fs/promises'
import { createServer } from 'node:http'
import path from 'node:path'
import { pathToFileURL } from 'node:url'

/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
// Run from web/. BALANCE_PLAYWRIGHT is a file URL to an existing playwright-core
// installation; BALANCE_EDGE is a local browser executable. No install or live API.
import { createRsbuild } from '@rsbuild/core'
import { pluginReact } from '@rsbuild/plugin-react'
import { pluginTailwindcss } from '@rsbuild/plugin-tailwindcss'

const { chromium } = await import(process.env.BALANCE_PLAYWRIGHT)
const root = process.cwd()
const rsbuild = await createRsbuild({
  rsbuildConfig: {
    plugins: [pluginReact(), pluginTailwindcss({ optimize: false })],
    source: {
      entry: {
        index:
          './src/features/channels/components/__tests__/upstream-balance.browser-fixture.tsx',
      },
      define: {
        'import.meta.env.VITE_APP_BASE_PATH': JSON.stringify('/new-api'),
        'import.meta.env.VITE_CORRESPONDING_SOURCE_URL': JSON.stringify(''),
      },
    },
    resolve: { alias: { '@': path.join(root, 'src') } },
    html: { template: './index.html' },
    output: {
      assetPrefix: '/new-api/',
      distPath: { root: '.balance-browser-dist' },
    },
  },
})
await rsbuild.build()
const known = {
  total: 120,
  used: 25,
  remaining: 95,
  unit: 'USD',
  scope: 'account',
  period: 'cumulative',
  precision: 2,
  checked_at: 1791975600,
  status: 'fresh',
  shared_account: true,
}
let snapshot = known,
  hold
const requests = []
const server = createServer(async (req, res) => {
  const pathname = new URL(req.url, 'http://localhost').pathname
  if (pathname.includes('/api/')) {
    requests.push(`${req.method} ${pathname}`)
    if (req.method === 'POST' && hold) {
      await hold
    }
    res.setHeader('Content-Type', 'application/json')
    res.end(JSON.stringify({ success: true, data: snapshot }))
    return
  }
  const relative = pathname.replace(/^\/new-api\/?/, '')
  const filename = relative.startsWith('static/') ? relative : 'index.html'
  try {
    const body = await readFile(
      path.join(root, '.balance-browser-dist', filename)
    )
    const mimeTypes = {
      '.js': 'text/javascript',
      '.css': 'text/css',
      '.woff2': 'font/woff2',
    }
    res.setHeader(
      'Content-Type',
      mimeTypes[path.extname(filename)] || 'text/html'
    )
    res.end(body)
  } catch {
    res.statusCode = 404
    res.end()
  }
})
await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve))
const origin = `http://127.0.0.1:${server.address().port}`
const browser = await chromium.launch({
  executablePath: process.env.BALANCE_EDGE,
  headless: true,
})
try {
  for (const theme of ['light', 'dark']) {
    for (const width of [375, 1280]) {
      const page = await browser.newPage({
        viewport: { width, height: 900 },
        reducedMotion: 'reduce',
      })
      const errors = []
      page.on('pageerror', (error) => errors.push(error.message))
      await page.goto(`${origin}/new-api/?theme=${theme}`)
      await page.getByRole('button', { name: 'Open quota' }).click()
      await page.getByText('Remaining: 95.00 USD', { exact: true }).waitFor()
      const bar = page.getByRole('progressbar')
      assert(
        Math.abs(
          Number(await bar.getAttribute('aria-valuenow')) - (25 / 120) * 100
        ) < 0.01
      )
      assert.equal(
        await page.getByText('Shared account; do not sum channels').count(),
        1
      )
      assert(
        await page.evaluate(
          () => document.documentElement.scrollWidth <= innerWidth
        )
      )
      if (process.env.BALANCE_SCREENSHOTS) {
        await mkdir(process.env.BALANCE_SCREENSHOTS, { recursive: true })
        await page.screenshot({
          path: path.join(
            process.env.BALANCE_SCREENSHOTS,
            `balance-${theme}-${width}.png`
          ),
        })
      }
      let release
      hold = new Promise((resolve) => {
        release = resolve
      })
      await page.getByRole('button', { name: 'Refresh', exact: true }).click()
      await page.getByText('Refreshing...', { exact: true }).waitFor()
      assert(
        await page
          .getByRole('button', { name: 'Refresh', exact: true })
          .isDisabled()
      )
      assert(
        await page
          .getByText('Remaining: 95.00 USD', { exact: true })
          .isVisible()
      )
      release()
      hold = null
      await page
        .getByText('Refreshing...', { exact: true })
        .waitFor({ state: 'hidden' })
      await page.keyboard.press('Escape')
      await page.getByRole('dialog').waitFor({ state: 'hidden' })
      assert.equal(
        await page.evaluate(() => document.activeElement?.textContent),
        'Open quota'
      )
      assert.deepEqual(errors, [])
      console.log(
        `PASS actual dialog ${theme} ${width}px: amounts, geometry, pending, overflow, Escape/focus`
      )
      await page.close()
    }
  }
  for (const [name, value, text, hasBar] of [
    [
      'unknown',
      {
        ...known,
        total: null,
        used: null,
        remaining: null,
        checked_at: 0,
        status: 'unknown',
      },
      'Unknown — refresh to check',
      false,
    ],
    [
      'zero',
      { ...known, total: 0, used: 0, remaining: 0 },
      'Remaining: 0.00 USD',
      false,
    ],
    [
      'overdrawn',
      { ...known, used: 125, remaining: -5 },
      'Remaining: -5.00 USD',
      true,
    ],
    [
      'stale',
      { ...known, status: 'stale', error: 'unavailable' },
      'Remaining: 95.00 USD',
      true,
    ],
    [
      'failure',
      {
        ...known,
        total: null,
        used: null,
        remaining: null,
        checked_at: 0,
        status: 'error',
        error: 'invalid_response',
      },
      'Unknown — refresh to check',
      false,
    ],
  ]) {
    snapshot = value
    const page = await browser.newPage({
      viewport: { width: 375, height: 900 },
    })
    await page.goto(`${origin}/new-api/?theme=dark&readonly`)
    await page.getByRole('button', { name: 'Open quota' }).click()
    await page.getByText(text, { exact: true }).waitFor()
    assert.equal(await page.getByRole('progressbar').count(), hasBar ? 1 : 0)
    assert(
      await page
        .getByRole('button', { name: 'Refresh', exact: true })
        .isDisabled()
    )
    if (name === 'overdrawn') {
      assert.equal(
        await page.getByRole('progressbar').getAttribute('aria-valuenow'),
        '100'
      )
      assert(await page.getByText('Overdrawn', { exact: true }).isVisible())
    }
    if (name === 'stale') {
      assert(
        (await page.getByRole('status').textContent()).includes(
          'last known values retained'
        )
      )
    }
    if (name === 'failure') {
      assert(
        (await page.getByRole('status').textContent()).includes(
          'Invalid upstream balance response'
        )
      )
    }
    assert(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth
      )
    )
    console.log(
      `PASS actual dialog ${name}: truthful values, bar availability, read-only`
    )
    await page.close()
  }
  assert(
    requests.every((request) =>
      /^GET \/new-api\/api\/channel\/11\/upstream_balance$|^POST \/new-api\/api\/channel\/11\/upstream_balance\/refresh$/.test(
        request
      )
    )
  )
  console.log(
    'PASS shared protected client subpath applied exactly once; only local mock requests'
  )
  const docPage = await browser.newPage({
    viewport: { width: 375, height: 900 },
  })
  const docRequests = []
  docPage.on('request', (request) => docRequests.push(request.url()))
  const packet = path.resolve(
    root,
    '../docs/design/channels/upstream-account-balance'
  )
  for (const name of ['design', 'ux', 'ui']) {
    await docPage.goto(pathToFileURL(path.join(packet, `${name}.html`)).href)
    assert.equal((await docPage.locator('h1,h2').count()) > 0, true)
    assert(
      await docPage.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth
      )
    )
  }
  for (const theme of ['light', 'dark']) {
    await docPage.locator('#theme').selectOption(theme)
    for (const state of [
      'known',
      'unknown',
      'zero',
      'overdrawn',
      'stale',
      'loading',
      'read-only',
      'failure',
    ]) {
      await docPage.locator('#state').selectOption(state)
      assert(
        await docPage.evaluate(
          () => document.documentElement.scrollWidth <= innerWidth
        )
      )
      if (['unknown', 'zero', 'failure'].includes(state)) {
        assert.equal(await docPage.locator('progress').count(), 0)
      }
    }
  }
  assert(docRequests.every((url) => url.startsWith('file:')))
  await docPage.close()
  console.log(
    'PASS offline technical/UX/UI packet, mobile overflow, theme/state controls; native diagram runtime remains separately pending'
  )
} finally {
  await browser.close()
  await new Promise((resolve) => server.close(resolve))
  await rm(path.join(root, '.balance-browser-dist'), {
    recursive: true,
    force: true,
  })
}
