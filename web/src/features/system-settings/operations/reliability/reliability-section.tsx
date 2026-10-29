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
import { useQuery } from '@tanstack/react-query'
import { Activity, CircleCheck, TriangleAlert } from 'lucide-react'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { StaticDataTable } from '@/components/data-table'
import { EmptyState } from '@/components/empty-state'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Label } from '@/components/ui/label'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import { StatCard } from '@/features/dashboard/components/ui/stat-card'
import { toIntlLocale } from '@/i18n/languages'
import { api } from '@/lib/api'
import { formatNumber } from '@/lib/format'
import { requireServerSuccess } from '@/lib/server-error-message'
import { useAuthStore } from '@/stores/auth-store'

import { SettingsSection } from '../../components/settings-section'
import { ReliabilityMetricsTable } from './metrics-table'
import type { ReliabilityReport } from './types'

export function ReliabilitySection() {
  const isRoot = useAuthStore((state) => state.auth.user?.role === 100)
  if (!isRoot) return null
  return <ReliabilityPanel />
}

function ReliabilityPanel() {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const [days, setDays] = useState(7)
  const offset = -new Date().getTimezoneOffset()
  const query = useQuery({
    queryKey: ['request-reliability', days, offset],
    queryFn: async () => {
      const response = await api.get<{
        success: boolean
        data: ReliabilityReport
      }>('/api/performance/reliability', {
        params: { days, offset_minutes: offset },
      })
      return requireServerSuccess(response.data).data
    },
    staleTime: 30_000,
    retry: false,
  })
  const data = query.data
  const names = new Map(
    data?.channels.map((channel) => [channel.channel_id, channel.name])
  )
  const channelName = (id: number) =>
    names.get(id) || t('Channel {{id}}', { id: formatNumber(id, locale) })
  const reasons: Record<string, string> = {
    timeout: t('Timeout'),
    client_disconnected: t('Client disconnected'),
    request_rejected: t('Request rejected'),
    upstream_error: t('Upstream error'),
    scanner_error: t('Stream interrupted'),
    stream_error: t('Stream interrupted'),
    upstream_stream_failed: t('Stream interrupted'),
    upstream_transport_error: t('Upstream error'),
    missing_terminal_event: t('Missing terminal event'),
    missing_terminal: t('Missing terminal event'),
    incomplete: t('Missing terminal event'),
  }
  return (
    <SettingsSection title={t('Request reliability')}>
      <div className='flex flex-wrap items-end gap-3'>
        <div className='space-y-1'>
          <Label htmlFor='reliability-period'>{t('Period')}</Label>
          <NativeSelect
            id='reliability-period'
            value={days}
            onChange={(event) => setDays(Number(event.target.value))}
          >
            <NativeSelectOption value={1}>{t('Today')}</NativeSelectOption>
            <NativeSelectOption value={7}>
              {t('Last 7 days')}
            </NativeSelectOption>
            <NativeSelectOption value={30}>
              {t('Last 30 days')}
            </NativeSelectOption>
          </NativeSelect>
        </div>
        <Button
          variant='outline'
          disabled={query.isFetching}
          onClick={() => void query.refetch()}
        >
          {t('Refresh')}
        </Button>
        {data && !query.isError && (
          <p className='text-muted-foreground text-xs'>
            {t('Updated at {{time}}', {
              time: new Date(data.window_end * 1000).toLocaleString(locale),
            })}
          </p>
        )}
      </div>
      <Alert>
        <AlertDescription className='space-y-2'>
          <p>
            {t(
              'Success requires a completed response, not just HTTP 200. Only ended, authenticated API requests are counted.'
            )}
          </p>
          <p>
            {t(
              'Overall and daily rates include local rejections. Channel rates include only single-attempt upstream requests; retries are not attributed to one provider.'
            )}
          </p>
          <p>
            {t(
              'Latency percentiles use successful requests only. First event latency is not first text token latency. Models and workloads differ between channels.'
            )}
          </p>
          <p>
            {t(
              'Days follow your browser timezone. Observations are retained for 30 days; earlier history and in-flight requests are not included.'
            )}
          </p>
        </AlertDescription>
      </Alert>
      {query.isPending && <LoadingState />}
      {query.isError && (
        <ErrorState
          title={t('Failed to load request reliability')}
          description={t(
            'Retry or choose a shorter period if there are too many observations.'
          )}
          onRetry={() => void query.refetch()}
        />
      )}
      {data && !query.isError && (
        <>
          {(data.writer_dropped > 0 || data.writer_failed > 0) && (
            <Alert variant='destructive'>
              <AlertDescription>
                {t(
                  'Observation loss on this instance: {{dropped}} dropped, {{failed}} failed writes. Statistics may be incomplete.',
                  {
                    dropped: formatNumber(data.writer_dropped, locale),
                    failed: formatNumber(data.writer_failed, locale),
                  }
                )}
              </AlertDescription>
            </Alert>
          )}
          <p className='text-muted-foreground text-sm'>
            {t(
              'Local requests: {{local}}. Retried requests: {{retries}}. Pending observations on this instance: {{queued}}.',
              {
                local: formatNumber(data.local_requests, locale),
                retries: formatNumber(data.retry_requests, locale),
                queued: formatNumber(data.writer_queued, locale),
              }
            )}
          </p>
          <div className='grid grid-cols-1 gap-3 sm:grid-cols-3'>
            <StatCard
              title={t('Total requests')}
              value={formatNumber(data.summary.requests, locale)}
              description={t('Recorded requests in the selected period')}
              icon={Activity}
              sparkline={data.daily.map((day) => day.requests)}
              sparklineVariant='bars'
            />
            <StatCard
              title={t('Success rate')}
              value={
                data.summary.success_rate === null
                  ? '-'
                  : `${formatNumber(data.summary.success_rate, locale)}%`
              }
              description={t('Successful requests: {{count}}', {
                count: formatNumber(data.summary.succeeded, locale),
              })}
              icon={CircleCheck}
            />
            <StatCard
              title={t('Unsuccessful requests')}
              value={formatNumber(
                data.summary.requests - data.summary.succeeded,
                locale
              )}
              description={t(
                'Rejections, failures, cancellations and unknown outcomes'
              )}
              icon={TriangleAlert}
            />
          </div>
          {data.summary.requests === 0 && (
            <EmptyState
              icon={Activity}
              title={t('No recorded requests in this period')}
            />
          )}
          <ReliabilityMetricsTable
            title={t('Channel success rates')}
            label={t('Channel')}
            rows={data.channels.map((channel) => ({
              ...channel,
              name: `${channelName(channel.channel_id)} (#${channel.channel_id})`,
            }))}
          />
          <ReliabilityMetricsTable
            title={t('Daily success rates')}
            label={t('Date')}
            rows={data.daily.map((day) => ({ ...day, name: day.day }))}
          />
          <div className='min-w-0 space-y-3'>
            <h4 className='font-medium'>{t('Recent unsuccessful requests')}</h4>
            <p className='text-muted-foreground text-sm'>
              {t(
                'Latest 50 unsuccessful observations in this period. No prompts, credentials or user identifiers are shown.'
              )}
            </p>
            <StaticDataTable
              data={data.recent_failures}
              tableProps={{ 'aria-label': t('Recent unsuccessful requests') }}
              className='overflow-x-auto'
              tableClassName='min-w-[800px]'
              emptyContent={t('No unsuccessful requests in this period')}
              columns={[
                {
                  id: 'time',
                  header: t('Time'),
                  cell: (row) =>
                    new Date(row.created_at * 1000).toLocaleString(locale),
                },
                {
                  id: 'channel',
                  header: t('Channel'),
                  cell: (row) =>
                    row.attempts === 0
                      ? t('Not sent upstream')
                      : channelName(row.channel_id),
                },
                {
                  id: 'model',
                  header: t('Model'),
                  cell: (row) => row.model_name,
                },
                {
                  id: 'reason',
                  header: t('Failure reason'),
                  cell: (row) => reasons[row.reason] || t('Other failures'),
                },
                {
                  id: 'http',
                  header: t('HTTP status'),
                  cell: (row) => row.http_status || '-',
                },
                {
                  id: 'upstream',
                  header: t('Error status'),
                  cell: (row) => row.error_status || '-',
                },
                {
                  id: 'attempts',
                  header: t('Attempts'),
                  cell: (row) => formatNumber(row.attempts, locale),
                },
                {
                  id: 'latency',
                  header: t('Latency'),
                  cell: (row) =>
                    t('{{value}} s', {
                      value: formatNumber(row.latency_ms / 1000, locale),
                    }),
                },
              ]}
            />
          </div>
        </>
      )}
    </SettingsSection>
  )
}
