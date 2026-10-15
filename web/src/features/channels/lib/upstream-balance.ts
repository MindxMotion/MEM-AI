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
import { z } from 'zod'

const balancePathSchema = z
  .string()
  .max(512)
  .refine(
    (value) =>
      value === '' ||
      (value.startsWith('/') &&
        !value.startsWith('//') &&
        !/[\\%?#\s]/.test(value) &&
        !value
          .split('/')
          .some((segment) => segment === '.' || segment === '..'))
  )

export const upstreamBalanceProfileSchema = z.object({
  profile: z.literal('legacy_subscription_usage'),
  origin: z.string().regex(/^[a-zA-Z0-9_-]{1,64}$/),
  subscription_path: balancePathSchema.optional(),
  usage_path: balancePathSchema.optional(),
  unit: z.string().regex(/^[a-zA-Z][a-zA-Z0-9_-]{0,15}$/),
  scope: z.enum(['account', 'key']),
  period: z.literal('cumulative'),
})
export type UpstreamBalanceProfile = z.infer<
  typeof upstreamBalanceProfileSchema
>
export const upstreamBalanceSnapshotSchema = z
  .object({
    total: z.number().finite().nonnegative().nullable(),
    used: z.number().finite().nonnegative().nullable(),
    remaining: z.number().finite().nullable(),
    unit: z.string(),
    scope: z.enum(['account', 'key']),
    period: z.literal('cumulative'),
    precision: z.number().int().min(0).max(8),
    checked_at: z.number().int().nonnegative(),
    status: z.enum(['unknown', 'fresh', 'stale', 'error']),
    error: z
      .enum([
        'configuration',
        'authorization',
        'timeout',
        'unavailable',
        'invalid_response',
      ])
      .optional(),
    shared_account: z.boolean(),
  })
  .refine((value) => {
    const known =
      value.total !== null && value.used !== null && value.remaining !== null
    return (
      known ||
      (value.total === null && value.used === null && value.remaining === null)
    )
  }, 'Incomplete upstream snapshot')
export type UpstreamBalanceSnapshot = z.infer<
  typeof upstreamBalanceSnapshotSchema
>

export function balanceSettingsRecord(raw: string): Record<string, unknown> {
  try {
    const parsed: unknown = JSON.parse(raw || '{}')
    if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) {
      return parsed as Record<string, unknown>
    }
  } catch {
    /* Invalid settings stay non-configured; server validates writes. */
  }
  return {}
}

export function hasUpstreamBalance(raw: string): boolean {
  return balanceSettingsRecord(raw).upstream_balance != null
}

export function withUpstreamBalance(
  raw: string,
  profile: UpstreamBalanceProfile | null
): string {
  const record = balanceSettingsRecord(raw)
  if (profile) record.upstream_balance = profile
  else delete record.upstream_balance
  return JSON.stringify(record)
}
