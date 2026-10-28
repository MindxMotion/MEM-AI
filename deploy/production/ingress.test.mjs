import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import { createRequire } from 'node:module'
import test from 'node:test'

const require = createRequire(new URL('../../web/package.json', import.meta.url))
const { parse } = require('yaml')
const route = parse(await readFile(new URL('./ingress.yaml', import.meta.url), 'utf8'))
const entry = route.spec.http.find((rule) => rule.name === 'canonical-entry')
const legacyEntry = route.spec.http.find((rule) => rule.name === 'legacy-entry')
const legacyConsole = route.spec.http.find((rule) => rule.name === 'legacy-console')
const application = route.spec.http.find((rule) => rule.name === 'application')

test('ingress owns only the canonical and legacy mounts and preserves project ownership', () => {
  assert.equal(route.metadata.namespace, 'new-api')
  assert.deepEqual(
    [...new Set(route.spec.http.flatMap((rule) => rule.match.paths))].sort(),
    ['/mem-ai', '/mem-ai/*', '/new-api', '/new-api/*']
  )
  for (const rule of route.spec.http) {
    assert.deepEqual(rule.match.hosts, ['gateway.example.com'])
    assert.equal(rule.backends[0].serviceName, 'new-api')
    assert.equal(rule.backends[0].servicePort, 3000)
  }
  assert.deepEqual(entry.match.methods, ['GET', 'HEAD'])
  assert.deepEqual(entry.plugins[0].config, {
    uri: 'https://gateway.example.com/mem-ai/',
    ret_code: 308,
    append_query_string: true,
  })
})

test('legacy console GET/HEAD redirects to the fixed canonical origin, preserving path and queries', () => {
  for (const rule of [legacyEntry, legacyConsole]) {
    assert.deepEqual(rule.match.methods, ['GET', 'HEAD'])
    assert.ok(rule.priority > (application.priority ?? 0))
    const config = rule.plugins.find((plugin) => plugin.name === 'redirect').config
    assert.equal(config.ret_code, 302)
    assert.equal(config.append_query_string, true)
    assert.deepEqual(rule.plugins.find((plugin) => plugin.name === 'response-rewrite').config.headers.set, {
      'Cache-Control': 'no-store',
      'Referrer-Policy': 'no-referrer',
    })
  }
  assert.equal(legacyEntry.plugins[0].config.uri, 'https://gateway.example.com/mem-ai/')
  const [pattern, replacement] = legacyConsole.plugins[0].config.regex_uri
  for (const [path, expected] of [
    ['/new-api/', '/mem-ai/'],
    ['/new-api/sign-in', '/mem-ai/sign-in'],
    ['/new-api/oauth/oidc', '/mem-ai/oauth/oidc'],
    ['/new-api/system-settings/auth/integration-clients', '/mem-ai/system-settings/auth/integration-clients'],
    ['/new-api/a%2Fb', '/mem-ai/a%2Fb'],
    ['/new-api//untrusted.example', '/mem-ai//untrusted.example'],
  ]) {
    const result = path.replace(new RegExp(pattern), replacement)
    const url = new URL(result)
    assert.equal(url.origin, 'https://gateway.example.com')
    assert.equal(url.pathname, expected)
  }
})

test('legacy management, model, streaming, WebSocket and artifact URLs never enter browser redirects', () => {
  const [{ subject, op, value }] = legacyConsole.match.exprs
  assert.deepEqual(subject, { scope: 'Path' })
  assert.equal(op, 'RegexNotMatch')
  const bypass = new RegExp(value)
  for (const path of [
    '/new-api/api/status', '/new-api/api/user/auth/refresh',
    '/new-api/api/integration/v1/token', '/new-api/api/oauth/oidc',
    '/new-api/v1/chat/completions', '/new-api/v1/responses', '/new-api/v1/realtime',
    '/new-api/v1beta/models/model:streamGenerateContent',
    '/new-api/mj/task/1/fetch', '/new-api/fast/mj/task/1/fetch',
    '/new-api/pg/chat/completions', '/new-api/dashboard/billing/usage',
    '/new-api/static/js/index.js', '/new-api/source.tar.gz',
  ]) assert.equal(bypass.test(path), true, path)
  for (const path of ['/new-api/', '/new-api/sign-in', '/new-api/oauth/oidc', '/new-api/api-other']) {
    assert.equal(bypass.test(path), false, path)
  }
  assert.equal(application.match.methods, undefined)
})

test('mount stripping preserves resource, management and relay paths', () => {
  const [pattern, replacement] = application.plugins.find((plugin) => plugin.name === 'proxy-rewrite').config.regex_uri
  const expression = new RegExp(pattern)
  for (const prefix of ['/new-api', '/mem-ai']) {
    for (const suffix of ['/', '/api/user/auth/refresh', '/static/js/index.js', '/v1/responses', '/source.tar.gz']) {
      assert.equal(`${prefix}${suffix}`.replace(expression, replacement), suffix)
    }
  }
  for (const otherPath of ['/api', '/api/kaiwuai/mindmotion/health', '/new-api-other/api', '/mem-ai-other/api']) {
    assert.equal(expression.test(otherPath), false)
  }
  assert.equal(application.websocket, true)
  assert.equal(application.timeout.read, '360s')
})

test('only unmounted local Location headers are selected for prefix rewriting', () => {
  const configuration = application.plugins.find((plugin) => plugin.name === 'response-rewrite').config
  assert.deepEqual(configuration.vars[0].slice(0, 2), ['upstream_http_location', '~~'])
  assert.deepEqual(configuration.headers, { set: { Location: '/mem-ai$upstream_http_location' } })
  assert.equal(configuration.body, undefined)
  assert.equal(configuration.filters, undefined)
  const expression = new RegExp(configuration.vars[0][2])
  for (const location of ['/', '/api/token/', '/sign-in?redirect=%2Fdashboard']) {
    assert.equal(expression.test(location), true)
  }
  for (const location of [
    '/new-api', '/new-api/', '/new-api?x=1', '/new-api#top',
    '/mem-ai', '/mem-ai/', '/mem-ai?x=1', '/mem-ai#top',
    'https://provider.example/authorize', '//provider.example/authorize', '',
  ]) {
    assert.equal(expression.test(location), false)
  }
})
