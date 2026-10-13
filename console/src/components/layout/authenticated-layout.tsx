// App shell: sidebar, header, auth banner and settings. A 401 from any API
// call opens settings with the re-auth banner, as useAuth.js does.
import { useEffect, useState } from 'react'
import { Outlet } from '@tanstack/react-router'
import { TriangleAlert } from 'lucide-react'
import { toast } from 'sonner'
import { SidebarInset, SidebarProvider } from '@/components/ui/sidebar'
import { SettingsDialog } from '@/components/settings-dialog'
import { useObservation } from '@/context/observation'
import { AUTH_REQUIRED_EVENT, isAuthDialogPending, resetAuthDialogShown } from '@/lib/api'
import { setAuthToken } from '@/lib/auth'
import { setLocale, useI18n } from '@/lib/i18n'
import { AppSidebar } from './app-sidebar'
import { Header } from './header'

export function AuthenticatedLayout() {
  const { t } = useI18n()
  const { refresh } = useObservation()
  const [settingsOpen, setSettingsOpen] = useState(false)
  const [authMessage, setAuthMessage] = useState('')

  useEffect(() => {
    const onAuthRequired = () => {
      setAuthMessage(t('auth.tokenExpired'))
      setSettingsOpen(true)
    }
    window.addEventListener(AUTH_REQUIRED_EVENT, onAuthRequired)
    if (isAuthDialogPending()) onAuthRequired()
    return () => window.removeEventListener(AUTH_REQUIRED_EVENT, onAuthRequired)
  }, [t])

  return (
    <SidebarProvider>
      <AppSidebar />
      <SidebarInset>
        <Header onSettings={() => setSettingsOpen(true)} />
        <main className='flex flex-1 flex-col gap-4 p-4'>
          {authMessage && (
            <div
              role='alert'
              className='border-destructive/40 bg-destructive/5 text-destructive flex items-start gap-2 rounded-lg border p-3 text-sm'
            >
              <TriangleAlert className='mt-0.5 size-4 shrink-0' />
              <div>
                <p className='font-medium'>{t('auth.reauthRequired')}</p>
                <p>{authMessage}</p>
              </div>
            </div>
          )}
          <Outlet />
        </main>
      </SidebarInset>
      <SettingsDialog
        open={settingsOpen}
        onOpenChange={setSettingsOpen}
        onLocaleChange={(locale) => {
          setLocale(locale)
          window.location.reload()
        }}
        onSave={(token) => {
          setAuthToken(token)
          setSettingsOpen(false)
          setAuthMessage('')
          resetAuthDialogShown()
          toast.success(t('msg.settingsSaved'))
          void refresh()
        }}
      />
    </SidebarProvider>
  )
}
