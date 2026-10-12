import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import { createRequire } from 'node:module'
import test from 'node:test'

const require = createRequire(new URL('../../web/package.json', import.meta.url))
const { parse } = require('yaml')
const route = parse(await readFile(new URL('./ingress.yaml', import.meta.url), 'utf8'))
const entry = route.spec.http.find((rule) => rule.name === 'canonical-entry')
const application = route.spec.http.find((rule) => rule.name === 'application')

test('ingress owns only the named mount and canonicalizes GET/HEAD with queries', () => {
  assert.equal(route.metadata.namespace, 'new-api')
  assert.deepEqual(route.spec.http.flatMap((rule) => rule.match.paths), ['/new-api', '/new-api/*'])
  for (const rule of route.spec.http) {
    assert.deepEqual(rule.match.hosts, ['thedoorofai.com'])
    assert.equal(rule.backends[0].serviceName, 'new-api')
    assert.equal(rule.backends[0].servicePort, 3000)
  }
  assert.deepEqual(entry.match.methods, ['GET', 'HEAD'])
  assert.deepEqual(entry.plugins[0].config, {
    uri: 'https://thedoorofai.com/new-api/',
    ret_code: 308,
    append_query_string: true,
  })
})

test('mount stripping preserves resource, management and relay paths', () => {
  const [pattern, replacement] = application.plugins.find((plugin) => plugin.name === 'proxy-rewrite').config.regex_uri
  const expression = new RegExp(pattern)
  for (const [publicPath, upstreamPath] of [
    ['/new-api/', '/'],
    ['/new-api/api/user/auth/refresh', '/api/user/auth/refresh'],
    ['/new-api/static/js/index.js', '/static/js/index.js'],
    ['/new-api/v1/responses', '/v1/responses'],
    ['/new-api/source.tar.gz', '/source.tar.gz'],
  ]) {
    assert.equal(publicPath.replace(expression, replacement), upstreamPath)
  }
  for (const otherPath of ['/api', '/api/kaiwuai/mindmotion/health', '/new-api-other/api']) {
    assert.equal(expression.test(otherPath), false)
  }
  assert.equal(application.websocket, true)
  assert.equal(application.timeout.read, '360s')
})

test('only unmounted local Location headers are selected for prefix rewriting', () => {
  const configuration = application.plugins.find((plugin) => plugin.name === 'response-rewrite').config
  assert.deepEqual(configuration.vars[0].slice(0, 2), ['upstream_http_location', '~~'])
  assert.deepEqual(configuration.headers, { set: { Location: '/new-api$upstream_http_location' } })
  assert.equal(configuration.body, undefined)
  assert.equal(configuration.filters, undefined)
  const expression = new RegExp(configuration.vars[0][2])
  for (const location of ['/', '/api/token/', '/sign-in?redirect=%2Fdashboard']) {
    assert.equal(expression.test(location), true)
  }
  for (const location of [
    '/new-api', '/new-api/', '/new-api?x=1', '/new-api#top',
    'https://provider.example/authorize', '//provider.example/authorize', '',
  ]) {
    assert.equal(expression.test(location), false)
  }
})
