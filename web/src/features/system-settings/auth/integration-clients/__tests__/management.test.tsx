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
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { act, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { api, type ApiRequestConfig } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import type { IntegrationClient, IntegrationClientList } from '../api'
import { IntegrationClientsSection } from '../integration-clients-section'

const client: IntegrationClient = {
  id: 1,
  client_id: 'nac_profile_cards',
  name: 'Profile cards',
  secret_hint: 'abcd',
  oidc_issuer: 'https://identity.example.test',
  oidc_client_id: 'gateway',
  enabled: true,
  created_at: 1700000000,
  expires_at: 4102444800,
  last_used_at: 0,
}

let list: IntegrationClientList
let failList: boolean

beforeEach(() => {
  useAuthStore.getState().auth.setUser({ id: 1, username: 'root', role: 100 })
  failList = false
  list = {
    items: [],
    oidc_configured: true,
    oidc_issuer: client.oidc_issuer,
    oidc_client_id: client.oidc_client_id,
  }
  vi.spyOn(api, 'get').mockImplementation(
    async (url, config: ApiRequestConfig | undefined) => {
      if (url === '/api/integration-clients') {
        if (failList) throw new Error('offline')
        return { data: { success: true, data: list } }
      }
      if (url === '/api/verify/methods') {
        return {
          data: {
            success: true,
            data: {
              scope: config?.params?.scope,
              methods: [{ method: 'password', available: true }],
              oauth_providers: [],
              password_encryption_enabled: false,
            },
          },
        }
      }
      throw new Error(`Unexpected GET ${url}`)
    }
  )
  vi.spyOn(api, 'post').mockImplementation(async (url, data) => {
    if (url === '/api/verify') {
      return {
        data: {
          success: true,
          data: {
            proof_token: 'one-use-proof',
            scope: (data as { scope: string }).scope,
            method: 'password',
            expires_at: Math.floor(Date.now() / 1000) + 60,
          },
        },
      }
    }
    if (url === '/api/integration-clients') {
      list = { ...list, items: [client] }
      return {
        data: {
          success: true,
          data: { item: client, client_secret: 'nacs_one-time-secret' },
        },
      }
    }
    if (url === '/api/integration-clients/nac_profile_cards/rotate') {
      return {
        data: {
          success: true,
          data: {
            client_id: client.client_id,
            client_secret: 'nacs_rotated-secret',
          },
        },
      }
    }
    throw new Error(`Unexpected POST ${url}`)
  })
  vi.spyOn(api, 'patch').mockResolvedValue({
    data: { success: true, data: { enabled: false } },
  })
})

afterEach(() => {
  useAuthStore.getState().auth.reset()
  vi.restoreAllMocks()
})

function renderManager() {
  const cache = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  render(
    <QueryClientProvider client={cache}>
      <IntegrationClientsSection />
    </QueryClientProvider>
  )
  return cache
}

async function verify(user: ReturnType<typeof userEvent.setup>) {
  await user.type(
    await screen.findByLabelText('Password', { selector: 'input' }),
    'current-password'
  )
  await user.click(screen.getByRole('button', { name: 'Verify' }))
}

describe('integration client management', () => {
  it('does not render or fetch clients for a non-root administrator', () => {
    useAuthStore.getState().auth.setUser({ id: 2, username: 'admin', role: 10 })
    renderManager()
    expect(
      screen.queryByRole('button', { name: 'Create client' })
    ).not.toBeInTheDocument()
    expect(api.get).not.toHaveBeenCalled()
  })

  it('shows the empty state and blocks creation when OIDC is not configured', async () => {
    list.oidc_configured = false
    renderManager()
    expect(await screen.findByText('No integration clients')).toBeVisible()
    expect(screen.getByRole('button', { name: 'Create client' })).toBeDisabled()
    expect(
      screen.getByText('Configure OIDC before creating an integration client.')
    ).toHaveAttribute('role', 'alert')
  })

  it('shows a recoverable error when the client list request fails', async () => {
    failList = true
    renderManager()
    expect(
      await screen.findByText('Failed to load integration clients')
    ).toBeVisible()
    failList = false
    await userEvent.click(screen.getByRole('button', { name: 'Retry' }))
    expect(await screen.findByText('No integration clients')).toBeVisible()
  })

  it('creates a client after verification and displays its secret only until the dialog closes', async () => {
    const cache = renderManager()
    const user = userEvent.setup()
    await user.click(
      await screen.findByRole('button', { name: 'Create client' })
    )
    const dialog = await screen.findByRole('dialog', { name: 'Create client' })
    await user.click(
      within(dialog).getByRole('button', { name: 'Create client' })
    )
    expect(
      await screen.findByText('Name must be 1 to 64 characters.')
    ).toBeVisible()
    await user.type(within(dialog).getByLabelText('Name'), 'Profile cards')
    await user.click(
      within(dialog).getByRole('button', { name: 'Create client' })
    )
    await verify(user)
    const credentials = await screen.findByRole('dialog', {
      name: 'Client credentials',
    })
    expect(within(credentials).getByLabelText('Client secret')).toHaveValue(
      'nacs_one-time-secret'
    )
    expect(
      within(credentials).getByRole('button', { name: 'Copy client secret' })
    ).toBeEnabled()
    expect(
      JSON.stringify(
        cache
          .getQueryCache()
          .getAll()
          .map((entry) => entry.state.data)
      )
    ).not.toContain('nacs_one-time-secret')
    const call = vi
      .mocked(api.post)
      .mock.calls.find(([url]) => url === '/api/integration-clients')
    expect(call?.[2]).toMatchObject({
      headers: { 'X-Security-Proof': 'one-use-proof' },
      singleUseAuthorization: true,
    })
    await user.click(
      within(credentials).getByText('Close', { selector: 'button' })
    )
    await waitFor(() =>
      expect(screen.queryByLabelText('Client secret')).not.toBeInTheDocument()
    )
    expect(
      await screen.findByRole('article', { name: 'Profile cards' })
    ).toBeVisible()
  })

  it('disables a client only after confirmation and scope-bound verification', async () => {
    list.items = [client]
    renderManager()
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Disable' }))
    const confirmation = await screen.findByRole('alertdialog')
    expect(api.patch).not.toHaveBeenCalled()
    await user.click(
      within(confirmation).getByRole('button', { name: 'Continue' })
    )
    await verify(user)
    await waitFor(() =>
      expect(api.patch).toHaveBeenCalledWith(
        '/api/integration-clients/nac_profile_cards',
        { enabled: false },
        expect.objectContaining({ singleUseAuthorization: true })
      )
    )
    const verification = vi
      .mocked(api.post)
      .mock.calls.find(([url]) => url === '/api/verify')
    expect(verification?.[1]).toMatchObject({
      scope: 'integration_client.status',
      context: { client_id: client.client_id, enabled: false },
    })
  })

  it('rotates a secret after verification and never adds plaintext to the client list cache', async () => {
    list.items = [client]
    const cache = renderManager()
    const user = userEvent.setup()
    await user.click(
      await screen.findByRole('button', { name: 'Rotate secret' })
    )
    await user.click(
      within(await screen.findByRole('alertdialog')).getByRole('button', {
        name: 'Continue',
      })
    )
    await verify(user)
    const credentials = await screen.findByRole('dialog', {
      name: 'Client credentials',
    })
    expect(within(credentials).getByLabelText('Client secret')).toHaveValue(
      'nacs_rotated-secret'
    )
    expect(
      JSON.stringify(
        cache
          .getQueryCache()
          .getAll()
          .map((entry) => entry.state.data)
      )
    ).not.toContain('nacs_rotated-secret')
    act(() =>
      useAuthStore
        .getState()
        .auth.setUser({ id: 3, username: 'another-root', role: 100 })
    )
    await waitFor(() =>
      expect(screen.queryByLabelText('Client secret')).not.toBeInTheDocument()
    )
  })

  it('marks an identity-source change and permits disabling but not rotating the old client', async () => {
    list.items = [{ ...client, oidc_client_id: 'previous-gateway' }]
    renderManager()
    expect(await screen.findByText('OIDC configuration changed')).toBeVisible()
    expect(screen.getByRole('button', { name: 'Rotate secret' })).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Disable' })).toBeEnabled()
  })
})
