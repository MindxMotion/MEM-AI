import { createInstance } from 'i18next'
import { afterEach, describe, expect, it, vi } from 'vitest'

import en from '@/i18n/locales/en.json'
import fr from '@/i18n/locales/fr.json'
import ja from '@/i18n/locales/ja.json'
import ru from '@/i18n/locales/ru.json'
import viLocale from '@/i18n/locales/vi.json'
import zhTW from '@/i18n/locales/zh-TW.json'
import zh from '@/i18n/locales/zh.json'

import {
  normalizeBasePath,
  joinPublicPath,
  validateSourceUrl,
} from '../public-path'

describe('public mount configuration', () => {
  it.each([
    ['', ''],
    ['/', ''],
    ['/new-api', '/new-api'],
    ['/new-api/', '/new-api'],
    ['/mem-ai/', '/mem-ai'],
    ['/a_B/9-x/', '/a_B/9-x'],
  ])('canonicalizes %j without changing its boundary', (input, expected) => {
    expect(normalizeBasePath(input)).toBe(expected)
  })

  describe('public mount consumers', () => {
    afterEach(() => {
      vi.unstubAllEnvs()
      vi.resetModules()
    })

    it.each(['', '/new-api', '/mem-ai'])(
      'mounts OAuth, affiliate and plugin browser URLs once for %j',
      async (prefix) => {
        vi.stubEnv('VITE_APP_BASE_PATH', prefix)
        vi.resetModules()
        const { publicSiteBase } = await import('../public-base')
        const { buildDiscordOAuthUrl, buildOIDCOAuthUrl } =
          await import('../oauth')
        const { generateAffiliateLink } =
          await import('@/features/wallet/lib/affiliate')
        const { pluginIconUrl, resolvePluginIcon } =
          await import('@/features/task-plugins/lib/plugin-icon')
        const base = `${window.location.origin}${prefix}`
        expect(publicSiteBase()).toBe(base)
        expect(
          new URL(buildDiscordOAuthUrl('fixture', 'fixture')).searchParams.get(
            'redirect_uri'
          )
        ).toBe(`${base}/oauth/discord`)
        expect(
          new URL(
            buildOIDCOAuthUrl(
              'https://provider.example/authorize',
              'fixture',
              'fixture'
            )
          ).searchParams.get('redirect_uri')
        ).toBe(`${base}/oauth/oidc`)
        expect(generateAffiliateLink('fixture')).toBe(
          `${base}/sign-up?aff=fixture`
        )
        expect(pluginIconUrl('fixture')).toBe('/api/plugin/task/fixture/icon')
        expect(resolvePluginIcon({ key: 'fixture', hasIcon: true })).toEqual({
          kind: 'image',
          src: `${prefix}/api/plugin/task/fixture/icon`,
        })
        expect(
          resolvePluginIcon({ key: 'fixture', iconSrc: 'blob:fixture' })
        ).toEqual({
          kind: 'image',
          src: 'blob:fixture',
        })
      }
    )
  })

  it.each([
    'new-api',
    ' /new-api',
    '/new-api ',
    '//host',
    'https://host/path',
    '/a//b',
    '/a//',
    '/a/../b',
    '/a/./b',
    '/a%2fb',
    '/a?b',
    '/a#b',
    '/a\\b',
    '/a\n',
    '/中文',
    '/a.b',
  ])('rejects invalid configuration %j rather than falling back', (input) => {
    expect(() => normalizeBasePath(input)).toThrow()
  })

  it.each([
    ['/api/status?x=%2F#part', '/new-api/api/status?x=%2F#part'],
    ['/new-api/api/status', '/new-api/api/status'],
    ['/new-api?x=1#part', '/new-api?x=1#part'],
    ['/new-api-other/logo.png', '/new-api/new-api-other/logo.png'],
    ['https://cdn.example/logo.png', 'https://cdn.example/logo.png'],
    ['//cdn.example/logo.png', '//cdn.example/logo.png'],
    ['blob:fixture', 'blob:fixture'],
    ['data:image/png;base64,AA', 'data:image/png;base64,AA'],
  ])(
    'resolves owned path %j once and preserves external sources',
    (input, expected) => {
      expect(joinPublicPath(input, '/new-api')).toBe(expected)
      expect(joinPublicPath(input, '')).toBe(input)
    }
  )

  it('accepts only a configured first-party corresponding-source archive', () => {
    expect(validateSourceUrl('', '/new-api')).toBe('')
    expect(validateSourceUrl('/new-api/source.tar.gz', '/new-api')).toBe(
      '/new-api/source.tar.gz'
    )
    expect(validateSourceUrl('/new-api/source/build.tar.gz', '/new-api')).toBe(
      '/new-api/source/build.tar.gz'
    )
    for (const input of [
      'https://host/source/a.tar.gz',
      '//host/source/a.tar.gz',
      '/source/a.tar.gz',
      '/new-api/source/../a.tar.gz',
      '/new-api/source/a.tar.gz?x=1',
    ]) {
      expect(() => validateSourceUrl(input, '/new-api')).toThrow()
    }
  })

  it('resolves source attribution in the actual translation namespaces', async () => {
    const resources = { en, fr, ja, ru, vi: viLocale, 'zh-TW': zhTW, zh }
    const i18n = createInstance()
    await i18n.init({ lng: 'en', resources, fallbackLng: false })
    for (const [language, resource] of Object.entries(resources)) {
      await i18n.changeLanguage(language)
      for (const key of ['Corresponding source', 'Modified build'] as const) {
        expect(i18n.exists(key)).toBe(true)
        expect(i18n.t(key)).toBe(resource.translation[key])
      }
    }
  })
})
