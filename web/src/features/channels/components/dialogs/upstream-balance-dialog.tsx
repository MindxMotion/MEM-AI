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
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useRef } from 'react'
import { useTranslation } from 'react-i18next'

import { Dialog } from '@/components/dialog'
import { Button } from '@/components/ui/button'
import { Progress, ProgressLabel } from '@/components/ui/progress'
import { toIntlLocale } from '@/i18n/languages'
import {
  ADMIN_PERMISSION_ACTIONS,
  ADMIN_PERMISSION_RESOURCES,
  hasPermission,
} from '@/lib/admin-permissions'
import { formatNumber, formatTimestampToDate } from '@/lib/format'
import { useAuthStore } from '@/stores/auth-store'

import { getUpstreamBalance, refreshUpstreamBalance } from '../../api'
import type { UpstreamBalanceSnapshot } from '../../lib/upstream-balance'

const errorLabels = {
  configuration: 'Invalid upstream balance configuration',
  authorization: 'Upstream balance authorization failed',
  timeout: 'Upstream balance request timed out',
  unavailable: 'Upstream balance unavailable',
  invalid_response: 'Invalid upstream balance response',
} as const

export function UpstreamBalanceValues(props: {
  snapshot?: UpstreamBalanceSnapshot
  loading: boolean
  failed: boolean
}) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const snapshot = props.snapshot
  // Site currency helpers convert USD to local quota; use authoritative upstream units.
  const amount = (value: number) =>
    `${new Intl.NumberFormat(locale, { minimumFractionDigits: snapshot?.precision ?? 2, maximumFractionDigits: snapshot?.precision ?? 2 }).format(value)} ${snapshot?.unit ?? ''}`
  const total = snapshot?.total
  const used = snapshot?.used
  const remaining = snapshot?.remaining
  const known = total != null && used != null && remaining != null
  return (
    <div className='bg-muted/50 space-y-4 rounded-lg border p-4 break-words'>
      <p className='text-sm font-medium'>
        {snapshot?.scope === 'key' && `${t('Key')} · `}
        {t('Upstream account quota')}
      </p>
      {snapshot?.shared_account && (
        <p className='text-muted-foreground text-xs'>
          {t('Shared account; do not sum channels')}
        </p>
      )}
      {known ? (
        <>
          <p className='text-xl font-semibold tabular-nums'>
            {t('Remaining')}: {amount(remaining)}
          </p>
          <dl className='flex flex-wrap gap-6 text-sm'>
            <div>
              <dt>{t('Upstream used')}</dt>
              <dd className='tabular-nums'>{amount(used)}</dd>
            </div>
            <div>
              <dt>{t('Total')}</dt>
              <dd className='tabular-nums'>{amount(total)}</dd>
            </div>
          </dl>
          {total > 0 && (
            <Progress
              value={Math.min(100, (used / total) * 100)}
              className='**:data-[slot=progress-track]:h-2 **:data-[slot=progress-indicator]:motion-reduce:transition-none'
              aria-label={t('Upstream used')}
              aria-valuetext={`${amount(used)} / ${amount(total)}`}
            >
              <ProgressLabel>
                {t('Upstream used')}:{' '}
                {formatNumber(Math.round((used / total) * 1000) / 10, locale)}%
              </ProgressLabel>
            </Progress>
          )}
          {remaining < 0 && (
            <p className='text-destructive text-sm'>{t('Overdrawn')}</p>
          )}
          <p className='text-muted-foreground text-xs'>
            {t(snapshot?.scope === 'account' ? 'Account' : 'Key')} ·{' '}
            {t('Cumulative')} · {t('Last updated:')}{' '}
            {formatTimestampToDate(snapshot?.checked_at ?? 0)}
          </p>
        </>
      ) : (
        <p>{t('Unknown — refresh to check')}</p>
      )}
      <p role='status' aria-live='polite' className='text-sm'>
        {props.loading && t('Refreshing...')}
        {!props.loading &&
          snapshot?.status === 'stale' &&
          t('Stale — last known values retained')}
        {snapshot?.error && ` ${t(errorLabels[snapshot.error])}`}
        {props.failed && ` ${t('Failed to query balance')}`}
      </p>
    </div>
  )
}

export function UpstreamBalanceDialog(props: {
  channelId: number
  channelName: string
  sensitiveVisible?: boolean
  onClose: () => void
}) {
  const { t } = useTranslation()
  const user = useAuthStore((state) => state.auth.user)
  const canOperate = hasPermission(
    user,
    ADMIN_PERMISSION_RESOURCES.CHANNEL,
    ADMIN_PERMISSION_ACTIONS.OPERATE
  )
  const client = useQueryClient()
  const mounted = useRef(true)
  const returnFocus = useRef(
    document.activeElement instanceof HTMLElement
      ? document.activeElement
      : null
  )
  const refreshController = useRef<AbortController | null>(null)
  useEffect(() => {
    mounted.current = true
    return () => {
      mounted.current = false
      refreshController.current?.abort()
    }
  }, [])
  const queryKey = ['upstream-balance', props.channelId, user?.id]
  const query = useQuery({
    queryKey,
    queryFn: ({ signal }) => getUpstreamBalance(props.channelId, signal),
    staleTime: 0,
    gcTime: 0,
    retry: false,
    refetchOnWindowFocus: false,
  })
  const mutation = useMutation({
    mutationFn: () => {
      refreshController.current = new AbortController()
      return refreshUpstreamBalance(
        props.channelId,
        refreshController.current.signal
      )
    },
    onSuccess: (snapshot) => {
      // The dialog is channel-keyed and unmounted on close; no cross-channel cache write.
      if (
        mounted.current &&
        useAuthStore.getState().auth.user?.id === user?.id
      ) {
        client.setQueryData(queryKey, snapshot)
      }
    },
  })
  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open) props.onClose()
      }}
      title={t('Query Balance')}
      description={props.channelName}
      contentHeight='auto'
      bodyClassName='space-y-4'
      finalFocus={() =>
        returnFocus.current?.isConnected
          ? returnFocus.current
          : document.querySelector<HTMLButtonElement>(
              `[data-upstream-balance-channel="${props.channelId}"]`
            )
      }
      footer={
        <Button variant='outline' onClick={props.onClose}>
          {t('Close')}
        </Button>
      }
    >
      {props.sensitiveVisible === false ? (
        <p>******</p>
      ) : (
        <UpstreamBalanceValues
          snapshot={query.data}
          loading={query.isPending || mutation.isPending}
          failed={query.isError || mutation.isError}
        />
      )}
      <Button
        className='w-full'
        disabled={!canOperate || query.isPending || mutation.isPending}
        onClick={() => mutation.mutate()}
      >
        {t('Refresh')}
      </Button>
    </Dialog>
  )
}
