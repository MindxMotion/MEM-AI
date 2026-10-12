import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { defineConfig, loadEnv } from '@rsbuild/core'
import { pluginReact } from '@rsbuild/plugin-react'
import { pluginTailwindcss } from '@rsbuild/plugin-tailwindcss'
import { tanstackRouter } from '@tanstack/router-plugin/rspack'

import { normalizeBasePath, validateSourceUrl } from './src/lib/public-path'

const __dirname = path.dirname(fileURLToPath(import.meta.url))

export default defineConfig(({ envMode }) => {
  const env = loadEnv({ mode: envMode, prefixes: ['VITE_'] })
  const basePath = normalizeBasePath(
    process.env.VITE_APP_BASE_PATH ?? env.rawPublicVars.VITE_APP_BASE_PATH
  )
  const sourceUrl = validateSourceUrl(
    process.env.VITE_CORRESPONDING_SOURCE_URL ??
      env.rawPublicVars.VITE_CORRESPONDING_SOURCE_URL ??
      '',
    basePath
  )
  const assetBase = `${basePath}/`
  const serverUrl =
    process.env.VITE_REACT_APP_SERVER_URL ||
    env.rawPublicVars.VITE_REACT_APP_SERVER_URL ||
    'http://localhost:3000'

  const isProd = envMode === 'production'
  // Proxy runs before Rsbuild strips server.base. Match only relay/management
  // boundaries, never shell assets or HMR; the upstream receives root paths.
  const devProxy = [
    {
      context: (pathname: string) => {
        const unmounted = basePath ? pathname.slice(basePath.length) : pathname
        if (basePath && !pathname.startsWith(`${basePath}/`)) return false
        return (
          /^\/(?:api|v1|v1beta|mj|pg)(?:\/|$)/.test(unmounted) ||
          /^\/[^/]+\/mj(?:\/|$)/.test(unmounted)
        )
      },
      target: serverUrl,
      changeOrigin: true,
      ws: true,
      pathRewrite: (pathname: string) =>
        basePath ? pathname.slice(basePath.length) : pathname,
    },
  ]

  return {
    plugins: [pluginReact(), pluginTailwindcss({ optimize: false })],
    // Rsbuild 2: replaces deprecated `performance.chunkSplit` (RSPack 2 aligned)
    splitChunks: {
      preset: 'default',
      cacheGroups: {
        'vendor-react': {
          test: /node_modules[\\/](react|react-dom)[\\/]/,
          name: 'vendor-react',
          chunks: 'all',
          priority: 0,
          enforce: true,
        },
        'vendor-ui-primitives': {
          test: /node_modules[\\/](@base-ui|@radix-ui)[\\/]/,
          name: 'vendor-ui-primitives',
          chunks: 'all',
          priority: 0,
          enforce: true,
        },
        'vendor-tanstack': {
          test: /node_modules[\\/]@tanstack[\\/]/,
          name: 'vendor-tanstack',
          chunks: 'all',
          priority: 0,
          enforce: true,
        },
      },
    },
    source: {
      define: {
        ...env.publicVars,
        'import.meta.env.VITE_APP_BASE_PATH': JSON.stringify(basePath),
        'import.meta.env.VITE_CORRESPONDING_SOURCE_URL':
          JSON.stringify(sourceUrl),
      },
      entry: {
        index: './src/main.tsx',
      },
    },
    resolve: {
      alias: {
        '@': path.resolve(__dirname, './src'),
      },
    },
    html: {
      template: './index.html',
      favicon: './public/favicon.ico',
    },
    server: {
      base: assetBase,
      host: '0.0.0.0',
      strictPort: false,
      proxy: devProxy,
    },
    dev: {
      assetPrefix: assetBase,
      client: { path: `${basePath}/rsbuild-hmr` },
    },
    output: {
      assetPrefix: assetBase,
      // Production optimizations
      minify: isProd,
      target: 'web',
      distPath: {
        root: 'dist',
      },
      // Rely on Rsbuild default legalComments ("linked" → per-chunk *.LICENSE.txt) in all modes.
      // Do not set "none" in production: that strips minifier-preserved third-party notices and
      // extracted license files, which some distributions require for open-source compliance.
    },
    performance: {
      // Remove console in production
      removeConsole: isProd ? ['log'] : false,
      buildCache: false,
    },
    tools: {
      rspack: {
        plugins: [
          tanstackRouter({
            target: 'react',
            // Dev: avoid per-route async chunks (reduces white flash on navigation + faster HMR feedback).
            // Prod: keep route-based code splitting.
            autoCodeSplitting: isProd,
          }),
        ],
      },
    },
  }
})
