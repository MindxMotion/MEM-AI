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
import { useTranslation } from 'react-i18next'

import {
  StaticDataTable,
  type StaticDataTableColumn,
} from '@/components/data-table'
import { toIntlLocale } from '@/i18n/languages'
import { formatNumber } from '@/lib/format'

import type { ReliabilityMetrics } from './types'

export function ReliabilityMetricsTable(props: {
  title: string
  label: string
  rows: (ReliabilityMetrics & { name: string })[]
}) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const duration = (value: number | null) =>
    value === null
      ? '-'
      : t('{{value}} s', { value: formatNumber(value / 1000, locale) })
  const columns: StaticDataTableColumn<
    ReliabilityMetrics & { name: string }
  >[] = [
    { id: 'name', header: props.label, cell: (row) => row.name },
    {
      id: 'requests',
      header: t('Total requests'),
      cell: (row) => formatNumber(row.requests, locale),
    },
    {
      id: 'succeeded',
      header: t('Successful requests'),
      cell: (row) => formatNumber(row.succeeded, locale),
    },
    {
      id: 'rate',
      header: t('Success rate'),
      cell: (row) =>
        row.success_rate === null
          ? '-'
          : `${formatNumber(row.success_rate, locale)}%`,
    },
    {
      id: 'rejected',
      header: t('Rejected'),
      cell: (row) => formatNumber(row.rejected, locale),
    },
    {
      id: 'failed',
      header: t('Failed'),
      cell: (row) => formatNumber(row.failed, locale),
    },
    {
      id: 'cancelled',
      header: t('Cancelled'),
      cell: (row) => formatNumber(row.cancelled, locale),
    },
    {
      id: 'unknown',
      header: t('Unknown'),
      cell: (row) => formatNumber(row.unknown, locale),
    },
    {
      id: 'p50',
      header: t('Completion P50'),
      cell: (row) => duration(row.latency_p50_ms),
    },
    {
      id: 'p95',
      header: t('Completion P95'),
      cell: (row) => duration(row.latency_p95_ms),
    },
    {
      id: 'first50',
      header: t('First event P50'),
      cell: (row) => duration(row.first_p50_ms),
    },
    {
      id: 'first95',
      header: t('First event P95'),
      cell: (row) => duration(row.first_p95_ms),
    },
    {
      id: 'samples',
      header: t('First event samples'),
      cell: (row) => formatNumber(row.first_samples, locale),
    },
  ]
  return (
    <div className='min-w-0 space-y-3'>
      <h4 className='font-medium'>{props.title}</h4>
      <StaticDataTable
        columns={columns}
        data={props.rows}
        getRowKey={(row) => row.name}
        className='overflow-x-auto'
        tableClassName='min-w-[1100px]'
        tableProps={{ 'aria-label': props.title }}
        emptyContent={t('No recorded requests in this period')}
      />
    </div>
  )
}
