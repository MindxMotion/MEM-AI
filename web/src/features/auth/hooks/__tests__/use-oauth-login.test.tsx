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
*/

import { act, renderHook } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import { handleServerError } from '@/lib/handle-server-error'

import { createOAuthAuthorization, logout } from '../../api'
import { rememberOAuthLoginRedirect } from '../../lib/oauth-callback-mode'
import { useOAuthLogin } from '../use-oauth-login'

vi.mock('react-i18next', () => ({
  useTranslation: () => ({ t: (key: string) => key }),
}))
vi.mock('../../api', () => ({
  createOAuthAuthorization: vi.fn(),
  createOAuthFlow: vi.fn(),
  logout: vi.fn(),
}))
vi.mock('../../lib/oauth-callback-mode', () => ({
  rememberOAuthLoginRedirect: vi.fn(),
}))
vi.mock('@/lib/api', () => ({ clearAuthentication: vi.fn() }))
vi.mock('@/lib/handle-server-error', () => ({
  handleServerError: vi.fn(),
}))

describe('environment-managed OIDC login', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    vi.mocked(logout).mockResolvedValue({ success: true, message: '' })
    vi.spyOn(window, 'open').mockImplementation(() => null)
  })

  it('uses the server authorization URL without requiring a public endpoint', async () => {
    vi.mocked(createOAuthAuthorization).mockResolvedValue({
      state: 'fixture-state',
      authorizationUrl:
        'https://identity.example.com/authorize?code_challenge=server-pkce',
    })
    const hook = renderHook(() =>
      useOAuthLogin({ oidc_enabled: true }, '/dashboard')
    )

    await act(() => hook.result.current.handleOIDCLogin())

    expect(createOAuthAuthorization).toHaveBeenCalledWith('oidc', 'login')
    expect(rememberOAuthLoginRedirect).toHaveBeenCalledWith(
      'fixture-state',
      '/dashboard'
    )
    expect(window.open).toHaveBeenCalledWith(
      'https://identity.example.com/authorize?code_challenge=server-pkce',
      '_self'
    )
    expect(hook.result.current.isLoading).toBe(false)
  })

  it('reports an incomplete authorization response without falling back to a client-built URL', async () => {
    vi.mocked(createOAuthAuthorization).mockResolvedValue({
      state: 'fixture-state',
    })
    const hook = renderHook(() =>
      useOAuthLogin({
        oidc_enabled: true,
        oidc_authorization_endpoint: 'https://identity.example.com/authorize',
        oidc_client_id: 'fixture-client',
      })
    )

    await act(() => hook.result.current.handleOIDCLogin())

    expect(window.open).not.toHaveBeenCalled()
    expect(handleServerError).toHaveBeenCalledOnce()
    expect(hook.result.current.isLoading).toBe(false)
  })

  it('does not start a disabled provider', async () => {
    const hook = renderHook(() => useOAuthLogin({ oidc_enabled: false }))

    await act(() => hook.result.current.handleOIDCLogin())

    expect(createOAuthAuthorization).not.toHaveBeenCalled()
    expect(window.open).not.toHaveBeenCalled()
  })
})
