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
import { zodResolver } from '@hookform/resolvers/zod'
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { z } from 'zod'

import { CopyButton } from '@/components/copy-button'
import { Dialog } from '@/components/dialog'
import { Button } from '@/components/ui/button'
import {
  Form,
  FormControl,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'

import type { ClientCredential } from './api'

const clientSchema = z.object({
  name: z
    .string()
    .trim()
    .min(1, 'Name must be 1 to 64 characters.')
    .max(64, 'Name must be 1 to 64 characters.'),
})

export function CreateClientDialog(props: {
  open: boolean
  hidden: boolean
  pending: boolean
  onClose: () => void
  onCreate: (name: string) => Promise<boolean>
}) {
  const { t } = useTranslation()
  const form = useForm<z.infer<typeof clientSchema>>({
    resolver: zodResolver(clientSchema),
    defaultValues: { name: '' },
  })
  const close = () => {
    if (props.pending) return
    form.reset()
    props.onClose()
  }
  return (
    <Dialog
      open={props.open && !props.hidden}
      onOpenChange={(open) => {
        if (!open) close()
      }}
      title={t('Create client')}
      description={t(
        'Client secrets expire after 90 days. Rotate them before expiry.'
      )}
      contentHeight='auto'
      footer={
        <>
          <Button variant='outline' onClick={close} disabled={props.pending}>
            {t('Cancel')}
          </Button>
          <Button
            type='submit'
            form='integration-client-form'
            disabled={props.pending}
          >
            {t('Create client')}
          </Button>
        </>
      }
    >
      <Form {...form}>
        <form
          id='integration-client-form'
          onSubmit={form.handleSubmit(async (values) => {
            if (await props.onCreate(values.name)) {
              form.reset()
              props.onClose()
            }
          })}
        >
          <FormField
            control={form.control}
            name='name'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Name')}</FormLabel>
                <FormControl>
                  <Input {...field} autoComplete='off' maxLength={64} />
                </FormControl>
                <FormMessage />
              </FormItem>
            )}
          />
        </form>
      </Form>
    </Dialog>
  )
}

export function ClientCredentialDialog(props: {
  credential: ClientCredential
  onClose: () => void
}) {
  const { t } = useTranslation()
  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open) props.onClose()
      }}
      title={t('Client credentials')}
      description={t('Copy this secret now. It will not be shown again.')}
      contentHeight='auto'
      footer={<Button onClick={props.onClose}>{t('Close')}</Button>}
    >
      <div className='space-y-4'>
        <div className='space-y-2'>
          <Label htmlFor='created-client-id'>{t('Client ID')}</Label>
          <div className='flex gap-2'>
            <Input
              id='created-client-id'
              value={props.credential.client_id}
              readOnly
              autoComplete='off'
            />
            <CopyButton
              value={props.credential.client_id}
              aria-label={t('Copy client ID')}
            />
          </div>
        </div>
        <div className='space-y-2'>
          <Label htmlFor='created-client-secret'>{t('Client secret')}</Label>
          <div className='flex gap-2'>
            <Input
              id='created-client-secret'
              value={props.credential.client_secret}
              readOnly
              autoComplete='off'
            />
            <CopyButton
              value={props.credential.client_secret}
              aria-label={t('Copy client secret')}
            />
          </div>
        </div>
      </div>
    </Dialog>
  )
}
