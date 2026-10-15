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
import { useId } from 'react'
import { useTranslation } from 'react-i18next'

import { Alert, AlertDescription } from '@/components/ui/alert'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Switch } from '@/components/ui/switch'

import {
  balanceSettingsRecord,
  withUpstreamBalance,
  type UpstreamBalanceProfile,
} from '../lib/upstream-balance'

export function UpstreamBalanceSettings(props: {
  value: string
  onChange: (value: string) => void
  error?: string
}) {
  const { t } = useTranslation()
  const id = useId()
  const raw = balanceSettingsRecord(props.value).upstream_balance
  const enabled = raw != null
  const profile = (raw ?? {
    profile: 'legacy_subscription_usage',
    origin: '',
    unit: 'USD',
    scope: 'account',
    period: 'cumulative',
  }) as UpstreamBalanceProfile
  const fields = [
    ['origin', 'Balance origin alias'],
    ['subscription_path', 'Subscription path'],
    ['usage_path', 'Usage path'],
    ['unit', 'Upstream unit'],
    ['scope', 'Balance scope (account or key)'],
  ] as const
  return (
    <div className='space-y-3 rounded-lg border p-4'>
      <div className='flex items-center justify-between gap-3'>
        <Label htmlFor={id}>{t('Upstream account quota')}</Label>
        <Switch
          id={id}
          checked={enabled}
          onCheckedChange={(checked) =>
            props.onChange(
              withUpstreamBalance(props.value, checked ? profile : null)
            )
          }
        />
      </div>
      {enabled && (
        <>
          <p className='text-muted-foreground text-sm'>
            {t(
              'Legacy subscription/usage profile. Confirm matching cumulative total and usage, unit and scope. Shared accounts must not be summed.'
            )}
          </p>
          {fields.map(([field, label]) => (
            <div key={field} className='space-y-1'>
              <Label htmlFor={`${id}-${field}`}>{t(label)}</Label>
              <Input
                id={`${id}-${field}`}
                aria-invalid={Boolean(props.error)}
                aria-describedby={props.error ? `${id}-error` : undefined}
                value={profile[field] ?? ''}
                onChange={(event) =>
                  props.onChange(
                    withUpstreamBalance(props.value, {
                      ...profile,
                      [field]: event.target.value,
                    })
                  )
                }
              />
            </div>
          ))}
          <p className='text-muted-foreground text-xs'>
            {t(
              'Empty paths use /v1/dashboard/billing/subscription and /v1/dashboard/billing/usage. Only root-relative paths are allowed. The saved channel key stays on the server.'
            )}
          </p>
        </>
      )}
      {props.error && (
        <Alert variant='destructive' id={`${id}-error`}>
          <AlertDescription>{t(props.error)}</AlertDescription>
        </Alert>
      )}
    </div>
  )
}
