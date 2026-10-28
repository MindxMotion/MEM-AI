import { describe, expect, it } from 'vitest'

import { calculateDashboardStats } from '../stats'

describe('calculateDashboardStats', () => {
  it('totals cache counters without changing existing tokens or charges', () => {
    expect(
      calculateDashboardStats([
        {
          created_at: 3600,
          count: 2,
          quota: 100,
          token_used: 40,
          cache_read_tokens: 14,
          cache_creation_tokens: 8,
        },
        { created_at: 7200, count: 1, quota: 20, token_used: 10 },
      ])
    ).toEqual({
      totalQuota: 120,
      totalCount: 3,
      totalTokens: 50,
      totalCacheReadTokens: 14,
      totalCacheCreationTokens: 8,
    })
  })

  it('shows zero for missing, negative, or non-finite cache counters', () => {
    expect(
      calculateDashboardStats([
        { created_at: 3600 },
        {
          created_at: 7200,
          cache_read_tokens: -2,
          cache_creation_tokens: Number.NaN,
        },
        {
          created_at: 10800,
          cache_read_tokens: Number.POSITIVE_INFINITY,
          cache_creation_tokens: -9,
        },
      ])
    ).toEqual({
      totalQuota: 0,
      totalCount: 0,
      totalTokens: 0,
      totalCacheReadTokens: 0,
      totalCacheCreationTokens: 0,
    })
  })
})
