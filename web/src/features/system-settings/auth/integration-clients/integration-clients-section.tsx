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
import { KeyRound } from 'lucide-react'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { CopyButton } from '@/components/copy-button'
import { EmptyState } from '@/components/empty-state'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { SecureVerificationDialog } from '@/features/auth/secure-verification'
import { toIntlLocale } from '@/i18n/languages'
import { useAuthStore } from '@/stores/auth-store'

import { SettingsSection } from '../../components/settings-section'
import type { IntegrationAction } from './api'
import { ClientCredentialDialog, CreateClientDialog } from './client-dialogs'
import { useIntegrationClients } from './use-integration-clients'

export function IntegrationClientsSection() {
  const isRoot = useAuthStore((state) => state.auth.user?.role === 100)
  if (!isRoot) return null
  return <IntegrationClientsManager />
}

function IntegrationClientsManager() {
  const { t, i18n } = useTranslation()
  const clients = useIntegrationClients()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const [createOpen, setCreateOpen] = useState(false)
  const [target, setTarget] = useState<Exclude<
    IntegrationAction,
    { kind: 'create' }
  > | null>(null)
  const formatTime = (seconds: number) =>
    new Date(seconds * 1000).toLocaleString(locale)
  const data = clients.list.data

  if (clients.list.isPending) return <LoadingState />
  if (clients.list.isError || !data) {
    return (
      <ErrorState
        title={t('Failed to load integration clients')}
        onRetry={() => void clients.list.refetch()}
      />
    )
  }
  return (
    <SettingsSection title={t('Integration clients')}>
      <Alert>
        <AlertDescription className='space-y-2'>
          <p>
            {t(
              'Only the balance API is available. Clients cannot sign in or manage users.'
            )}
          </p>
          <p>
            {t(
              'Use the same OIDC subject as New API. Never match users by email or name.'
            )}
          </p>
          <p>
            {t(
              'Keep client secrets on the calling server, never in the browser.'
            )}
          </p>
          <code className='block break-all'>
            POST /api/integration/v1/token
          </code>
          <p>
            {t(
              'Use HTTP Basic with the client ID and secret, grant_type=client_credentials and scope=balance:read.'
            )}
          </p>
          <code className='block break-all'>
            GET /api/integration/v1/balance?subject=…
          </code>
          <p>
            {t(
              'Use the returned Bearer token. Tokens expire after five minutes. Include the site base path in both URLs.'
            )}
          </p>
        </AlertDescription>
      </Alert>
      {!data.oidc_configured && (
        <p role='alert'>
          {t('Configure OIDC before creating an integration client.')}
        </p>
      )}
      <div>
        <Button
          onClick={() => setCreateOpen(true)}
          disabled={!data.oidc_configured || clients.pending}
        >
          {t('Create client')}
        </Button>
      </div>
      {data.items.length === 0 ? (
        <EmptyState icon={KeyRound} title={t('No integration clients')} />
      ) : (
        <div className='space-y-3'>
          {data.items.map((client) => {
            const expired = client.expires_at <= Date.now() / 1000
            const identityChanged =
              !data.oidc_configured ||
              client.oidc_issuer !== data.oidc_issuer ||
              client.oidc_client_id !== data.oidc_client_id
            let status = t('Enabled')
            if (!client.enabled) status = t('Disabled')
            else if (expired) status = t('Expired')
            else if (identityChanged) status = t('OIDC configuration changed')
            return (
              <article
                key={client.client_id}
                className='min-w-0 space-y-3 rounded-lg border p-4'
                aria-label={client.name}
              >
                <div className='flex flex-wrap items-center gap-2'>
                  <h4 className='min-w-0 font-medium break-all'>
                    {client.name}
                  </h4>
                  <Badge variant='outline'>{status}</Badge>
                </div>
                <div className='flex min-w-0 items-center gap-2'>
                  <code className='min-w-0 text-xs break-all'>
                    {client.client_id}
                  </code>
                  <CopyButton
                    value={client.client_id}
                    aria-label={t('Copy client ID')}
                  />
                </div>
                <p className='text-muted-foreground text-xs break-all'>
                  {client.oidc_issuer}
                </p>
                <p className='text-muted-foreground text-xs'>
                  {t('Secret expires {{time}}', {
                    time: formatTime(client.expires_at),
                  })}
                  {' · '}
                  {client.last_used_at
                    ? t('Last used {{time}}', {
                        time: formatTime(client.last_used_at),
                      })
                    : t('Never used')}
                </p>
                <div className='flex flex-wrap gap-2'>
                  <Button
                    variant='outline'
                    disabled={clients.pending || identityChanged}
                    onClick={() => setTarget({ kind: 'rotate', client })}
                  >
                    {t('Rotate secret')}
                  </Button>
                  <Button
                    variant='outline'
                    disabled={
                      clients.pending || (identityChanged && !client.enabled)
                    }
                    onClick={() =>
                      setTarget({
                        kind: 'status',
                        client,
                        enabled: !client.enabled,
                      })
                    }
                  >
                    {client.enabled ? t('Disable') : t('Enable')}
                  </Button>
                </div>
              </article>
            )
          })}
        </div>
      )}
      <CreateClientDialog
        open={createOpen}
        hidden={clients.verifying}
        pending={clients.pending}
        onClose={() => setCreateOpen(false)}
        onCreate={(name) => clients.operate({ kind: 'create', name })}
      />
      <ConfirmDialog
        open={target !== null}
        onOpenChange={(open) => {
          if (!open) setTarget(null)
        }}
        title={
          target?.kind === 'rotate'
            ? t('Rotate secret')
            : t('Change client status')
        }
        desc={t(
          'This immediately invalidates all existing access tokens for this client.'
        )}
        destructive
        isLoading={clients.pending}
        handleConfirm={() => {
          if (!target || clients.pending) return
          const action = target
          setTarget(null)
          void clients.operate(action)
        }}
      />
      <SecureVerificationDialog {...clients.verification.dialogProps} />
      {clients.created && (
        <ClientCredentialDialog
          credential={clients.created}
          onClose={clients.clearCreated}
        />
      )}
    </SettingsSection>
  )
}
