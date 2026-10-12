// Run against the exact locked frontend package, not a copied router algorithm.
import assert from 'node:assert/strict'
import path from 'node:path'
import { pathToFileURL } from 'node:url'

assert.ok(process.env.PROOF_WEB, 'Set PROOF_WEB to the locked web directory')
const entry = path.join(process.env.PROOF_WEB, 'node_modules/@tanstack/react-router/dist/esm/index.js')
const { createRouter, createRootRoute, createRoute, createMemoryHistory } = await import(pathToFileURL(entry).href)
for (const basepath of ['/', '/new-api']) {
  const root = createRootRoute()
  const home = createRoute({ getParentRoute: () => root, path: '/' })
  const login = createRoute({ getParentRoute: () => root, path: '/sign-in' })
  const dashboard = createRoute({ getParentRoute: () => root, path: '/dashboard' })
  const prefix = basepath === '/' ? '' : basepath
  const history = createMemoryHistory({ initialEntries: [`${prefix}/sign-in?redirect=%2Fdashboard`] })
  const router = createRouter({ routeTree: root.addChildren([home, login, dashboard]), basepath, history })
  await router.load()
  assert.equal(router.state.location.pathname, '/sign-in')
  assert.equal(router.state.location.href, '/sign-in?redirect=%2Fdashboard')
  await router.navigate({ href: '/dashboard?view=grid' })
  assert.equal(router.state.location.pathname, '/dashboard')
  assert.equal(history.location.href, `${prefix}/dashboard?view=grid`)
  await router.navigate({ href: `${prefix}/sign-in` })
  assert.equal(router.state.location.pathname, '/sign-in')
  assert.equal(history.location.href, `${prefix}/sign-in`)
  console.log(JSON.stringify({ basepath, internal: router.state.location.pathname, public: history.location.href, result: 'PASS' }))
}
