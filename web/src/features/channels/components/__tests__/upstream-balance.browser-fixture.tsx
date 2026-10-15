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
// Local browser fixture only; never included in the production entry.
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import i18next from 'i18next'
import { useState } from 'react'
import { createRoot } from 'react-dom/client'
import { initReactI18next } from 'react-i18next'

import { Button } from '@/components/ui/button'
import { UpstreamBalanceDialog } from '@/features/channels/components/dialogs/upstream-balance-dialog'
import { useAuthStore } from '@/stores/auth-store'

import '@/styles/index.css'

await i18next
  .use(initReactI18next)
  .init({ lng: 'en', resources: { en: { translation: {} } } })
const params = new URLSearchParams(location.search)
document.documentElement.classList.add(params.get('theme') || 'light')
useAuthStore.getState().auth.setUser({
  id: 1,
  username: 'fixture-admin',
  role: params.has('readonly') ? 10 : 100,
})
const client = new QueryClient({
  defaultOptions: { queries: { retry: false } },
})
export function Fixture() {
  const [open, setOpen] = useState(false)
  return (
    <QueryClientProvider client={client}>
      <main className='p-6'>
        <Button onClick={() => setOpen(true)}>Open quota</Button>
      </main>
      {open && (
        <UpstreamBalanceDialog
          channelId={11}
          channelName='Fixture — same account, no summed quota'
          onClose={() => setOpen(false)}
        />
      )}
    </QueryClientProvider>
  )
}
const container = document.querySelector('#root')
if (!container) throw new Error('Missing browser fixture root')
createRoot(container).render(<Fixture />)
