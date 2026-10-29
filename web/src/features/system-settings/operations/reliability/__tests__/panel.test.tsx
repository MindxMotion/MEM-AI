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
import i18next from 'i18next'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'

import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import { ReliabilitySection } from '../reliability-section'
import type { ReliabilityMetrics, ReliabilityReport } from '../types'

const metrics: ReliabilityMetrics = {
  requests: 3,
  succeeded: 1,
  rejected: 1,
  failed: 1,
  cancelled: 0,
  unknown: 0,
  success_rate: 33.33,
  latency_p50_ms: 1500,
  latency_p95_ms: 1500,
  first_p50_ms: 300,
  first_p95_ms: 300,
  first_samples: 1,
}
let report: ReliabilityReport
const clients: QueryClient[] = []

function renderPanel() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  clients.push(client)
  return render(
    <QueryClientProvider client={client}>
      <ReliabilitySection />
    </QueryClientProvider>
  )
}

beforeEach(async () => {
  await i18next.changeLanguage('en')
  useAuthStore
    .getState()
    .auth.setUser({ id: 1, username: 'fixture-root', role: 100 })
  report = {
    window_start: 1793203200,
    window_end: 1793242800,
    offset_minutes: 480,
    summary: { ...metrics },
    channels: [
      {
        ...metrics,
        requests: 2,
        rejected: 0,
        success_rate: 50,
        channel_id: 13,
        name: 'BestMax',
      },
    ],
    daily: [{ ...metrics, day: '2026-10-29' }],
    recent_failures: [
      {
        created_at: 1793242700,
        channel_id: 13,
        model_name: 'fixture-model',
        reason: 'timeout',
        http_status: 200,
        error_status: 0,
        attempts: 1,
        latency_ms: 300000,
      },
      {
        created_at: 1793242600,
        channel_id: 13,
        model_name: 'fixture-model',
        reason: 'request_rejected',
        http_status: 403,
        error_status: 403,
        attempts: 0,
        latency_ms: 15,
      },
    ],
    local_requests: 1,
    retry_requests: 0,
    writer_dropped: 0,
    writer_failed: 0,
    writer_queued: 0,
  }
  vi.spyOn(api, 'get').mockImplementation(async () => ({
    data: { success: true, data: report },
  }))
})

afterEach(async () => {
  for (const client of clients.splice(0)) client.clear()
  useAuthStore.getState().auth.setUser(null)
  await i18next.changeLanguage('en')
})

test('completed outcomes, not HTTP 200, determine the displayed global and channel rates', async () => {
  renderPanel()
  const channels = await screen.findByRole('table', {
    name: 'Channel success rates',
  })
  expect(within(channels).getByText('50%')).toBeVisible()
  expect(within(channels).getByText('BestMax (#13)')).toBeVisible()
  expect(screen.getAllByText('33.33%').length).toBeGreaterThan(0)
  const failures = screen.getByRole('table', {
    name: 'Recent unsuccessful requests',
  })
  expect(
    within(failures).getByRole('row', { name: /Timeout.*200/ })
  ).toBeVisible()
  expect(within(failures).getByText('Not sent upstream')).toBeVisible()
  expect(
    screen.getByRole('table', { name: 'Daily success rates' })
  ).toBeVisible()
  expect(within(channels).getAllByText('1.5 s')).toHaveLength(2)
})

test('no recorded traffic shows unknown rates and an explicit empty state, not 100 percent', async () => {
  report.summary = {
    ...metrics,
    requests: 0,
    succeeded: 0,
    rejected: 0,
    failed: 0,
    success_rate: null,
    latency_p50_ms: null,
    latency_p95_ms: null,
    first_p50_ms: null,
    first_p95_ms: null,
    first_samples: 0,
  }
  report.channels = []
  report.daily = [{ ...report.summary, day: '2026-10-29' }]
  report.recent_failures = []
  renderPanel()
  expect(
    (await screen.findAllByText('No recorded requests in this period')).length
  ).toBeGreaterThan(0)
  expect(screen.queryByText('100%')).not.toBeInTheDocument()
  expect(
    screen.getByText('No unsuccessful requests in this period')
  ).toBeVisible()
})

test('period changes request new calendar-day data with the browser timezone', async () => {
  const user = userEvent.setup()
  renderPanel()
  await screen.findByRole('table', { name: 'Channel success rates' })
  await user.selectOptions(
    screen.getByRole('combobox', { name: 'Period' }),
    '1'
  )
  await waitFor(() =>
    expect(api.get).toHaveBeenLastCalledWith('/api/performance/reliability', {
      params: { days: 1, offset_minutes: -new Date().getTimezoneOffset() },
    })
  )
})

test('a failed response shows retry controls rather than success-shaped empty data', async () => {
  vi.mocked(api.get).mockResolvedValueOnce({
    data: {
      success: false,
      message: 'Too many observations. Choose a shorter period.',
    },
  })
  const user = userEvent.setup()
  renderPanel()
  expect(
    await screen.findByText('Failed to load request reliability')
  ).toBeVisible()
  expect(
    screen.queryByRole('table', { name: 'Channel success rates' })
  ).not.toBeInTheDocument()
  await user.click(screen.getByRole('button', { name: 'Retry' }))
  expect(
    await screen.findByRole('table', { name: 'Channel success rates' })
  ).toBeVisible()
})

test('observation loss is explicit and long channel names remain in the scrollable table', async () => {
  report.writer_dropped = 2
  report.writer_failed = 1
  report.channels[0].name = 'Long provider name '.repeat(30)
  renderPanel()
  expect(await screen.findByText(/2 dropped, 1 failed writes/)).toBeVisible()
  const table = screen.getByRole('table', { name: 'Channel success rates' })
  expect(table.closest('.overflow-x-auto')).not.toBeNull()
  expect(within(table).getByText(/Long provider name/)).toBeInTheDocument()
})

test.each([1, 10])(
  'non-root role %i cannot render the panel or request its data',
  (role) => {
    useAuthStore
      .getState()
      .auth.setUser({ id: 2, username: 'fixture-user', role })
    const view = renderPanel()
    expect(view.container).toBeEmptyDOMElement()
    expect(api.get).not.toHaveBeenCalled()
  }
)

test.each([
  ['zhCN', '33.33%'],
  ['zhTW', '33.33%'],
  ['en', '33.33%'],
  ['fr', '33,33%'],
  ['ru', '33,33%'],
  ['ja', '33.33%'],
  ['vi', '33,33%'],
  ['not-a-language', '33.33%'],
])(
  'switching interface language to %s updates valid locale-aware numbers',
  async (language, rate) => {
    renderPanel()
    await screen.findByRole('table', { name: 'Channel success rates' })
    i18next.addResourceBundle(
      language,
      'translation',
      { 'Request reliability': 'Request reliability' },
      true,
      true
    )
    await act(async () => {
      await i18next.changeLanguage(language)
    })
    expect(screen.getAllByText(rate).length).toBeGreaterThan(0)
  }
)
