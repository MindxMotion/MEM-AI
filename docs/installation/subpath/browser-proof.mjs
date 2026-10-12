// Loopback-only feasibility fixture, not a backend authentication test.
import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { readFile } from 'node:fs/promises'
import http from 'node:http'
import path from 'node:path'
import { pathToFileURL } from 'node:url'

const dist = process.env.PROOF_DIST
const playwrightModule = process.env.PROOF_PLAYWRIGHT
const browserExecutable = process.env.PROOF_BROWSER
assert.ok(dist && playwrightModule && browserExecutable, 'Set PROOF_DIST, PROOF_PLAYWRIGHT and PROOF_BROWSER')
const prefix = process.env.PROOF_PREFIX || ''
const { chromium } = await import(pathToFileURL(playwrightModule).href)
const requests = []
let initialized = true
let footerHtml = ''
const expires = Math.floor(Date.now() / 1000) + 3600
const user = { id: 1, username: 'mount-proof', role: 100, status: 1, email: '', group: 'default' }
const bundle = {
  access_token: 'loopback-fixture-not-a-valid-credential',
  token_type: 'Bearer',
  access_expires_at: expires,
  user,
  session: {
    sid: 'loopback-mount-fixture',
    current: true,
    login_method: 'password',
    ip: '127.0.0.1',
    user_agent: 'mount-proof',
    created_at: expires - 3600,
    last_active_at: expires - 3600,
    expires_at: expires,
  },
}
const contentTypes = { '.js': 'text/javascript', '.css': 'text/css', '.html': 'text/html', '.svg': 'image/svg+xml', '.png': 'image/png', '.ico': 'image/x-icon', '.woff2': 'font/woff2' }
const server = http.createServer(async (req, res) => {
  const url = new URL(req.url, 'http://localhost')
  const mounted = !prefix || url.pathname === prefix || url.pathname.startsWith(`${prefix}/`)
  const pathname = mounted ? url.pathname.slice(prefix.length) || '/' : url.pathname
  requests.push({ method: req.method, path: url.pathname, mounted })
  if (!mounted) {
    res.writeHead(404).end('Outside proof mount')
    return
  }
  const json = (value, status = 200) => {
    res.writeHead(status, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' })
    res.end(JSON.stringify(value))
  }
  if (pathname === '/api/status') {
    json({ success: true, data: { system_name: 'Mount proof', password_login_enabled: true, register_enabled: true, footer_html: footerHtml, server_address: `http://127.0.0.1:${server.address().port}${prefix}` } })
    return
  }
  if (pathname === '/api/setup') {
    json({ success: true, data: { status: initialized, root_init: false, database_type: 'mysql' } })
    return
  }
  if (pathname === '/api/user/auth/refresh') {
    json({ success: false, code: 'AUTH_SESSION_INVALID', message: 'Unauthorized' }, 401)
    return
  }
  if (pathname === '/api/user/login') {
    // Request bodies and credential headers are intentionally not read or logged.
    json({ success: true, data: bundle })
    return
  }
  if (pathname === '/api/user/self') {
    json({ success: true, data: user })
    return
  }
  if (pathname === '/api/home_page_content' || pathname === '/api/notice') {
    json({ success: true, data: '' })
    return
  }
  if (pathname.startsWith('/api/')) {
    json({ success: true, data: [] })
    return
  }
  try {
    const candidate = path.resolve(dist, `.${pathname}`)
    assert.ok(candidate.startsWith(`${path.resolve(dist)}${path.sep}`))
    const body = await readFile(candidate)
    res.writeHead(200, { 'Content-Type': contentTypes[path.extname(candidate)] || 'application/octet-stream' })
    res.end(body)
  } catch {
    if (path.extname(pathname)) {
      res.writeHead(404).end('Missing proof asset')
      return
    }
    res.writeHead(200, { 'Content-Type': 'text/html', 'Cache-Control': 'no-store' })
    res.end(await readFile(path.join(dist, 'index.html')))
  }
})
await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve))
let browser
try {
  browser = await chromium.launch({ executablePath: browserExecutable, headless: true })
  const context = await browser.newContext({ locale: 'en-US' })
  await context.route('**/*', (route) => {
    const url = new URL(route.request().url())
    if (url.hostname !== '127.0.0.1') return route.abort()
    return route.continue()
  })
  const page = await context.newPage()
  const origin = `http://127.0.0.1:${server.address().port}`
  const consoleErrors = []
  page.on('pageerror', (error) => consoleErrors.push(error.name))
  await page.goto(`${origin}${prefix}/sign-in`)
  await page.locator('input[name="username"]').waitFor()
  await page.reload()
  await page.locator('input[name="username"]').waitFor()
  console.log(JSON.stringify({ scenario: 'login-reload', url: new URL(page.url()).pathname, browser: browser.version() }))
  await page.locator('input[name="username"]').fill('mount-proof')
  await page.locator('input[name="password"]').fill('loopback-fixture-only')
  await page.locator('form button[type="submit"]').first().click()
  await page.waitForURL(`${origin}${prefix}/dashboard`, { timeout: 15000 }).catch(async (error) => {
    console.log(JSON.stringify({ scenario: 'login-navigation-failure', url: new URL(page.url()).pathname, pageErrorNames: consoleErrors, text: (await page.locator('body').innerText()).slice(0, 600) }))
    throw error
  })
  console.log(JSON.stringify({ scenario: 'login-navigation', url: new URL(page.url()).pathname }))
  initialized = false
  await context.clearCookies()
  await page.goto(`${origin}${prefix}/setup`)
  await page.getByText('Database check', { exact: true }).first().waitFor().catch(async (error) => {
    console.log(JSON.stringify({ scenario: 'setup-failure', url: new URL(page.url()).pathname, text: (await page.locator('body').innerText()).slice(0, 600), pageErrorNames: consoleErrors, requests: requests.slice(-12) }))
    throw error
  })
  await page.reload()
  await page.getByText('Database check', { exact: true }).first().waitFor()
  console.log(JSON.stringify({ scenario: 'setup-reload', url: new URL(page.url()).pathname }))
  if (process.env.PROOF_SOURCE_ARCHIVE) {
    initialized = true
    const expected = createHash('sha256').update(await readFile(process.env.PROOF_SOURCE_ARCHIVE)).digest('hex')
    for (const custom of [false, true]) {
      footerHtml = custom ? '<p>Operator footer fixture</p>' : ''
      const footerContext = await browser.newContext({ locale: 'en-US' })
      try {
        await footerContext.route('**/*', (route) =>
          new URL(route.request().url()).hostname === '127.0.0.1'
            ? route.continue()
            : route.abort()
        )
        const footerPage = await footerContext.newPage()
        footerPage.on('pageerror', (error) => consoleErrors.push(error.name))
        await footerPage.goto(`${origin}${prefix}/`)
        const sourceLink = footerPage.getByRole('link', { name: 'Corresponding source', exact: true })
        await sourceLink.waitFor()
        assert.equal(await sourceLink.getAttribute('href'), `${prefix}/source.tar.gz`)
        assert.ok(await footerPage.getByRole('link', { name: 'New API', exact: true }).count(), 'Upstream attribution must remain visible')
        if (custom) await footerPage.getByText('Operator footer fixture', { exact: true }).waitFor()
        const sourceResponse = await footerContext.request.get(`${origin}${prefix}/source.tar.gz`)
        assert.equal(sourceResponse.status(), 200)
        assert.equal(createHash('sha256').update(await sourceResponse.body()).digest('hex'), expected)
        console.log(JSON.stringify({ scenario: custom ? 'custom-footer-source' : 'default-footer-source', sourceSha256: expected }))
      } finally {
        await footerContext.close()
      }
    }
  }
  const escaping = requests.filter((request) => !request.mounted)
  console.log(JSON.stringify({ prefix, requests, escaping, pageErrorNames: consoleErrors }))
  assert.equal(escaping.length, 0, 'Owned requests escaped mount')
  assert.equal(consoleErrors.length, 0, 'Browser page errors failed the mount proof')
} catch (error) {
  console.log(JSON.stringify({ scenario: 'mount-proof-failure', prefix, requests }))
  throw error
} finally {
  if (browser) await browser.close()
  await new Promise((resolve) => server.close(resolve))
}
