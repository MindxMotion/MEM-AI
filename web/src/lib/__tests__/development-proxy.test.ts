import { IncomingMessage } from 'node:http'
import { Socket } from 'node:net'

import { afterEach, expect, it, vi } from 'vitest'

import configure from '../../../rsbuild.config'

afterEach(() => vi.unstubAllEnvs())

it.each(['', '/new-api'])(
  'uses the installed proxy API to keep shell assets and HMR local for %j',
  (prefix) => {
    vi.stubEnv('VITE_APP_BASE_PATH', prefix)
    vi.stubEnv('VITE_CORRESPONDING_SOURCE_URL', '')
    const config = configure({
      env: 'development',
      envMode: 'development',
      command: 'dev',
    })
    const proxies = config.server?.proxy
    if (!Array.isArray(proxies)) throw new Error('Expected bounded proxy array')
    const proxy = proxies[0]
    const filter = proxy?.pathFilter
    const rewrite = proxy?.pathRewrite
    if (typeof filter !== 'function' || typeof rewrite !== 'function') {
      throw new Error('Expected native pathFilter and pathRewrite callbacks')
    }
    const request = new IncomingMessage(new Socket())
    for (const path of [
      '/api/status',
      '/v1/chat/completions',
      '/v1beta/models',
      '/mj/task',
      '/pg/task',
      '/fixture/mj/task',
    ]) {
      expect(filter(`${prefix}${path}`, request)).toBe(true)
    }
    for (const path of [
      '/',
      '/sign-in',
      '/static/js/index.js',
      '/rsbuild-hmr',
      '/apiary/status',
      '/v1-other/models',
    ]) {
      expect(filter(`${prefix}${path}`, request)).toBe(false)
    }
    if (prefix) expect(filter('/new-api-other/api/status', request)).toBe(false)
    expect(rewrite(`${prefix}/api/status?probe=1`, request)).toBe(
      '/api/status?probe=1'
    )
  }
)
