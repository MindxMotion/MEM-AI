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
import { api } from '@/lib/api'
import { authRequestOptions, authResult } from '@/lib/secure-verification'

export interface IntegrationClient {
  id: number
  client_id: string
  name: string
  secret_hint: string
  oidc_issuer: string
  oidc_client_id: string
  enabled: boolean
  created_at: number
  expires_at: number
  last_used_at: number
}

export interface IntegrationClientList {
  items: IntegrationClient[]
  oidc_configured: boolean
  oidc_issuer: string
  oidc_client_id: string
}

export type IntegrationAction =
  | { kind: 'create'; name: string }
  | { kind: 'rotate'; client: IntegrationClient }
  | { kind: 'status'; client: IntegrationClient; enabled: boolean }

export interface ClientCredential {
  client_id: string
  client_secret: string
}

export function listIntegrationClients(
  signal: AbortSignal
): Promise<IntegrationClientList> {
  return authResult(
    api.get('/api/integration-clients', { ...authRequestOptions, signal }),
    'Failed to load integration clients'
  )
}
