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
export type ReliabilityMetrics = {
  requests: number
  succeeded: number
  rejected: number
  failed: number
  cancelled: number
  unknown: number
  success_rate: number | null
  latency_p50_ms: number | null
  latency_p95_ms: number | null
  first_p50_ms: number | null
  first_p95_ms: number | null
  first_samples: number
}

export type ReliabilityReport = {
  window_start: number
  window_end: number
  offset_minutes: number
  summary: ReliabilityMetrics
  channels: (ReliabilityMetrics & { channel_id: number; name: string })[]
  daily: (ReliabilityMetrics & { day: string })[]
  recent_failures: {
    created_at: number
    channel_id: number
    model_name: string
    reason: string
    http_status: number
    error_status: number
    attempts: number
    latency_ms: number
  }[]
  local_requests: number
  retry_requests: number
  writer_dropped: number
  writer_failed: number
  writer_queued: number
}
