import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { RouterProvider, createHashHistory, createRouter } from '@tanstack/react-router'
import { Toaster } from '@/components/ui/sonner'
import { TooltipProvider } from '@/components/ui/tooltip'
import { ObservationProvider } from '@/context/observation'
import { TaskFilterProvider } from '@/features/tasks/filter-context'
import { TaskObservationSync } from '@/features/tasks/sync'
import { I18nProvider } from '@/lib/i18n'
import { routeTree } from './routeTree.gen'
import './styles/index.css'

// Hash history keeps the Vue console's deep links (#/tasks, #/alerts, ...).
const router = createRouter({ routeTree, history: createHashHistory() })

declare module '@tanstack/react-router' {
  interface Register {
    router: typeof router
  }
}

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <I18nProvider>
      <TooltipProvider>
        <ObservationProvider>
          <TaskFilterProvider>
            <TaskObservationSync />
            <RouterProvider router={router} />
          </TaskFilterProvider>
        </ObservationProvider>
      </TooltipProvider>
      <Toaster richColors position='top-right' />
    </I18nProvider>
  </StrictMode>,
)
