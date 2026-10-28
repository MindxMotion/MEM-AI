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
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useCallback, useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import {
  useSecureVerification,
  type VerificationOperation,
} from '@/features/auth/secure-verification'
import { api } from '@/lib/api'
import { handleServerError } from '@/lib/handle-server-error'
import {
  AuthOperationError,
  authRequestOptions,
  authResult,
} from '@/lib/secure-verification'
import { useAuthStore } from '@/stores/auth-store'

import {
  listIntegrationClients,
  type ClientCredential,
  type IntegrationAction,
  type IntegrationClient,
} from './api'

export function useIntegrationClients() {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const userId = useAuthStore((state) => state.auth.user?.id)
  const sessionId = useAuthStore((state) => state.auth.session?.sid)
  const owner = `${userId}:${sessionId}`
  const verification = useSecureVerification()
  const { requestVerification, cancel } = verification
  const active = useRef<AbortController | null>(null)
  const [pending, setPending] = useState(false)
  const [created, setCreated] = useState<
    (ClientCredential & { owner: string }) | null
  >(null)
  const list = useQuery({
    queryKey: ['integration-clients', userId, sessionId],
    queryFn: ({ signal }) => listIntegrationClients(signal),
    retry: false,
  })

  useEffect(
    () => () => {
      active.current?.abort()
      active.current = null
      cancel()
      setCreated(null)
      setPending(false)
    },
    [owner, cancel]
  )

  const operate = useCallback(
    async (action: IntegrationAction): Promise<boolean> => {
      if (active.current) return false
      const controller = new AbortController()
      active.current = controller
      setPending(true)
      try {
        let operation: VerificationOperation
        let path: string
        let body: { name: string } | { enabled: boolean } | undefined
        if (action.kind === 'create') {
          if (!list.data?.oidc_configured) return false
          operation = {
            scope: 'integration_client.create',
            context: {
              name: action.name,
              issuer: list.data.oidc_issuer,
              oidc_client_id: list.data.oidc_client_id,
            },
          }
          path = '/api/integration-clients'
          body = { name: action.name }
        } else {
          const clientId = action.client.client_id
          path = `/api/integration-clients/${encodeURIComponent(clientId)}`
          if (action.kind === 'rotate') {
            operation = {
              scope: 'integration_client.rotate',
              context: { client_id: clientId },
            }
            path += '/rotate'
          } else {
            operation = {
              scope: 'integration_client.status',
              context: { client_id: clientId, enabled: action.enabled },
            }
            body = { enabled: action.enabled }
          }
        }
        const proof = await requestVerification(operation)
        if (!proof || active.current !== controller) return false
        const options = {
          ...authRequestOptions,
          headers: { 'X-Security-Proof': proof.proof_token },
          singleUseAuthorization: true,
          signal: controller.signal,
        }
        if (action.kind === 'status') {
          await authResult(api.patch(path, body, options))
        } else {
          const result = await authResult<{
            item?: IntegrationClient
            client_id?: string
            client_secret: string
          }>(api.post(path, body, options))
          if (active.current !== controller) return false
          const clientId = result.item?.client_id ?? result.client_id
          if (!clientId || !result.client_secret) {
            throw new AuthOperationError('Please try again later.')
          }
          // Plaintext credentials never enter a Query/Mutation cache or storage.
          setCreated({
            client_id: clientId,
            client_secret: result.client_secret,
            owner,
          })
        }
        if (active.current !== controller) return false
        toast.success(t('Saved successfully'))
        return true
      } catch (error) {
        if (active.current === controller) {
          const failure = AuthOperationError.from(error)
          if (failure.code !== 'AUTH_CANCELLED') handleServerError(failure)
        }
        return false
      } finally {
        if (active.current === controller) {
          active.current = null
          setPending(false)
          void queryClient.invalidateQueries({
            queryKey: ['integration-clients', userId, sessionId],
          })
        }
      }
    },
    [list.data, requestVerification, owner, queryClient, userId, sessionId, t]
  )

  const phase = verification.dialogProps.state.phase
  return {
    list,
    pending,
    operate,
    created: created?.owner === owner ? created : null,
    clearCreated: () => setCreated(null),
    verification,
    verifying: phase !== 'idle' && phase !== 'loading',
  }
}
