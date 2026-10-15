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
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, expect, test, vi } from 'vitest'

import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import {
  CHANNEL_FORM_DEFAULT_VALUES,
  channelFormSchema,
  transformFormDataToUpdatePayload,
} from '../../lib/channel-form'
import {
  upstreamBalanceSnapshotSchema,
  withUpstreamBalance,
  type UpstreamBalanceSnapshot,
} from '../../lib/upstream-balance'
import {
  UpstreamBalanceDialog,
  UpstreamBalanceValues,
} from '../dialogs/upstream-balance-dialog'
import { UpstreamBalanceSettings } from '../upstream-balance-settings'

test('closing a pending refresh aborts it and discards its late result', async () => {
  useAuthStore
    .getState()
    .auth.setUser({ id: 1, username: 'fixture', role: 100 })
  vi.spyOn(api, 'get').mockResolvedValue({
    data: { success: true, data: known },
  })
  type Response = { data: { success: boolean; data: UpstreamBalanceSnapshot } }
  let settle: ((response: Response) => void) | undefined
  const pending = new Promise<Response>((resolve) => {
    settle = resolve
  })
  const post = vi.spyOn(api, 'post').mockReturnValue(pending)
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  const view = render(
    <QueryClientProvider client={client}>
      <UpstreamBalanceDialog
        channelId={11}
        channelName='Fixture'
        onClose={vi.fn()}
      />
    </QueryClientProvider>
  )
  await screen.findByText('Remaining: 95.00 USD')
  await userEvent.setup().click(screen.getByRole('button', { name: 'Refresh' }))
  expect(screen.getByRole('button', { name: 'Refresh' })).toBeDisabled()
  view.unmount()
  expect(post.mock.calls[0][2]?.signal?.aborted).toBe(true)
  settle?.({
    data: { success: true, data: { ...known, remaining: 77 } },
  })
  await pending
  await waitFor(() =>
    expect(client.getQueryData(['upstream-balance', 11, 1])).toBeUndefined()
  )
})

const known: UpstreamBalanceSnapshot = {
  total: 120,
  used: 25,
  remaining: 95,
  unit: 'USD',
  scope: 'account',
  period: 'cumulative',
  precision: 2,
  checked_at: 1000,
  status: 'fresh',
  shared_account: true,
}

afterEach(() => {
  vi.restoreAllMocks()
  useAuthStore.getState().auth.reset()
})

test('positive total uses upstream amounts and exposes an accessible percentage bar', () => {
  render(
    <UpstreamBalanceValues snapshot={known} loading={false} failed={false} />
  )
  expect(screen.getByText('Remaining: 95.00 USD')).toBeVisible()
  const bar = screen.getByRole('progressbar', { name: /Upstream used/ })
  expect(Number(bar.getAttribute('aria-valuenow'))).toBeCloseTo(
    (25 / 120) * 100
  )
  expect(screen.getByText('Shared account; do not sum channels')).toBeVisible()
})

test('unknown does not invent zero and zero total does not divide', () => {
  const view = render(<UpstreamBalanceValues loading={false} failed={false} />)
  expect(screen.getByText('Unknown — refresh to check')).toBeVisible()
  expect(screen.queryByRole('progressbar')).not.toBeInTheDocument()
  view.rerender(
    <UpstreamBalanceValues
      snapshot={{ ...known, total: 0, used: 0, remaining: 0 }}
      loading={false}
      failed={false}
    />
  )
  expect(screen.getByText('Remaining: 0.00 USD')).toBeVisible()
  expect(screen.queryByRole('progressbar')).not.toBeInTheDocument()
})

test('key scope is labeled as a key rather than an account', () => {
  render(
    <UpstreamBalanceValues
      snapshot={{ ...known, scope: 'key', shared_account: false }}
      loading={false}
      failed={false}
    />
  )
  expect(screen.getByText('Key · Upstream account quota')).toBeVisible()
  expect(screen.queryByText('Shared account; do not sum channels')).toBeNull()
})

test('overdrawn clamps geometry while failed refresh retains true amounts', () => {
  render(
    <UpstreamBalanceValues
      snapshot={{
        ...known,
        used: 125,
        remaining: -5,
        status: 'stale',
        error: 'unavailable',
      }}
      loading={false}
      failed={false}
    />
  )
  expect(screen.getByRole('progressbar')).toHaveAttribute(
    'aria-valuenow',
    '100'
  )
  expect(screen.getByText('Remaining: -5.00 USD')).toBeVisible()
  expect(screen.getByText('125.00 USD')).toBeVisible()
  expect(screen.getByText('Overdrawn')).toBeVisible()
  expect(screen.getByRole('status')).toHaveTextContent(
    'last known values retained'
  )
})

test('profile edits preserve unrelated JSON fields and disabling only removes the profile', async () => {
  const user = userEvent.setup()
  const changed = vi.fn()
  render(
    <UpstreamBalanceSettings
      value='{"unrelated":{"keep":true}}'
      onChange={changed}
    />
  )
  await user.click(
    screen.getByRole('switch', { name: 'Upstream account quota' })
  )
  const result: string = changed.mock.calls[0][0]
  expect(JSON.parse(result).unrelated).toEqual({ keep: true })
  expect(JSON.parse(result).upstream_balance.profile).toBe(
    'legacy_subscription_usage'
  )
  expect(JSON.parse(withUpstreamBalance(result, null))).toEqual({
    unrelated: { keep: true },
  })
})

test('strict snapshot rejects nonnumeric upstream values rather than coercing to zero', () => {
  expect(
    upstreamBalanceSnapshotSchema.safeParse({ ...known, total: '120' }).success
  ).toBe(false)
  expect(
    upstreamBalanceSnapshotSchema.safeParse({ ...known, total: null }).success
  ).toBe(false)
})

test('saved profile survives the existing form payload without a replacement key and invalid paths block saving', () => {
  const profile = {
    profile: 'legacy_subscription_usage',
    origin: 'primary',
    scope: 'account',
    period: 'cumulative',
    unit: 'USD',
  }
  const form = {
    ...CHANNEL_FORM_DEFAULT_VALUES,
    type: 1,
    name: 'Fixture',
    models: 'fixture-model',
    group: ['default'],
    key: '',
    settings: JSON.stringify({ preserved: true, upstream_balance: profile }),
  }
  const payload = transformFormDataToUpdatePayload(form, 11)
  expect(JSON.parse(payload.settings ?? '{}').upstream_balance).toEqual(profile)
  expect(JSON.parse(payload.settings ?? '{}').preserved).toBe(true)
  expect(payload.key).toBeUndefined()
  expect(
    transformFormDataToUpdatePayload({ ...form, key: '******' }, 11).key
  ).toBeUndefined()
  expect(
    channelFormSchema.safeParse({
      ...form,
      settings: JSON.stringify({
        upstream_balance: { ...profile, usage_path: '//example.com/private' },
      }),
    }).success
  ).toBe(false)
})

test('read-only administrator reads cache but cannot trigger refresh', async () => {
  vi.spyOn(api, 'get').mockResolvedValue({
    data: { success: true, data: known },
  })
  const post = vi.spyOn(api, 'post')
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  render(
    <QueryClientProvider client={client}>
      <UpstreamBalanceDialog
        channelId={11}
        channelName='Fixture channel'
        onClose={vi.fn()}
      />
    </QueryClientProvider>
  )
  await waitFor(() =>
    expect(screen.getByText('Remaining: 95.00 USD')).toBeVisible()
  )
  expect(screen.getByRole('button', { name: 'Refresh' })).toBeDisabled()
  expect(post).not.toHaveBeenCalled()
})
